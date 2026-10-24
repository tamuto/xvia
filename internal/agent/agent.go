// Package agent implements the host side of xvia: it accepts container
// clients on a Unix domain socket, opens TCP listeners for the ports they
// register and relays every accepted TCP connection to the owning client.
package agent

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/tamuto/xvia/internal/config"
	"github.com/tamuto/xvia/internal/logx"
	"github.com/tamuto/xvia/internal/protocol"
	"github.com/tamuto/xvia/internal/tunnel"
)

// Config configures an Agent. Zero values are replaced by defaults.
type Config struct {
	SocketPath string
	Bind       net.IP
	AllowPorts config.PortRange
	Software   string // xvia release, reported to clients
	Log        *logx.Logger

	MaxClients        int           // concurrent control sessions
	MaxPortsPerClient int           // listeners per session
	MaxConns          int           // concurrent forwarded TCP connections (active + pending)
	HandshakeTimeout  time.Duration // first message on a new Unix connection
	OpenTimeout       time.Duration // client must attach a stream within this time
	ClientTimeout     time.Duration // session closed if nothing is received for this long
	WriteTimeout      time.Duration // control message write deadline
}

func (c *Config) setDefaults() {
	if c.Bind == nil {
		c.Bind = net.IPv4(127, 0, 0, 1)
	}
	if c.AllowPorts == (config.PortRange{}) {
		c.AllowPorts, _ = config.ParsePortRange(config.DefaultAllowPorts)
	}
	if c.Log == nil {
		c.Log = logx.Discard()
	}
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	defDur := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&c.MaxClients, 64)
	def(&c.MaxPortsPerClient, 64)
	def(&c.MaxConns, 1024)
	defDur(&c.HandshakeTimeout, 5*time.Second)
	defDur(&c.OpenTimeout, 10*time.Second)
	defDur(&c.ClientTimeout, 45*time.Second)
	defDur(&c.WriteTimeout, 10*time.Second)
}

// Agent is the host agent. Create it with Listen and run it with Serve.
type Agent struct {
	cfg   Config
	log   *logx.Logger
	ln    *net.UnixListener
	slots chan struct{} // semaphore bounding forwarded connections

	mu       sync.Mutex
	closed   bool
	ports    map[int]*forward
	sessions map[*session]struct{}
	pending  map[uint64]*pendingConn
	nextSess uint64
	nextID   uint64 // stream IDs
	wg       sync.WaitGroup
}

type forward struct {
	port int
	ln   net.Listener
	sess *session
}

type pendingConn struct {
	tcp   net.Conn
	token string
	sess  *session
	timer *time.Timer
}

// Listen prepares the socket directory, removes a stale socket and starts
// listening. It fails if another agent is already serving the socket.
func Listen(cfg Config) (*Agent, error) {
	cfg.setDefaults()
	if err := config.ValidateSocketPath(cfg.SocketPath); err != nil {
		return nil, err
	}
	if err := prepareSocketDir(filepath.Dir(cfg.SocketPath)); err != nil {
		return nil, err
	}
	if err := removeStaleSocket(cfg.SocketPath); err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: cfg.SocketPath, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", cfg.SocketPath, err)
	}
	// We remove the file ourselves in Close, also after a crash on next start.
	ln.SetUnlinkOnClose(false)
	if err := os.Chmod(cfg.SocketPath, 0o600); err != nil {
		ln.Close()
		os.Remove(cfg.SocketPath)
		return nil, err
	}
	return &Agent{
		cfg:      cfg,
		log:      cfg.Log,
		ln:       ln,
		slots:    make(chan struct{}, cfg.MaxConns),
		ports:    make(map[int]*forward),
		sessions: make(map[*session]struct{}),
		pending:  make(map[uint64]*pendingConn),
	}, nil
}

// prepareSocketDir creates dir with mode 0700 and refuses directories owned
// by somebody else, since whoever controls the directory controls the socket.
func prepareSocketDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create socket directory: %w", err)
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("socket directory %s is not a directory", dir)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("socket directory %s is owned by uid %d, not by the current user (uid %d); "+
			"remove it and restart the agent (Docker creates missing bind mount sources as root, so start the agent before the containers)",
			dir, sys.Uid, os.Getuid())
	}
	if st.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("restrict socket directory permissions: %w", err)
		}
	}
	return nil
}

func removeStaleSocket(path string) error {
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket; refusing to replace it", path)
	}
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		return fmt.Errorf("another xvia agent is already listening on %s", path)
	}
	return os.Remove(path)
}

// SocketPath returns the path the agent listens on.
func (a *Agent) SocketPath() string { return a.cfg.SocketPath }

// Serve accepts clients until ctx is cancelled or Close is called. It always
// returns after all connections have been torn down.
func (a *Agent) Serve(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { a.Close() })
	defer stop()
	a.log.Infof("agent listening on %s (forwarding to %s, ports %s)", a.cfg.SocketPath, a.cfg.Bind, a.cfg.AllowPorts)
	for {
		c, err := a.ln.AcceptUnix()
		if err != nil {
			a.mu.Lock()
			closed := a.closed
			a.mu.Unlock()
			if closed {
				a.wg.Wait()
				return nil
			}
			a.log.Warnf("accept on unix socket: %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if !a.track() {
			c.Close()
			continue
		}
		go func() {
			defer a.wg.Done()
			a.handleUnix(c)
		}()
	}
}

// track registers a goroutine with the agent's WaitGroup unless the agent is closing.
func (a *Agent) track() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return false
	}
	a.wg.Add(1)
	return true
}

// Close stops the agent: the Unix listener, every session, every TCP
// listener and every forwarded connection are closed and the socket file is
// removed.
func (a *Agent) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	sessions := make([]*session, 0, len(a.sessions))
	for s := range a.sessions {
		sessions = append(sessions, s)
	}
	a.mu.Unlock()

	err := a.ln.Close()
	os.Remove(a.cfg.SocketPath)
	for _, s := range sessions {
		s.conn.Close() // the session goroutine performs the cleanup
	}
	return err
}

func (a *Agent) handleUnix(c *net.UnixConn) {
	c.SetDeadline(time.Now().Add(a.cfg.HandshakeTimeout))
	m, err := protocol.Read(c)
	if err != nil {
		a.log.Debugf("handshake failed: %v", err)
		c.Close()
		return
	}
	switch m.Type {
	case protocol.TypeHello:
		a.handleHello(c, m)
	case protocol.TypeAttach:
		a.handleAttach(c, m)
	default:
		a.log.Debugf("unexpected first message %q", m.Type)
		protocol.Write(c, &protocol.Message{Type: protocol.TypeError, Error: "expected hello or attach"})
		c.Close()
	}
}

func (a *Agent) handleHello(c *net.UnixConn, m *protocol.Message) {
	reject := func(reason string) {
		a.log.Warnf("rejected client: %s", reason)
		protocol.Write(c, &protocol.Message{Type: protocol.TypeWelcome, Version: protocol.Version, Software: a.cfg.Software, Error: reason})
		c.Close()
	}
	if err := protocol.CheckVersion(m.Version); err != nil {
		reject(fmt.Sprintf("%v (agent %s, client %s)", err, a.cfg.Software, m.Software))
		return
	}
	s := &session{a: a, conn: c, ports: make(map[int]*forward), streams: make(map[net.Conn]struct{})}
	a.mu.Lock()
	full := len(a.sessions) >= a.cfg.MaxClients
	if !full {
		a.nextSess++
		s.id = a.nextSess
		a.sessions[s] = struct{}{}
	}
	a.mu.Unlock()
	if full {
		reject("too many clients")
		return
	}
	if err := s.send(&protocol.Message{Type: protocol.TypeWelcome, Version: protocol.Version, Software: a.cfg.Software}); err != nil {
		s.close()
		return
	}
	a.log.Infof("client #%d connected", s.id)
	s.run()
}

func (a *Agent) handleAttach(c *net.UnixConn, m *protocol.Message) {
	a.mu.Lock()
	p := a.pending[m.Stream]
	ok := p != nil && protocol.CheckVersion(m.Version) == nil &&
		subtle.ConstantTimeCompare([]byte(p.token), []byte(m.Token)) == 1
	if ok {
		delete(a.pending, m.Stream)
		p.timer.Stop()
		p.sess.streams[p.tcp] = struct{}{}
		p.sess.streams[c] = struct{}{}
	}
	a.mu.Unlock()
	if !ok {
		a.log.Debugf("attach for unknown stream %d rejected", m.Stream)
		c.Close()
		return
	}
	c.SetDeadline(time.Time{})
	tunnel.Pipe(p.tcp, c)
	a.mu.Lock()
	delete(p.sess.streams, p.tcp)
	delete(p.sess.streams, c)
	a.mu.Unlock()
	<-a.slots
	a.log.Debugf("stream %d closed", m.Stream)
}

// takePending removes a pending connection; the caller then owns its TCP
// connection and its slot.
func (a *Agent) takePending(id uint64, sess *session) *pendingConn {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.pending[id]
	if p == nil || (sess != nil && p.sess != sess) {
		return nil
	}
	delete(a.pending, id)
	p.timer.Stop()
	return p
}

func (a *Agent) dropPending(p *pendingConn) {
	p.tcp.Close()
	<-a.slots
}

func newToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// session is one connected client.
type session struct {
	a    *Agent
	id   uint64
	conn *net.UnixConn
	wmu  sync.Mutex

	// Guarded by a.mu.
	ports   map[int]*forward
	streams map[net.Conn]struct{}
}

func (s *session) send(m *protocol.Message) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.conn.SetWriteDeadline(time.Now().Add(s.a.cfg.WriteTimeout))
	err := protocol.Write(s.conn, m)
	if err != nil {
		s.conn.Close() // makes run() return and clean up
	}
	return err
}

func (s *session) run() {
	defer s.close()
	for {
		s.conn.SetReadDeadline(time.Now().Add(s.a.cfg.ClientTimeout))
		m, err := protocol.Read(s.conn)
		if err != nil {
			s.a.log.Debugf("client #%d read: %v", s.id, err)
			return
		}
		switch m.Type {
		case protocol.TypePing:
			s.send(&protocol.Message{Type: protocol.TypePong})
		case protocol.TypeRegister:
			reply := &protocol.Message{Type: protocol.TypeRegistered, Port: m.Port}
			if err := s.register(m.Port); err != nil {
				reply.Error = err.Error()
			} else {
				reply.Address = net.JoinHostPort(s.a.cfg.Bind.String(), strconv.Itoa(m.Port))
			}
			s.send(reply)
		case protocol.TypeUnregister:
			s.unregister(m.Port)
		case protocol.TypeOpenFailed:
			if p := s.a.takePending(m.Stream, s); p != nil {
				s.a.log.Debugf("client #%d could not open stream %d: %s", s.id, m.Stream, m.Error)
				s.a.dropPending(p)
			}
		default:
			// Unknown messages are ignored so that newer clients with
			// optional extensions keep working.
			s.a.log.Debugf("client #%d sent unknown message %q", s.id, m.Type)
		}
	}
}

func (s *session) register(port int) error {
	a := s.a
	if !protocol.ValidPort(port) {
		return fmt.Errorf("invalid port %d", port)
	}
	if !a.cfg.AllowPorts.Contains(port) {
		return fmt.Errorf("port %d is outside the range allowed by the agent (%s)", port, a.cfg.AllowPorts)
	}
	a.mu.Lock()
	if f, ok := a.ports[port]; ok {
		a.mu.Unlock()
		if f.sess == s {
			return nil
		}
		a.log.Warnf("client #%d: port %d is already forwarded for client #%d", s.id, port, f.sess.id)
		return fmt.Errorf("port %d is already in use", port)
	}
	if len(s.ports) >= a.cfg.MaxPortsPerClient {
		a.mu.Unlock()
		return fmt.Errorf("too many ports registered (max %d)", a.cfg.MaxPortsPerClient)
	}
	// Reserve the port while listening outside the lock.
	f := &forward{port: port, sess: s}
	a.ports[port] = f
	a.mu.Unlock()

	addr := net.JoinHostPort(a.cfg.Bind.String(), strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)

	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		delete(a.ports, port)
		a.log.Warnf("client #%d: cannot listen on %s: %v", s.id, addr, err)
		if errors.Is(err, syscall.EADDRINUSE) {
			return fmt.Errorf("port %d is already in use", port)
		}
		return fmt.Errorf("cannot listen on %s: %v", addr, errors.Unwrap(err))
	}
	if _, alive := a.sessions[s]; !alive || a.ports[port] != f {
		// The session ended while we were listening.
		delete(a.ports, port)
		ln.Close()
		return errors.New("session closed")
	}
	f.ln = ln
	s.ports[port] = f
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.acceptLoop(f)
	}()
	a.log.Infof("forwarding %s -> client #%d", addr, s.id)
	return nil
}

func (s *session) unregister(port int) {
	a := s.a
	a.mu.Lock()
	f := s.ports[port]
	if f != nil {
		delete(s.ports, port)
		delete(a.ports, port)
	}
	a.mu.Unlock()
	if f != nil {
		f.ln.Close()
		a.log.Infof("stopped forwarding port %d (client #%d)", port, s.id)
	}
}

// close tears the session down and releases everything it owns.
func (s *session) close() {
	a := s.a
	s.conn.Close()
	a.mu.Lock()
	if _, ok := a.sessions[s]; !ok {
		a.mu.Unlock()
		return
	}
	delete(a.sessions, s)
	var fwds []*forward
	for port, f := range s.ports {
		delete(a.ports, port)
		fwds = append(fwds, f)
	}
	s.ports = map[int]*forward{}
	var pend []*pendingConn
	for id, p := range a.pending {
		if p.sess == s {
			delete(a.pending, id)
			p.timer.Stop()
			pend = append(pend, p)
		}
	}
	var streams []net.Conn
	for c := range s.streams {
		streams = append(streams, c)
	}
	a.mu.Unlock()

	for _, f := range fwds {
		f.ln.Close()
		a.log.Infof("stopped forwarding port %d (client #%d disconnected)", f.port, s.id)
	}
	for _, p := range pend {
		a.dropPending(p)
	}
	for _, c := range streams {
		c.Close()
	}
	a.log.Infof("client #%d disconnected", s.id)
}

func (a *Agent) acceptLoop(f *forward) {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			a.log.Warnf("accept on port %d: %v", f.port, err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		select {
		case a.slots <- struct{}{}:
		default:
			a.log.Warnf("connection limit (%d) reached; rejecting connection on port %d", a.cfg.MaxConns, f.port)
			c.Close()
			continue
		}
		a.mu.Lock()
		a.nextID++
		id := a.nextID
		p := &pendingConn{tcp: c, token: newToken(), sess: f.sess}
		p.timer = time.AfterFunc(a.cfg.OpenTimeout, func() {
			if p := a.takePending(id, nil); p != nil {
				a.log.Warnf("client did not open stream %d for port %d in time", id, f.port)
				a.dropPending(p)
			}
		})
		a.pending[id] = p
		a.mu.Unlock()
		a.log.Debugf("port %d: new connection from %s (stream %d)", f.port, c.RemoteAddr(), id)
		if err := f.sess.send(&protocol.Message{Type: protocol.TypeOpen, Port: f.port, Stream: id, Token: p.token}); err != nil {
			if p := a.takePending(id, nil); p != nil {
				a.dropPending(p)
			}
		}
	}
}

// Ports returns the currently forwarded ports (for tests and diagnostics).
func (a *Agent) Ports() []int {
	a.mu.Lock()
	defer a.mu.Unlock()
	ports := make([]int, 0, len(a.ports))
	for p, f := range a.ports {
		if f.ln != nil {
			ports = append(ports, p)
		}
	}
	return ports
}
