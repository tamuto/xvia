// Package process runs the wrapped command: it forwards signals, reaps every
// descendant (xvia is a child subreaper), propagates the exit code and
// terminates leftover descendants when the command exits.
package process

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/tamuto/xvia/internal/logx"
)

const prSetChildSubreaper = 36

// Child is a running command.
type Child struct {
	Pid  int
	log  *logx.Logger
	tty  bool // child shares our process group and terminal
	done chan struct{}
	code int

	sigs chan os.Signal
	stop chan struct{}
	once sync.Once
}

// reaper is the process-wide child reaper. As a subreaper we must collect
// orphans with wait4(-1), so a single loop owns all waiting and hands exit
// statuses to the Child they belong to.
var reaper struct {
	once     sync.Once
	mu       sync.Mutex // held while reaping and while a child is being registered
	children map[int]*Child
}

func startReaper() {
	reaper.once.Do(func() {
		reaper.children = map[int]*Child{}
		chld := make(chan os.Signal, 1)
		signal.Notify(chld, syscall.SIGCHLD)
		go func() {
			tick := time.NewTicker(time.Second) // safety net for coalesced SIGCHLD
			for {
				reapAll()
				select {
				case <-chld:
				case <-tick.C:
				}
			}
		}()
	})
}

func reapAll() {
	reaper.mu.Lock()
	defer reaper.mu.Unlock()
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if err == syscall.EINTR {
			continue
		}
		if pid <= 0 || err != nil {
			return
		}
		c := reaper.children[pid]
		if c == nil {
			continue // an orphaned descendant
		}
		delete(reaper.children, pid)
		switch {
		case ws.Exited():
			c.code = ws.ExitStatus()
		case ws.Signaled():
			c.code = 128 + int(ws.Signal())
		default:
			c.code = 1
		}
		close(c.done)
	}
}

// StartError carries the exit code to use when the command cannot be started
// (127: not found, 126: not executable), following shell conventions.
type StartError struct {
	Code int
	Err  error
}

func (e *StartError) Error() string { return e.Err.Error() }
func (e *StartError) Unwrap() error { return e.Err }

// Start runs args with the current working directory, environment and
// standard streams.
//
// When stdin is the controlling terminal and xvia is in the foreground, the
// child stays in xvia's process group so that it can read from the terminal
// and receives Ctrl+C / Ctrl+\ directly from the kernel; xvia then only
// forwards signals sent to xvia itself (SIGTERM, SIGHUP). Otherwise the child
// gets its own process group and every signal is forwarded to that group.
func Start(args []string, log *logx.Logger) (*Child, error) {
	if len(args) == 0 {
		return nil, &StartError{Code: 2, Err: errors.New("no command given")}
	}
	// Orphaned grandchildren are reparented to us instead of init, which keeps
	// them visible to port detection and lets us reap and terminate them.
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
		log.Debugf("cannot become child subreaper: %v", errno)
	}

	c := &Child{
		log:  log,
		tty:  foregroundTerminal(),
		done: make(chan struct{}),
		sigs: make(chan os.Signal, 8),
		stop: make(chan struct{}),
	}
	startReaper()
	signal.Notify(c.sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Pdeathsig terminates the command if xvia itself is killed. The kernel
	// tracks the parent *thread*, so the thread that forks stays locked and
	// alive until the command exits.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: !c.tty, Pdeathsig: syscall.SIGTERM}
	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		// Hold the reaper lock so the exit status cannot be collected
		// before the child is registered.
		reaper.mu.Lock()
		err := cmd.Start()
		if err == nil {
			c.Pid = cmd.Process.Pid
			reaper.children[c.Pid] = c
		}
		reaper.mu.Unlock()
		started <- err
		if err == nil {
			<-c.done
		}
	}()
	if err := <-started; err != nil {
		signal.Stop(c.sigs)
		code := 126
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			code = 127
		}
		return nil, &StartError{Code: code, Err: fmt.Errorf("cannot run %s: %w", args[0], err)}
	}
	// cmd.Wait is never called: the reaper collects the exit status together
	// with all reparented orphans.
	go c.forwardSignals()
	return c, nil
}

// Done is closed when the command has exited.
func (c *Child) Done() <-chan struct{} { return c.done }

// ExitCode is valid after Done is closed: the exit status, or 128+signal.
func (c *Child) ExitCode() int { return c.code }

// Signal sends sig to the command (its process group when it has its own).
func (c *Child) Signal(sig syscall.Signal) {
	if c.tty {
		syscall.Kill(c.Pid, sig)
	} else {
		syscall.Kill(-c.Pid, sig)
	}
}

func (c *Child) forwardSignals() {
	for {
		select {
		case <-c.stop:
			return
		case s := <-c.sigs:
			sig := s.(syscall.Signal)
			if c.tty && (sig == syscall.SIGINT || sig == syscall.SIGQUIT) {
				// Sent by the terminal to the whole foreground group, which
				// includes the child; forwarding would deliver it twice.
				continue
			}
			select {
			case <-c.done:
				// The command is gone; leftovers are handled by Cleanup.
			default:
				c.log.Debugf("forwarding %v to the command", sig)
				c.Signal(sig)
			}
		}
	}
}

// Cleanup terminates descendants that outlived the command (SIGTERM, then
// SIGKILL after grace), reaps them and stops signal handling. Call it after
// Done is closed.
func (c *Child) Cleanup(grace time.Duration) {
	c.once.Do(func() {
		self := os.Getpid()
		left := Descendants(self)
		if len(left) > 0 {
			c.log.Debugf("terminating %d leftover process(es)", len(left))
			signalAll(left, syscall.SIGTERM)
			deadline := time.Now().Add(grace)
			for time.Now().Before(deadline) && len(Descendants(self)) > 0 {
				time.Sleep(50 * time.Millisecond)
			}
			if left = Descendants(self); len(left) > 0 {
				signalAll(left, syscall.SIGKILL)
				deadline = time.Now().Add(time.Second)
				for time.Now().Before(deadline) && len(Descendants(self)) > 0 {
					time.Sleep(20 * time.Millisecond)
				}
			}
		}
		close(c.stop)
		signal.Stop(c.sigs)
		reapAll()
	})
}

// signalAll signals processes that are still the same process (PID reuse check).
func signalAll(procs []Proc, sig syscall.Signal) {
	for _, p := range procs {
		if cur, err := ReadStat(p.Pid); err == nil && cur.Start == p.Start {
			syscall.Kill(p.Pid, sig)
		}
	}
}

// foregroundTerminal reports whether stdin is a terminal whose foreground
// process group is ours.
func foregroundTerminal() bool {
	var pgrp int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, 0, syscall.TIOCGPGRP, uintptr(unsafe.Pointer(&pgrp)))
	return errno == 0 && int(pgrp) == syscall.Getpgrp()
}
