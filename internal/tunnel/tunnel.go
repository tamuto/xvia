// Package tunnel copies byte streams between two connections.
package tunnel

import (
	"errors"
	"io"
	"net"
	"sync"
)

type closeWriter interface {
	CloseWrite() error
}

// Pipe copies data in both directions between a and b until both directions
// are finished, then closes both connections.
//
// When one side reaches EOF, the write half of the other side is shut down
// (half-close) so that protocols relying on it keep working. If a copy fails
// with an error other than EOF, both connections are closed immediately so
// that the other direction does not hang. Backpressure is provided by the
// blocking reads and writes of the underlying sockets.
func Pipe(a, b net.Conn) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			a.Close()
			b.Close()
		})
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		copyHalf(b, a, closeBoth)
	}()
	go func() {
		defer wg.Done()
		copyHalf(a, b, closeBoth)
	}()
	wg.Wait()
	closeBoth()
}

func copyHalf(dst, src net.Conn, abort func()) {
	buf := make([]byte, 32*1024)
	_, err := io.CopyBuffer(dst, src, buf)
	if err != nil && !errors.Is(err, net.ErrClosed) {
		abort()
		return
	}
	if cw, ok := dst.(closeWriter); ok {
		if cw.CloseWrite() == nil {
			return
		}
	}
	// The destination cannot be half-closed; closing it is the only way to
	// tell the peer that no more data follows.
	abort()
}
