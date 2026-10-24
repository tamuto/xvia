package detector

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tamuto/xvia/internal/process"
)

// TestMain doubles as a helper process that listens on TCP ports.
func TestMain(m *testing.M) {
	if addrs := os.Getenv("XVIA_TEST_LISTEN"); addrs != "" {
		var ports []string
		for _, a := range strings.Split(addrs, ",") {
			ln, err := net.Listen("tcp", a)
			if err != nil {
				os.Exit(3)
			}
			defer ln.Close()
			ports = append(ports, strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
		}
		os.Stdout.WriteString(strings.Join(ports, ",") + "\n")
		time.Sleep(time.Minute)
		return
	}
	os.Exit(m.Run())
}

// startListener starts a helper (optionally through a shell, making it a
// grandchild) and returns the ports it listens on.
func startListener(t *testing.T, viaShell bool, addrs ...string) []int {
	t.Helper()
	var cmd *exec.Cmd
	if viaShell {
		cmd = exec.Command("sh", "-c", `"$0"; :`, os.Args[0])
	} else {
		cmd = exec.Command(os.Args[0])
	}
	cmd.Env = append(os.Environ(), "XVIA_TEST_LISTEN="+strings.Join(addrs, ","))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, p := range process.Descendants(cmd.Process.Pid) {
			syscall.Kill(p.Pid, syscall.SIGKILL)
		}
		cmd.Process.Kill()
		cmd.Wait()
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("helper failed: %v", err)
	}
	var ports []int
	for _, p := range strings.Split(strings.TrimSpace(line), ",") {
		n, _ := strconv.Atoi(p)
		ports = append(ports, n)
	}
	return ports
}

func TestDetectsDescendantListeners(t *testing.T) {
	// A listener owned by the root (not a descendant) must be ignored.
	own, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	ownPort := own.Addr().(*net.TCPAddr).Port

	addrs := []string{"127.0.0.1:0", "0.0.0.0:0"}
	ipv6 := false
	if ln, err := net.Listen("tcp", "[::1]:0"); err == nil {
		ln.Close()
		ipv6 = true
		addrs = append(addrs, "[::1]:0")
	}
	ports := startListener(t, true, addrs...)

	d := New(os.Getpid(), nil)
	got := d.Scan()
	if _, ok := got[ownPort]; ok {
		t.Fatalf("root's own listener %d reported", ownPort)
	}
	want := map[int][]string{ports[0]: {"127.0.0.1:" + strconv.Itoa(ports[0])}}
	// Go listens on "0.0.0.0" with a dual-stack [::] socket when IPv6 is available.
	if wild := got[ports[1]]; len(wild) == 0 || wild[0] != "127.0.0.1:"+strconv.Itoa(ports[1]) {
		t.Errorf("wildcard port %d: got %v", ports[1], wild)
	}
	if ipv6 {
		want[ports[2]] = []string{"[::1]:" + strconv.Itoa(ports[2])}
	}
	for port, addrs := range want {
		if !slices.Equal(got[port], addrs) {
			t.Errorf("port %d: got %v want %v (all: %v)", port, got[port], addrs, got)
		}
	}
}

func TestUnrelatedProcessIgnored(t *testing.T) {
	// The helper is our child; scanning from an unrelated process (a sibling)
	// must not report its port even though both share the network namespace.
	ports := startListener(t, false, "127.0.0.1:0")
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { sleeper.Process.Kill(); sleeper.Wait() }()
	got := New(sleeper.Process.Pid, nil).Scan()
	if _, ok := got[ports[0]]; ok {
		t.Fatal("listener of an unrelated process detected")
	}
}

func TestPortDisappearsWhenProcessExits(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "XVIA_TEST_LISTEN=127.0.0.1:0")
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	port, _ := strconv.Atoi(strings.TrimSpace(line))
	d := New(os.Getpid(), nil)
	if _, ok := d.Scan()[port]; !ok {
		t.Fatal("port not detected")
	}
	cmd.Process.Kill()
	cmd.Wait()
	if _, ok := d.Scan()[port]; ok {
		t.Fatal("port still reported after exit")
	}
}

func TestParseTCPLine(t *testing.T) {
	// Lines as produced on little-endian machines.
	listen4 := "   0: 0100007F:1435 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12345 1 0000000000000000 100 0 0 10 0"
	e, ok := parseTCPLine(listen4)
	if !ok || e.port != 0x1435 || e.inode != 12345 {
		t.Fatalf("got %+v %v", e, ok)
	}
	if isLittleEndian() && !e.ip.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("ip %v", e.ip)
	}
	established := strings.Replace(listen4, " 0A ", " 01 ", 1)
	if _, ok := parseTCPLine(established); ok {
		t.Fatal("non-LISTEN socket accepted")
	}
	listen6 := "   0: 00000000000000000000000001000000:1F90 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 777 1 0000000000000000 100 0 0 10 0"
	e, ok = parseTCPLine(listen6)
	if !ok || e.port != 8080 || e.inode != 777 {
		t.Fatalf("got %+v %v", e, ok)
	}
	if isLittleEndian() && !e.ip.Equal(net.IPv6loopback) {
		t.Fatalf("ip %v", e.ip)
	}
	for _, bad := range []string{"", "garbage", "0: zz:1435 0 0A a b c d e 1"} {
		if _, ok := parseTCPLine(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

func isLittleEndian() bool {
	ip, _ := parseHexIP("0100007F")
	return ip.Equal(net.IPv4(127, 0, 0, 1))
}

func TestDialAddrs(t *testing.T) {
	cases := []struct {
		ips  []net.IP
		want []string
	}{
		{[]net.IP{net.IPv4zero}, []string{"127.0.0.1:80"}},
		{[]net.IP{net.IPv6unspecified}, []string{"127.0.0.1:80", "[::1]:80"}},
		{[]net.IP{net.IPv6loopback}, []string{"[::1]:80"}},
		{[]net.IP{net.IPv4zero, net.IPv6unspecified}, []string{"127.0.0.1:80", "[::1]:80"}},
		{[]net.IP{net.ParseIP("::ffff:10.0.0.5")}, []string{"10.0.0.5:80"}},
		{[]net.IP{net.ParseIP("172.17.0.2")}, []string{"172.17.0.2:80"}},
	}
	for _, c := range cases {
		if got := DialAddrs(80, c.ips); !slices.Equal(got, c.want) {
			t.Errorf("%v: got %v want %v", c.ips, got, c.want)
		}
	}
}
