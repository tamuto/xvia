package agent

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tamuto/xvia/internal/config"
	"github.com/tamuto/xvia/internal/protocol"
)

// testBind is a loopback address other than 127.0.0.1 so that the agent can
// listen on the same port number as test servers bound to 127.0.0.1.
var testBind = net.IPv4(127, 0, 0, 2)

func startAgent(t *testing.T, mod func(*Config)) *Agent {
	t.Helper()
	cfg := Config{
		SocketPath:    filepath.Join(t.TempDir(), "xvia", "agent.sock"),
		Bind:          testBind,
		AllowPorts:    config.PortRange{Min: 1024, Max: 65535},
		ClientTimeout: 5 * time.Second,
	}
	if mod != nil {
		mod(&cfg)
	}
	a, err := Listen(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.Serve(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("agent did not stop")
		}
	})
	return a
}

type rawClient struct {
	t *testing.T
	c net.Conn
}

func dial(t *testing.T, a *Agent) *rawClient {
	t.Helper()
	c, err := net.Dial("unix", a.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &rawClient{t, c}
}

func (r *rawClient) send(m *protocol.Message) {
	r.t.Helper()
	if err := protocol.Write(r.c, m); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rawClient) recv() *protocol.Message {
	r.t.Helper()
	r.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	m, err := protocol.Read(r.c)
	if err != nil {
		r.t.Fatal(err)
	}
	return m
}

func hello(t *testing.T, a *Agent) *rawClient {
	t.Helper()
	r := dial(t, a)
	r.send(&protocol.Message{Type: protocol.TypeHello, Version: protocol.Version})
	if w := r.recv(); w.Type != protocol.TypeWelcome || w.Error != "" {
		t.Fatalf("welcome: %+v", w)
	}
	return r
}

func (r *rawClient) register(port int) *protocol.Message {
	r.t.Helper()
	r.send(&protocol.Message{Type: protocol.TypeRegister, Port: port})
	m := r.recv()
	if m.Type != protocol.TypeRegistered || m.Port != port {
		r.t.Fatalf("unexpected reply %+v", m)
	}
	return m
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(testBind.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func hostAddr(port int) string { return net.JoinHostPort(testBind.String(), strconv.Itoa(port)) }

func listening(port int) bool {
	c, err := net.DialTimeout("tcp", hostAddr(port), time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSocketPermissionsAndCleanup(t *testing.T) {
	a := startAgent(t, nil)
	st, err := os.Stat(a.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSocket == 0 || st.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v", st.Mode())
	}
	dst, _ := os.Stat(filepath.Dir(a.SocketPath()))
	if dst.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", dst.Mode())
	}
	a.Close()
	if _, err := os.Stat(a.SocketPath()); !os.IsNotExist(err) {
		t.Fatal("socket file not removed on close")
	}
}

func TestSecondAgentRefusedAndStaleSocketReplaced(t *testing.T) {
	a := startAgent(t, nil)
	if _, err := Listen(Config{SocketPath: a.SocketPath()}); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("second agent: %v", err)
	}

	// A socket file left behind by a crashed agent is replaced.
	path := filepath.Join(t.TempDir(), "stale.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
	b, err := Listen(Config{SocketPath: path})
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	b.Close()
}

func TestConnectDisconnect(t *testing.T) {
	a := startAgent(t, nil)
	for i := 0; i < 3; i++ {
		r := hello(t, a)
		r.send(&protocol.Message{Type: protocol.TypePing})
		if m := r.recv(); m.Type != protocol.TypePong {
			t.Fatalf("got %+v", m)
		}
		r.c.Close()
	}
	waitFor(t, "sessions to be released", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return len(a.sessions) == 0
	})
}

func TestVersionMismatch(t *testing.T) {
	a := startAgent(t, nil)
	r := dial(t, a)
	r.send(&protocol.Message{Type: protocol.TypeHello, Version: protocol.Version + 1})
	w := r.recv()
	if w.Type != protocol.TypeWelcome || !strings.Contains(w.Error, "version mismatch") || w.Version != protocol.Version {
		t.Fatalf("welcome: %+v", w)
	}
}

func TestMalformedInputDoesNotCrash(t *testing.T) {
	a := startAgent(t, nil)
	for _, junk := range [][]byte{
		{0xff, 0xff, 0xff, 0xff},
		{0, 0, 0, 3, 'a', 'b', 'c'},
		[]byte("GET / HTTP/1.1\r\n\r\n"),
	} {
		r := dial(t, a)
		r.c.Write(junk)
		r.c.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 1)
		if _, err := r.c.Read(buf); err == nil {
			t.Fatal("expected connection to be closed")
		}
	}
	// Garbage after a successful hello closes only that session.
	r := hello(t, a)
	r.c.Write([]byte{0, 0, 0, 2, '{', '}'})
	// Unknown first message.
	u := dial(t, a)
	u.send(&protocol.Message{Type: "bogus"})
	if m := u.recv(); m.Type != protocol.TypeError {
		t.Fatalf("got %+v", m)
	}
	// Attach with an unknown stream is rejected.
	at := dial(t, a)
	at.send(&protocol.Message{Type: protocol.TypeAttach, Version: protocol.Version, Stream: 99, Token: "x"})
	at.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := at.c.Read(make([]byte, 1)); err == nil {
		t.Fatal("attach to unknown stream accepted")
	}
	// The agent still works.
	ok := hello(t, a)
	ok.send(&protocol.Message{Type: protocol.TypePing})
	if m := ok.recv(); m.Type != protocol.TypePong {
		t.Fatal("agent unusable after malformed input")
	}
}

func TestRegisterUnregister(t *testing.T) {
	a := startAgent(t, nil)
	r := hello(t, a)
	port := freePort(t)
	m := r.register(port)
	if m.Error != "" || m.Address != hostAddr(port) {
		t.Fatalf("register: %+v", m)
	}
	if !listening(port) {
		t.Fatal("port not listening after register")
	}
	if m := r.recv(); m.Type != protocol.TypeOpen { // produced by the probe above
		t.Fatalf("expected open, got %+v", m)
	}
	// Registering again is idempotent.
	if m := r.register(port); m.Error != "" {
		t.Fatalf("re-register: %+v", m)
	}
	r.send(&protocol.Message{Type: protocol.TypeUnregister, Port: port})
	waitFor(t, "listener to close", func() bool { return !listening(port) })
	if len(a.Ports()) != 0 {
		t.Fatalf("ports left: %v", a.Ports())
	}
}

func TestRegisterValidation(t *testing.T) {
	a := startAgent(t, func(c *Config) {
		c.AllowPorts = config.PortRange{Min: 20000, Max: 20010}
		c.MaxPortsPerClient = 1
	})
	r := hello(t, a)
	for _, p := range []int{0, -1, 70000, 19999, 20011} {
		if m := r.register(p); m.Error == "" {
			t.Errorf("port %d accepted", p)
		}
	}
	if m := r.register(20000); m.Error != "" && !strings.Contains(m.Error, "in use") {
		t.Fatalf("20000: %+v", m)
	}
	if m := r.register(20001); !strings.Contains(m.Error, "too many ports") {
		t.Fatalf("limit not enforced: %+v", m)
	}
}

func TestPortConflict(t *testing.T) {
	a := startAgent(t, nil)
	port := freePort(t)
	first := hello(t, a)
	if m := first.register(port); m.Error != "" {
		t.Fatal(m.Error)
	}
	second := hello(t, a)
	m := second.register(port)
	if m.Error != "port "+strconv.Itoa(port)+" is already in use" {
		t.Fatalf("conflict error: %q", m.Error)
	}
	// The second session stays usable.
	second.send(&protocol.Message{Type: protocol.TypePing})
	if m := second.recv(); m.Type != protocol.TypePong {
		t.Fatal("second session closed after conflict")
	}
	// Once the first client leaves, the second may take the port.
	first.c.Close()
	waitFor(t, "port release", func() bool { return len(a.Ports()) == 0 })
	if m := second.register(port); m.Error != "" {
		t.Fatal(m.Error)
	}
}

func TestPortUsedByHostProcess(t *testing.T) {
	a := startAgent(t, nil)
	ln, err := net.Listen("tcp", net.JoinHostPort(testBind.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	if m := hello(t, a).register(port); !strings.Contains(m.Error, "already in use") {
		t.Fatalf("got %+v", m)
	}
}

func TestDisconnectReleasesListeners(t *testing.T) {
	a := startAgent(t, nil)
	r := hello(t, a)
	p1, p2 := freePort(t), freePort(t)
	r.register(p1)
	r.register(p2)
	if !listening(p1) || !listening(p2) {
		t.Fatal("not listening")
	}
	r.c.Close() // simulates a crashed client
	waitFor(t, "listeners to close", func() bool { return !listening(p1) && !listening(p2) })
}

func TestOpenTimeoutAndOpenFailed(t *testing.T) {
	a := startAgent(t, func(c *Config) { c.OpenTimeout = 200 * time.Millisecond })
	r := hello(t, a)
	port := freePort(t)
	r.register(port)

	// Client never attaches: the TCP connection is closed after OpenTimeout.
	c1, err := net.Dial("tcp", hostAddr(port))
	if err != nil {
		t.Fatal(err)
	}
	if m := r.recv(); m.Type != protocol.TypeOpen || m.Port != port || m.Token == "" {
		t.Fatalf("open: %+v", m)
	}
	c1.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c1.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Fatalf("expected close, got %v", err)
	}

	// Client reports failure: the TCP connection is closed right away.
	c2, err := net.Dial("tcp", hostAddr(port))
	if err != nil {
		t.Fatal(err)
	}
	m := r.recv()
	r.send(&protocol.Message{Type: protocol.TypeOpenFailed, Stream: m.Stream, Error: "refused"})
	c2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c2.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Fatalf("expected close, got %v", err)
	}
	waitFor(t, "slots to be released", func() bool { return len(a.slots) == 0 })
}

func TestAttachRequiresToken(t *testing.T) {
	a := startAgent(t, nil)
	r := hello(t, a)
	port := freePort(t)
	r.register(port)
	tcp, err := net.Dial("tcp", hostAddr(port))
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	m := r.recv()

	bad := dial(t, a)
	bad.send(&protocol.Message{Type: protocol.TypeAttach, Version: protocol.Version, Stream: m.Stream, Token: "wrong"})
	bad.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := bad.c.Read(make([]byte, 1)); err == nil {
		t.Fatal("attach with wrong token accepted")
	}

	good := dial(t, a)
	good.send(&protocol.Message{Type: protocol.TypeAttach, Version: protocol.Version, Stream: m.Stream, Token: m.Token})
	tcp.Write([]byte("ping"))
	buf := make([]byte, 4)
	good.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := good.c.Read(buf); err != nil || string(buf) != "ping" {
		t.Fatalf("data not relayed: %q %v", buf, err)
	}
}

func TestConnectionLimit(t *testing.T) {
	a := startAgent(t, func(c *Config) { c.MaxConns = 1 })
	r := hello(t, a)
	port := freePort(t)
	r.register(port)
	c1, _ := net.Dial("tcp", hostAddr(port))
	defer c1.Close()
	r.recv()
	c2, _ := net.Dial("tcp", hostAddr(port))
	defer c2.Close()
	c2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c2.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Fatalf("second connection should be rejected, got %v", err)
	}
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}
