// Package client implements the container side of xvia: it keeps a control
// session with the host agent, registers ports and relays forwarded
// connections to servers inside the container.
package client

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/tamuto/xvia/internal/logx"
	"github.com/tamuto/xvia/internal/protocol"
	"github.com/tamuto/xvia/internal/tunnel"
)

// Config configures a Forwarder. Zero values are replaced by defaults.
type Config struct {
	SocketPath string
	Software   string
	Log        *logx.Logger

	PingInterval time.Duration // keepalive interval
	Timeout      time.Duration // agent considered dead after this long without data
	MinBackoff   time.Duration // first reconnect delay
	MaxBackoff   time.Duration // reconnect delay cap
	DialTimeout  time.Duration // connecting to the agent and to local servers
	MaxStreams   int           // concurrent forwarded connections
}

func (c *Config) setDefaults() {
	if c.Log == nil {
		c.Log = logx.Discard()
	}
	defDur := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	defDur(&c.PingInterval, 15*time.Second)
	defDur(&c.Timeout, 45*time.Second)
	defDur(&c.MinBackoff, 250*time.Millisecond)
	defDur(&c.MaxBackoff, 10*time.Second)
	defDur(&c.DialTimeout, 5*time.Second)
	if c.MaxStreams <= 0 {
		c.MaxStreams = 1024
	}
}

// ErrIncompatible is returned by Run when the agent speaks another protocol version.
var ErrIncompatible = errors.New("incompatible agent")

// Forwarder maintains the connection to the agent and the set of forwarded ports.
type Forwarder struct {
	cfg   Config
	log   *logx.Logger
	slots chan struct{}

	// mu guards the fields below and serializes control writes, which keeps
	// register/unregister messages in the same order as Set/Remove calls.
	mu      sync.Mutex
	targets map[int][]string // port -> local dial addresses
	active  map[int]bool     // port -> agent confirmed the registration
	conn    *net.UnixConn    // nil while disconnected
}

// New returns a Forwarder. Call Run to connect.
func New(cfg Config) *Forwarder {
	cfg.setDefaults()
	return &Forwarder{
		cfg:     cfg,
		log:     cfg.Log,
		slots:   make(chan struct{}, cfg.MaxStreams),
		targets: make(map[int][]string),
		active:  make(map[int]bool),
	}
}

// LocalAddrs returns the addresses tried when connecting to a port inside
// the container whose listen address is unknown.
func LocalAddrs(port int) []string {
	p := strconv.Itoa(port)
	return []string{net.JoinHostPort("127.0.0.1", p), net.JoinHostPort("::1", p)}
}

// Set starts forwarding port, connecting to addrs (tried in order) inside the
// container. Calling Set again updates the addresses.
func (f *Forwarder) Set(port int, addrs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, known := f.targets[port]
	f.targets[port] = addrs
	if !known && f.conn != nil {
		f.writeLocked(&protocol.Message{Type: protocol.TypeRegister, Port: port})
	}
}

// Remove stops forwarding port.
func (f *Forwarder) Remove(port int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, known := f.targets[port]; !known {
		return
	}
	delete(f.targets, port)
	if f.active[port] {
		f.log.Infof("port %d is no longer forwarded", port)
	}
	delete(f.active, port)
	if f.conn != nil {
		f.writeLocked(&protocol.Message{Type: protocol.TypeUnregister, Port: port})
	}
}

// Active returns the ports the agent currently forwards for us.
func (f *Forwarder) Active() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ports []int
	for p, ok := range f.active {
		if ok {
			ports = append(ports, p)
		}
	}
	sort.Ints(ports)
	return ports
}

func (f *Forwarder) writeLocked(m *protocol.Message) error {
	c := f.conn
	c.SetWriteDeadline(time.Now().Add(f.cfg.DialTimeout))
	err := protocol.Write(c, m)
	if err != nil {
		c.Close() // the read loop notices and reconnects
	}
	return err
}

func (f *Forwarder) send(c *net.UnixConn, m *protocol.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conn == c {
		f.writeLocked(m)
	}
}

// Run connects to the agent and keeps reconnecting with exponential backoff
// until ctx is cancelled. It returns ErrIncompatible (wrapped) if the agent
// cannot be used at all.
func (f *Forwarder) Run(ctx context.Context) error {
	backoff := f.cfg.MinBackoff
	reported := false // whether the current outage was already reported
	everConnected := false
	for ctx.Err() == nil {
		connected, err := f.session(ctx)
		if errors.Is(err, ErrIncompatible) {
			f.log.Errorf("%v", err)
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if connected {
			everConnected = true
			backoff = f.cfg.MinBackoff
			reported = true
			f.log.Warnf("lost connection to agent (%v); reconnecting", err)
		} else if !reported {
			reported = true
			f.log.Warnf("%s; retrying in the background", describeDialError(f.cfg.SocketPath, err, everConnected))
		} else {
			f.log.Debugf("reconnect failed: %v", err)
		}
		// Full jitter keeps several clients from reconnecting in lockstep.
		delay := backoff/2 + rand.N(backoff/2+1)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		backoff = min(backoff*2, f.cfg.MaxBackoff)
	}
	return nil
}

func describeDialError(path string, err error, before bool) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Sprintf("agent socket %s not found; start `xvia agent` on the host and mount its socket directory into the container", path)
	case errors.Is(err, os.ErrPermission):
		return fmt.Sprintf("permission denied on agent socket %s; the container user must match the agent's user (or be root)", path)
	case before:
		return fmt.Sprintf("cannot reconnect to agent at %s: %v", path, err)
	}
	return fmt.Sprintf("cannot connect to agent at %s: %v", path, err)
}

// session runs one control connection. connected reports whether the
// handshake succeeded.
func (f *Forwarder) session(ctx context.Context) (connected bool, err error) {
	d := net.Dialer{Timeout: f.cfg.DialTimeout}
	nc, err := d.DialContext(ctx, "unix", f.cfg.SocketPath)
	if err != nil {
		return false, err
	}
	c := nc.(*net.UnixConn)
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()

	c.SetDeadline(time.Now().Add(f.cfg.DialTimeout))
	if err := protocol.Write(c, &protocol.Message{Type: protocol.TypeHello, Version: protocol.Version, Software: f.cfg.Software}); err != nil {
		return false, err
	}
	w, err := protocol.Read(c)
	if err != nil {
		return false, fmt.Errorf("handshake: %w", err)
	}
	if w.Type != protocol.TypeWelcome {
		return false, fmt.Errorf("handshake: unexpected %q", w.Type)
	}
	if err := protocol.CheckVersion(w.Version); err != nil {
		return false, fmt.Errorf("%w: %v (agent %s, client %s)", ErrIncompatible, err, w.Software, f.cfg.Software)
	}
	if w.Error != "" {
		return false, fmt.Errorf("agent refused connection: %s", w.Error)
	}
	c.SetDeadline(time.Time{})
	f.log.Infof("connected to agent at %s", f.cfg.SocketPath)

	f.mu.Lock()
	f.conn = c
	ports := make([]int, 0, len(f.targets))
	for p := range f.targets {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	for _, p := range ports {
		if f.writeLocked(&protocol.Message{Type: protocol.TypeRegister, Port: p}) != nil {
			break
		}
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.conn = nil
		f.active = make(map[int]bool)
		f.mu.Unlock()
	}()

	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(f.cfg.PingInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				f.send(c, &protocol.Message{Type: protocol.TypePing})
			}
		}
	}()

	for {
		c.SetReadDeadline(time.Now().Add(f.cfg.Timeout))
		m, err := protocol.Read(c)
		if err != nil {
			return true, err
		}
		switch m.Type {
		case protocol.TypePong:
		case protocol.TypeRegistered:
			f.handleRegistered(m)
		case protocol.TypeOpen:
			go f.handleOpen(c, m)
		case protocol.TypeError:
			return true, fmt.Errorf("agent error: %s", m.Error)
		default:
			f.log.Debugf("ignoring unknown message %q from agent", m.Type)
		}
	}
}

func (f *Forwarder) handleRegistered(m *protocol.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, wanted := f.targets[m.Port]; !wanted {
		return // removed in the meantime
	}
	if m.Error != "" {
		f.log.Errorf("%s", m.Error)
		return
	}
	if !f.active[m.Port] {
		f.active[m.Port] = true
		f.log.Infof("forwarding %s (host) -> port %d (container)", m.Address, m.Port)
	}
}

func (f *Forwarder) handleOpen(ctrl *net.UnixConn, m *protocol.Message) {
	fail := func(reason string) {
		f.send(ctrl, &protocol.Message{Type: protocol.TypeOpenFailed, Stream: m.Stream, Error: reason})
	}
	select {
	case f.slots <- struct{}{}:
		defer func() { <-f.slots }()
	default:
		f.log.Warnf("too many forwarded connections; rejecting connection to port %d", m.Port)
		fail("too many connections")
		return
	}
	f.mu.Lock()
	addrs, ok := f.targets[m.Port]
	f.mu.Unlock()
	if !ok {
		fail("port not forwarded")
		return
	}
	local, err := f.dialLocal(addrs)
	if err != nil {
		f.log.Warnf("cannot connect to port %d in the container: %v", m.Port, err)
		fail(err.Error())
		return
	}
	data, err := net.DialTimeout("unix", f.cfg.SocketPath, f.cfg.DialTimeout)
	if err != nil {
		local.Close()
		f.log.Warnf("cannot open data connection to agent: %v", err)
		return // the agent's open timeout cleans up
	}
	data.SetWriteDeadline(time.Now().Add(f.cfg.DialTimeout))
	if err := protocol.Write(data, &protocol.Message{Type: protocol.TypeAttach, Version: protocol.Version, Stream: m.Stream, Token: m.Token}); err != nil {
		local.Close()
		data.Close()
		return
	}
	data.SetWriteDeadline(time.Time{})
	tunnel.Pipe(data, local)
}

func (f *Forwarder) dialLocal(addrs []string) (net.Conn, error) {
	var firstErr error
	for _, a := range addrs {
		c, err := net.DialTimeout("tcp", a, f.cfg.DialTimeout)
		if err == nil {
			return c, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = errors.New("no address to connect to")
	}
	return nil, firstErr
}

// Probe reports whether something accepts connections on one of addrs.
func Probe(addrs []string, timeout time.Duration) bool {
	for _, a := range addrs {
		if c, err := net.DialTimeout("tcp", a, timeout); err == nil {
			c.Close()
			return true
		}
	}
	return false
}
