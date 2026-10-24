// Package config resolves defaults from flags and environment variables and
// validates user supplied values.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Environment variables.
const (
	EnvSocket       = "XVIA_SOCKET"
	EnvBind         = "XVIA_BIND"
	EnvLogLevel     = "XVIA_LOG_LEVEL"
	EnvAllowPorts   = "XVIA_ALLOW_PORTS"
	EnvPollInterval = "XVIA_POLL_INTERVAL"
)

// ContainerSocket is the conventional socket location inside containers.
const ContainerSocket = "/run/xvia/agent.sock"

// DefaultAllowPorts excludes privileged ports, which a non-root agent cannot bind anyway.
const DefaultAllowPorts = "1024-65535"

// maxSocketPath is the usable length of sun_path on Linux.
const maxSocketPath = 107

// Env returns the value of an environment variable or def when unset/empty.
func Env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// HostSocketPath is the default agent socket: $XDG_RUNTIME_DIR/xvia/agent.sock,
// /run/user/<uid>/xvia/agent.sock, or /tmp/xvia-<uid>/agent.sock as a last resort.
func HostSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "xvia", "agent.sock")
	}
	runUser := fmt.Sprintf("/run/user/%d", os.Getuid())
	if st, err := os.Stat(runUser); err == nil && st.IsDir() {
		return filepath.Join(runUser, "xvia", "agent.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("xvia-%d", os.Getuid()), "agent.sock")
}

// ClientSocketCandidates lists where the client looks for the agent socket,
// in order of preference.
func ClientSocketCandidates() []string {
	c := []string{ContainerSocket}
	if host := HostSocketPath(); host != ContainerSocket {
		c = append(c, host)
	}
	return c
}

// ClientSocketPath returns the first existing candidate, or the container
// default when none exists yet (the client keeps retrying).
func ClientSocketPath() string {
	cands := ClientSocketCandidates()
	for _, p := range cands {
		if st, err := os.Stat(p); err == nil && st.Mode()&os.ModeSocket != 0 {
			return p
		}
	}
	return cands[0]
}

// ValidateSocketPath checks that p is usable as a Unix socket path.
func ValidateSocketPath(p string) error {
	if p == "" {
		return fmt.Errorf("socket path is empty")
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("socket path %q must be absolute", p)
	}
	if len(p) > maxSocketPath {
		return fmt.Errorf("socket path %q is too long (max %d bytes)", p, maxSocketPath)
	}
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("socket path contains NUL byte")
	}
	return nil
}

// ParseBind validates the agent bind address. It must be an IP literal.
// It reports whether the address is loopback-only.
func ParseBind(s string) (ip net.IP, loopback bool, err error) {
	ip = net.ParseIP(strings.Trim(s, "[]"))
	if ip == nil {
		return nil, false, fmt.Errorf("invalid bind address %q: must be an IP address such as 127.0.0.1", s)
	}
	return ip, ip.IsLoopback(), nil
}

// PortRange is an inclusive range of allowed ports.
type PortRange struct{ Min, Max int }

// Contains reports whether p is within the range.
func (r PortRange) Contains(p int) bool { return p >= r.Min && p <= r.Max }

func (r PortRange) String() string { return fmt.Sprintf("%d-%d", r.Min, r.Max) }

// ParsePortRange parses "MIN-MAX" or a single port.
func ParsePortRange(s string) (PortRange, error) {
	lo, hi, found := strings.Cut(strings.TrimSpace(s), "-")
	if !found {
		hi = lo
	}
	min, err1 := ParsePort(lo)
	max, err2 := ParsePort(hi)
	if err1 != nil || err2 != nil || min > max {
		return PortRange{}, fmt.Errorf("invalid port range %q (want MIN-MAX, e.g. %s)", s, DefaultAllowPorts)
	}
	return PortRange{min, max}, nil
}

// ParsePort parses a TCP port number (1-65535).
func ParsePort(s string) (int, error) {
	p, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("invalid port %q (want 1-65535)", s)
	}
	return p, nil
}
