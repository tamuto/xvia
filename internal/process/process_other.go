//go:build !linux

package process

import (
	"errors"
	"syscall"
	"time"

	"github.com/tamuto/xvia/internal/logx"
)

// Proc identifies a process.
type Proc struct {
	Pid   int
	PPid  int
	Start uint64
}

// Child is a running command.
type Child struct {
	Pid  int
	done chan struct{}
}

type StartError struct {
	Code int
	Err  error
}

func (e *StartError) Error() string { return e.Err.Error() }
func (e *StartError) Unwrap() error { return e.Err }

// Start is only implemented on Linux.
func Start(args []string, log *logx.Logger) (*Child, error) {
	return nil, &StartError{Code: 1, Err: errors.New("running commands is only supported on Linux")}
}

func (c *Child) Done() <-chan struct{}       { return c.done }
func (c *Child) ExitCode() int               { return 1 }
func (c *Child) Signal(sig syscall.Signal)   {}
func (c *Child) Cleanup(grace time.Duration) {}
func Descendants(root int) []Proc            { return nil }
func ReadStat(pid int) (Proc, error)         { return Proc{}, errors.New("unsupported") }
