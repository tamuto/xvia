// Package logx is a minimal leveled logger writing "xvia: ..." lines to stderr.
package logx

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

type Level int

const (
	Debug Level = iota
	Info
	Warn
	Error
)

// ParseLevel accepts debug, info, warn(ing) and error.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return Debug, nil
	case "", "info":
		return Info, nil
	case "warn", "warning":
		return Warn, nil
	case "error":
		return Error, nil
	}
	return Info, fmt.Errorf("invalid log level %q (want debug, info, warn or error)", s)
}

type Logger struct {
	mu         sync.Mutex
	out        io.Writer
	level      Level
	timestamps bool
}

// New returns a logger. Timestamps are useful for the long running agent and
// noise for the client, whose output is interleaved with the wrapped command.
func New(out io.Writer, level Level, timestamps bool) *Logger {
	return &Logger{out: out, level: level, timestamps: timestamps}
}

// Discard returns a logger that drops everything.
func Discard() *Logger { return New(io.Discard, Error+1, false) }

// Stderr returns a logger writing to os.Stderr.
func Stderr(level Level, timestamps bool) *Logger { return New(os.Stderr, level, timestamps) }

func (l *Logger) logf(lv Level, prefix, format string, args ...any) {
	if l == nil || lv < l.level {
		return
	}
	msg := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.timestamps {
		fmt.Fprintf(l.out, "%s xvia: %s%s\n", time.Now().Format("2006-01-02T15:04:05.000"), prefix, msg)
	} else {
		fmt.Fprintf(l.out, "xvia: %s%s\n", prefix, msg)
	}
}

func (l *Logger) Debugf(format string, args ...any) { l.logf(Debug, "debug: ", format, args...) }
func (l *Logger) Infof(format string, args ...any)  { l.logf(Info, "", format, args...) }
func (l *Logger) Warnf(format string, args ...any)  { l.logf(Warn, "warning: ", format, args...) }
func (l *Logger) Errorf(format string, args ...any) { l.logf(Error, "", format, args...) }
