package process

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tamuto/xvia/internal/logx"
)

func run(t *testing.T, args ...string) *Child {
	t.Helper()
	c, err := Start(args, logx.Discard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Cleanup(time.Second) })
	return c
}

func wait(t *testing.T, c *Child) int {
	t.Helper()
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("command did not exit")
	}
	return c.ExitCode()
}

func TestExitCode(t *testing.T) {
	if code := wait(t, run(t, "sh", "-c", "exit 3")); code != 3 {
		t.Fatalf("exit code %d", code)
	}
	if code := wait(t, run(t, "true")); code != 0 {
		t.Fatalf("exit code %d", code)
	}
}

func TestKilledBySignal(t *testing.T) {
	if code := wait(t, run(t, "sh", "-c", "kill -TERM $$")); code != 128+15 {
		t.Fatalf("exit code %d", code)
	}
}

func TestCommandNotFound(t *testing.T) {
	_, err := Start([]string{"xvia-definitely-missing-command"}, logx.Discard())
	var se *StartError
	if !errors.As(err, &se) || se.Code != 127 {
		t.Fatalf("got %v", err)
	}
	_, err = Start([]string{"/etc/hostname"}, logx.Discard()) // exists, not executable
	if !errors.As(err, &se) || se.Code != 126 {
		t.Fatalf("got %v", err)
	}
}

func TestInheritsEnvAndDir(t *testing.T) {
	t.Setenv("XVIA_TEST_VALUE", "inherited")
	dir := t.TempDir()
	old, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(old)
	script := `test "$XVIA_TEST_VALUE" = inherited && test "$(pwd -P)" = "$(cd "$1" && pwd -P)"`
	if code := wait(t, run(t, "sh", "-c", script, "sh", dir)); code != 0 {
		t.Fatalf("environment or working directory not inherited (exit %d)", code)
	}
}

func TestForwardsSIGTERM(t *testing.T) {
	c := run(t, "sleep", "30")
	time.Sleep(100 * time.Millisecond)
	// xvia receives SIGTERM (e.g. docker stop) and must pass it on.
	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	if code := wait(t, c); code != 128+15 {
		t.Fatalf("exit code %d", code)
	}
}

func TestLeftoverDescendantsAreTerminated(t *testing.T) {
	// The shell exits immediately, leaving a background sleep behind. As a
	// subreaper we inherit it and must clean it up.
	c := run(t, "sh", "-c", "sleep 60 & echo started")
	wait(t, c)
	left := Descendants(os.Getpid())
	if len(left) == 0 {
		t.Fatal("orphaned grandchild was not reparented to us (subreaper not active?)")
	}
	start := time.Now()
	c.Cleanup(5 * time.Second)
	if time.Since(start) > 4*time.Second {
		t.Fatal("cleanup took too long")
	}
	if rest := Descendants(os.Getpid()); len(rest) != 0 {
		t.Fatalf("processes left after cleanup: %v", rest)
	}
	// No zombies either: every child has been reaped.
	var ws syscall.WaitStatus
	if pid, _ := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil); pid > 0 {
		t.Fatalf("unreaped child %d", pid)
	}
}

func TestDescendantsTree(t *testing.T) {
	c := run(t, "sh", "-c", "sh -c 'sleep 30' & sleep 30")
	var procs []Proc
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if procs = Descendants(os.Getpid()); len(procs) >= 4 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(procs) < 3 {
		t.Fatalf("expected nested descendants, got %v", procs)
	}
	scanned := descendantsByScan(os.Getpid())
	if len(scanned) != len(procs) {
		t.Fatalf("children walk %v and stat scan %v disagree", procs, scanned)
	}
	c.Signal(syscall.SIGKILL)
	wait(t, c)
}

func TestParseStat(t *testing.T) {
	line := "1234 (weird ) name) S 1 1234 1234 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000 100 18446744073709551615"
	p, err := parseStat(1234, []byte(line))
	if err != nil || p.PPid != 1 || p.Start != 987654 {
		t.Fatalf("got %+v %v", p, err)
	}
	if _, err := parseStat(1, []byte("garbage")); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatal("garbage accepted")
	}
}
