package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/tamuto/xvia/internal/agent"
	"github.com/tamuto/xvia/internal/config"
	"github.com/tamuto/xvia/internal/protocol"
)

// The agent binds 127.0.0.2 so it can use the same port numbers as the
// "container" servers, which bind 127.0.0.1 in these single-host tests.
var testBind = net.IPv4(127, 0, 0, 2)

type harness struct {
	t      *testing.T
	socket string
	agent  *agent.Agent
	stop   func()
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, socket: filepath.Join(t.TempDir(), "agent.sock")}
	h.startAgent()
	t.Cleanup(func() { h.stopAgent() })
	return h
}

func (h *harness) startAgent() {
	h.t.Helper()
	a, err := agent.Listen(agent.Config{
		SocketPath: h.socket,
		Bind:       testBind,
		AllowPorts: config.PortRange{Min: 1024, Max: 65535},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.Serve(ctx)
		close(done)
	}()
	h.agent = a
	h.stop = func() {
		cancel()
		<-done
	}
}

func (h *harness) stopAgent() {
	if h.stop != nil {
		h.stop()
		h.stop = nil
	}
}

func (h *harness) forwarder() (*Forwarder, context.CancelFunc) {
	h.t.Helper()
	f := New(Config{SocketPath: h.socket, MinBackoff: 20 * time.Millisecond, MaxBackoff: 200 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		f.Run(ctx)
		close(done)
	}()
	stop := func() {
		cancel()
		<-done
	}
	h.t.Cleanup(stop)
	return f, stop
}

// localListener listens on 127.0.0.1 ("inside the container") using a port
// that is also free on the agent's bind address.
func localListener(t *testing.T) (net.Listener, int) {
	t.Helper()
	for i := 0; i < 20; i++ {
		probe, err := net.Listen("tcp", net.JoinHostPort(testBind.String(), "0"))
		if err != nil {
			t.Fatal(err)
		}
		port := probe.Addr().(*net.TCPAddr).Port
		probe.Close()
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue
		}
		t.Cleanup(func() { ln.Close() })
		return ln, port
	}
	t.Fatal("no free port")
	return nil, 0
}

func localServer(t *testing.T, handler func(net.Conn)) int {
	t.Helper()
	ln, port := localListener(t)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handler(c)
		}
	}()
	return port
}

func echo(c net.Conn) {
	io.Copy(c, c)
	c.(*net.TCPConn).CloseWrite()
	c.Close()
}

func hostAddr(port int) string { return net.JoinHostPort(testBind.String(), strconv.Itoa(port)) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func hostListening(port int) bool {
	c, err := net.DialTimeout("tcp", hostAddr(port), time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func TestHTTPThroughTunnel(t *testing.T) {
	h := newHarness(t)
	ln, port := localListener(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello %s", r.URL.Path)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	f, _ := h.forwarder()
	f.Set(port, LocalAddrs(port))
	waitFor(t, "registration", func() bool { return slices.Contains(f.Active(), port) })

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	for i := 0; i < 5; i++ {
		resp, err := client.Get(fmt.Sprintf("http://%s/via%d", hostAddr(port), i))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != fmt.Sprintf("hello /via%d", i) {
			t.Fatalf("body %q", body)
		}
	}
}

func TestConcurrentConnectionsAndHalfClose(t *testing.T) {
	h := newHarness(t)
	port := localServer(t, echo)
	f, _ := h.forwarder()
	f.Set(port, LocalAddrs(port))
	waitFor(t, "registration", func() bool { return slices.Contains(f.Active(), port) })

	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := make([]byte, 256<<10)
			rand.Read(payload)
			c, err := net.Dial("tcp", hostAddr(port))
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			go func() {
				c.Write(payload)
				c.(*net.TCPConn).CloseWrite() // half-close: echo must still answer
			}()
			c.SetReadDeadline(time.Now().Add(20 * time.Second))
			got, err := io.ReadAll(c)
			if err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(got, payload) {
				errs <- fmt.Errorf("payload mismatch: got %d bytes", len(got))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestInteractiveStream(t *testing.T) {
	// A long lived, line oriented exchange in both directions, like a
	// WebSocket or HMR connection.
	h := newHarness(t)
	port := localServer(t, func(c net.Conn) {
		defer c.Close()
		sc := bufio.NewScanner(c)
		for sc.Scan() {
			fmt.Fprintf(c, "re:%s\n", sc.Text())
		}
	})
	f, _ := h.forwarder()
	f.Set(port, LocalAddrs(port))
	waitFor(t, "registration", func() bool { return slices.Contains(f.Active(), port) })

	c, err := net.Dial("tcp", hostAddr(port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := bufio.NewReader(c)
	for i := 0; i < 50; i++ {
		fmt.Fprintf(c, "msg%d\n", i)
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := r.ReadString('\n')
		if err != nil || line != fmt.Sprintf("re:msg%d\n", i) {
			t.Fatalf("line %q err %v", line, err)
		}
	}
}

func TestMultiplePortsAndRemove(t *testing.T) {
	h := newHarness(t)
	p1 := localServer(t, echo)
	p2 := localServer(t, echo)
	f, _ := h.forwarder()
	f.Set(p1, LocalAddrs(p1))
	f.Set(p2, LocalAddrs(p2))
	waitFor(t, "registration", func() bool { return len(f.Active()) == 2 })
	for _, p := range []int{p1, p2} {
		roundTrip(t, p)
	}
	f.Remove(p1)
	waitFor(t, "removal", func() bool { return !hostListening(p1) })
	roundTrip(t, p2)
}

func roundTrip(t *testing.T, port int) {
	t.Helper()
	c, err := net.Dial("tcp", hostAddr(port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("xyz"))
	buf := make([]byte, 3)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "xyz" {
		t.Fatalf("round trip on %d: %q %v", port, buf, err)
	}
}

func TestUnreachableLocalPortClosesConnection(t *testing.T) {
	h := newHarness(t)
	port := localServer(t, echo)
	f, _ := h.forwarder()
	f.Set(port, []string{"127.0.0.1:1"}) // nothing listens there
	waitFor(t, "registration", func() bool { return slices.Contains(f.Active(), port) })
	c, err := net.Dial("tcp", hostAddr(port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected the connection to be closed")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("connection was left hanging")
	}
}

func TestClientStopReleasesPorts(t *testing.T) {
	h := newHarness(t)
	port := localServer(t, echo)
	f, stop := h.forwarder()
	f.Set(port, LocalAddrs(port))
	waitFor(t, "registration", func() bool { return hostListening(port) })
	stop()
	waitFor(t, "release", func() bool { return !hostListening(port) && len(h.agent.Ports()) == 0 })
}

func TestTwoClientsConflict(t *testing.T) {
	h := newHarness(t)
	port := localServer(t, echo)
	f1, stop1 := h.forwarder()
	f1.Set(port, LocalAddrs(port))
	waitFor(t, "first registration", func() bool { return slices.Contains(f1.Active(), port) })
	f2, _ := h.forwarder()
	f2.Set(port, LocalAddrs(port))
	time.Sleep(300 * time.Millisecond)
	if len(f2.Active()) != 0 {
		t.Fatal("conflicting registration accepted")
	}
	roundTrip(t, port)
	// When the owner goes away, the port is free again for others.
	stop1()
	waitFor(t, "release", func() bool { return !hostListening(port) })
	f2.Remove(port)
	f2.Set(port, LocalAddrs(port))
	waitFor(t, "second registration", func() bool { return slices.Contains(f2.Active(), port) })
	roundTrip(t, port)
}

func TestReconnectAfterAgentRestart(t *testing.T) {
	h := newHarness(t)
	port := localServer(t, echo)
	f, _ := h.forwarder()
	f.Set(port, LocalAddrs(port))
	waitFor(t, "registration", func() bool { return slices.Contains(f.Active(), port) })
	roundTrip(t, port)

	h.stopAgent()
	waitFor(t, "listener gone", func() bool { return !hostListening(port) })
	waitFor(t, "client noticed", func() bool { return len(f.Active()) == 0 })
	h.startAgent()
	waitFor(t, "re-registration", func() bool { return slices.Contains(f.Active(), port) })
	roundTrip(t, port)
}

func TestAgentMissingThenStarted(t *testing.T) {
	h := newHarness(t)
	h.stopAgent()
	port := localServer(t, echo)
	f, _ := h.forwarder()
	f.Set(port, LocalAddrs(port))
	time.Sleep(100 * time.Millisecond)
	h.startAgent()
	waitFor(t, "registration", func() bool { return slices.Contains(f.Active(), port) })
	roundTrip(t, port)
}

func TestIncompatibleAgent(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "fake.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		// Pretend to be an agent speaking protocol version 99.
		protocol.Write(c, &protocol.Message{Type: protocol.TypeWelcome, Version: 99})
		time.Sleep(time.Second)
	}()
	f := New(Config{SocketPath: socket})
	err = f.Run(context.Background())
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("got %v", err)
	}
}
