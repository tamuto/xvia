package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// End-to-end tests run the real binary: "xvia agent" plus "xvia <server>",
// where the server is this test binary in helper mode. The agent binds
// 127.0.0.2 so that host and "container" can share one network namespace.

const hostIP = "127.0.0.2"

var xviaBin string

func TestMain(m *testing.M) {
	if addr := os.Getenv("XVIA_E2E_HTTP"); addr != "" {
		http.ListenAndServe(addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "pong %s", r.URL.Path)
		}))
		os.Exit(1)
	}
	dir, err := os.MkdirTemp("", "xvia-e2e")
	if err != nil {
		panic(err)
	}
	xviaBin = filepath.Join(dir, "xvia")
	goBin, err := exec.LookPath("go")
	if err != nil {
		goBin = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	build := exec.Command(goBin, "build", "-o", xviaBin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type proc struct {
	cmd  *exec.Cmd
	out  *syncBuffer
	done chan struct{}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func start(t *testing.T, env []string, args ...string) *proc {
	t.Helper()
	p := &proc{cmd: exec.Command(xviaBin, args...), out: &syncBuffer{}, done: make(chan struct{})}
	p.cmd.Env = append(os.Environ(), env...)
	p.cmd.Stdout = p.out
	p.cmd.Stderr = p.out
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		p.cmd.Process.Kill()
		<-p.done
		if t.Failed() {
			t.Logf("output of %v:\n%s", args, p.out.String())
		}
	})
	return p
}

func (p *proc) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		t.Fatalf("process did not exit; output:\n%s", p.out.String())
	}
	return p.cmd.ProcessState.ExitCode()
}

func startAgent(t *testing.T, socket string) *proc {
	t.Helper()
	a := start(t, nil, "agent", "--socket", socket, "--bind", hostIP)
	waitFor(t, "agent socket", func() bool {
		c, err := net.Dial("unix", socket)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	return a
}

// freePort returns a port that is free on both 127.0.0.1 and the host IP.
func freePort(t *testing.T) int {
	t.Helper()
	for i := 0; i < 20; i++ {
		ln, err := net.Listen("tcp", hostIP+":0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		if l2, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port)); err == nil {
			l2.Close()
			return port
		}
	}
	t.Fatal("no free port")
	return 0
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func get(port int, path string) (string, error) {
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get(fmt.Sprintf("http://%s:%d%s", hostIP, port, path))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func listening(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", hostIP, port), time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func serverEnv(port int) []string {
	return []string{fmt.Sprintf("XVIA_E2E_HTTP=127.0.0.1:%d", port)}
}

func TestE2EAutoDetectHTTP(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "xvia", "agent.sock")
	startAgent(t, socket)
	port := freePort(t)
	// Run the server through a shell so that the listener is a grandchild,
	// like Vite under pnpm.
	client := start(t, serverEnv(port), "--socket", socket, "--", "sh", "-c", `"$0"; exit 0`, os.Args[0])

	waitFor(t, "forwarding", func() bool { body, _ := get(port, "/"); return body == "pong /" })

	// Several concurrent connections.
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, err := get(port, "/"+strconv.Itoa(i))
			if err == nil && body != "pong /"+strconv.Itoa(i) {
				err = fmt.Errorf("unexpected body %q", body)
			}
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// Stopping the dev server removes the forwarding; the shell exits 0.
	stopServer(t, client)
	waitFor(t, "listener removal", func() bool { return !listening(port) })
	if code := client.wait(t); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(client.out.String(), fmt.Sprintf("forwarding %s:%d (host) -> port %d (container)", hostIP, port, port)) {
		t.Errorf("missing forwarding message:\n%s", client.out.String())
	}
}

// stopServer kills the helper server (the grandchild of xvia).
func stopServer(t *testing.T, client *proc) {
	t.Helper()
	out, _ := exec.Command("sh", "-c", fmt.Sprintf("for c in $(cat /proc/%d/task/*/children); do cat /proc/$c/task/*/children; done", client.cmd.Process.Pid)).Output()
	pids := strings.Fields(string(out))
	if len(pids) == 0 {
		t.Fatal("server process not found")
	}
	for _, p := range pids {
		pid, _ := strconv.Atoi(p)
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

func TestE2EClientKilledReleasesListener(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "agent.sock")
	startAgent(t, socket)
	port := freePort(t)
	client := start(t, serverEnv(port), "--socket", socket, "--", os.Args[0])
	waitFor(t, "forwarding", func() bool { body, _ := get(port, "/"); return body == "pong /" })

	client.cmd.Process.Signal(syscall.SIGKILL)
	client.wait(t)
	waitFor(t, "listener removal", func() bool { return !listening(port) })
	// The server is terminated too (parent death signal).
	waitFor(t, "server exit", func() bool {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			c.Close()
		}
		return err != nil
	})
}

func TestE2EManualPortAndAgentRestart(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "agent.sock")
	agent := startAgent(t, socket)
	port := freePort(t)
	client := start(t, serverEnv(port), "--socket", socket, "--no-detect", "--port", strconv.Itoa(port), "--", os.Args[0])
	waitFor(t, "forwarding", func() bool { body, _ := get(port, "/a"); return body == "pong /a" })

	agent.cmd.Process.Signal(syscall.SIGTERM)
	if code := agent.wait(t); code != 0 {
		t.Fatalf("agent exit code %d", code)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatal("agent did not remove its socket")
	}
	startAgent(t, socket)
	waitFor(t, "re-registration", func() bool { body, _ := get(port, "/b"); return body == "pong /b" })

	client.cmd.Process.Signal(syscall.SIGTERM)
	if code := client.wait(t); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit code %d", code)
	}
	waitFor(t, "listener removal", func() bool { return !listening(port) })
}

func TestE2EExitCodeWithoutAgent(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "missing.sock")
	p := start(t, nil, "--socket", socket, "sh", "-c", "sleep 0.3; exit 7")
	if code := p.wait(t); code != 7 {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(p.out.String(), "not found") {
		t.Errorf("expected a hint about the missing socket:\n%s", p.out.String())
	}
	if code := start(t, nil, "--socket", socket, "xvia-no-such-command").wait(t); code != 127 {
		t.Fatalf("missing command exit code %d", code)
	}
}

func TestE2EPortConflictKeepsServerRunning(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "agent.sock")
	startAgent(t, socket)
	port := freePort(t)
	start(t, serverEnv(port), "--socket", socket, "--", os.Args[0])
	waitFor(t, "forwarding", func() bool { body, _ := get(port, "/"); return body == "pong /" })

	// A second client wants the same port for a different server.
	second := start(t, nil, "--socket", socket, "--no-detect", "--port", strconv.Itoa(port), "sleep", "30")
	msg := fmt.Sprintf("xvia: port %d is already in use", port)
	waitFor(t, "conflict message", func() bool { return strings.Contains(second.out.String(), msg) })
	select {
	case <-second.done:
		t.Fatal("client exited after port conflict")
	default:
	}
}

func TestCLIUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--version"}, &out, &errOut); code != 0 || !strings.HasPrefix(out.String(), "xvia ") {
		t.Fatalf("--version: %d %q", code, out.String())
	}
	out.Reset()
	if code := run([]string{"version"}, &out, &errOut); code != 0 || !strings.HasPrefix(out.String(), "xvia ") {
		t.Fatalf("version: %d %q", code, out.String())
	}
	out.Reset()
	if code := run([]string{"--help"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "Usage:") {
		t.Fatalf("--help: %d", code)
	}
	for _, args := range [][]string{
		{},
		{"--port", "0", "true"},
		{"--port", "abc", "true"},
		{"--socket", "relative.sock", "true"},
		{"--poll-interval", "1ns", "true"},
		{"--bogus", "true"},
		{"agent", "--bind", "localhost"},
		{"agent", "--allow-ports", "10-5"},
		{"agent", "extra"},
	} {
		errOut.Reset()
		if code := run(args, &out, &errOut); code != 2 {
			t.Errorf("%v: exit %d, want 2 (%s)", args, code, errOut.String())
		}
	}
}

func TestPortListFlag(t *testing.T) {
	var p portList
	if err := p.Set("5173,8000"); err != nil {
		t.Fatal(err)
	}
	if err := p.Set("3000"); err != nil {
		t.Fatal(err)
	}
	if p.String() != "5173,8000,3000" {
		t.Fatal(p.String())
	}
}
