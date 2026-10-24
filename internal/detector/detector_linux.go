// Package detector finds TCP ports in LISTEN state that are owned by the
// descendants of a process, using only /proc.
//
// /proc/<pid>/net/tcp{,6} lists every socket of the network namespace, not
// just those of <pid>. A port is therefore attributed to a process only if
// the socket inode from that table is also referenced by one of the
// process's file descriptors (/proc/<pid>/fd/* -> "socket:[inode]").
package detector

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/tamuto/xvia/internal/logx"
	"github.com/tamuto/xvia/internal/process"
)

const tcpListen = "0A"

// Detector scans the descendants of Root.
type Detector struct {
	Root int
	Log  *logx.Logger

	mu     sync.Mutex
	denied map[process.Proc]bool // processes we already warned about
}

// New returns a detector for the descendants of root (root itself excluded).
func New(root int, log *logx.Logger) *Detector {
	if log == nil {
		log = logx.Discard()
	}
	return &Detector{Root: root, Log: log, denied: map[process.Proc]bool{}}
}

// Scan returns the listening ports of all descendants, mapped to the
// addresses that reach them from inside the container (preferred first).
func (d *Detector) Scan() map[int][]string {
	procs := process.Descendants(d.Root)
	inodes := map[uint64]bool{}
	netns := map[string]int{} // namespace -> one pid in it
	for _, p := range procs {
		if !d.socketInodes(p, inodes) {
			continue
		}
		ns, err := os.Readlink(filepath.Join(process.ProcRoot, strconv.Itoa(p.Pid), "ns", "net"))
		if err != nil {
			ns = "unknown"
		}
		if _, ok := netns[ns]; !ok {
			netns[ns] = p.Pid
		}
	}
	listen := map[int][]net.IP{}
	if len(inodes) == 0 {
		return map[int][]string{}
	}
	for _, pid := range netns {
		for _, file := range []string{"tcp", "tcp6"} {
			path := filepath.Join(process.ProcRoot, strconv.Itoa(pid), "net", file)
			entries, err := readTCP(path)
			if err != nil {
				continue // tcp6 is missing when IPv6 is disabled
			}
			for _, e := range entries {
				if inodes[e.inode] {
					listen[e.port] = append(listen[e.port], e.ip)
				}
			}
		}
	}
	out := make(map[int][]string, len(listen))
	for port, ips := range listen {
		out[port] = DialAddrs(port, ips)
	}
	return out
}

// socketInodes adds the socket inodes held by p to set. It reports false if
// the process could not be inspected.
func (d *Detector) socketInodes(p process.Proc, set map[uint64]bool) bool {
	dir := filepath.Join(process.ProcRoot, strconv.Itoa(p.Pid), "fd")
	fds, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			d.mu.Lock()
			if !d.denied[p] {
				d.denied[p] = true
				d.Log.Warnf("cannot inspect process %d (permission denied); its ports will not be detected, use --port", p.Pid)
			}
			d.mu.Unlock()
		}
		return false
	}
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join(dir, fd.Name()))
		if err != nil || !strings.HasPrefix(target, "socket:[") {
			continue
		}
		if ino, err := strconv.ParseUint(strings.TrimSuffix(target[len("socket:["):], "]"), 10, 64); err == nil {
			set[ino] = true
		}
	}
	return true
}

type tcpEntry struct {
	ip    net.IP
	port  int
	inode uint64
}

// readTCP returns the LISTEN entries of a /proc/net/tcp{,6} file.
func readTCP(path string) ([]tcpEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []tcpEntry
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		if e, ok := parseTCPLine(sc.Text()); ok {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

func parseTCPLine(line string) (tcpEntry, bool) {
	f := strings.Fields(line)
	if len(f) < 10 || f[3] != tcpListen {
		return tcpEntry{}, false
	}
	host, portHex, ok := strings.Cut(f[1], ":")
	if !ok {
		return tcpEntry{}, false
	}
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil || port == 0 {
		return tcpEntry{}, false
	}
	ip, ok := parseHexIP(host)
	if !ok {
		return tcpEntry{}, false
	}
	inode, err := strconv.ParseUint(f[9], 10, 64)
	if err != nil || inode == 0 {
		return tcpEntry{}, false
	}
	return tcpEntry{ip: ip, port: int(port), inode: inode}, true
}

// parseHexIP decodes the kernel's representation: the address as 32-bit
// words in host byte order.
func parseHexIP(s string) (net.IP, bool) {
	raw, err := hex.DecodeString(s)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return nil, false
	}
	ip := make(net.IP, len(raw))
	for i := 0; i < len(raw); i += 4 {
		binary.BigEndian.PutUint32(ip[i:], binary.NativeEndian.Uint32(raw[i:]))
	}
	return ip, true
}

// DialAddrs converts listen addresses into addresses to connect to:
// wildcard addresses are reached through loopback.
func DialAddrs(port int, ips []net.IP) []string {
	var v4, v6 []string
	seen := map[string]bool{}
	add := func(list *[]string, ip net.IP) {
		a := net.JoinHostPort(ip.String(), strconv.Itoa(port))
		if !seen[a] {
			seen[a] = true
			*list = append(*list, a)
		}
	}
	for _, ip := range ips {
		switch {
		case ip.Equal(net.IPv4zero):
			add(&v4, net.IPv4(127, 0, 0, 1))
		case ip.Equal(net.IPv6unspecified):
			add(&v6, net.IPv6loopback)
			add(&v4, net.IPv4(127, 0, 0, 1))
		case ip.To4() != nil:
			add(&v4, ip.To4())
		default:
			add(&v6, ip)
		}
	}
	sort.Strings(v4)
	sort.Strings(v6)
	return append(v4, v6...)
}
