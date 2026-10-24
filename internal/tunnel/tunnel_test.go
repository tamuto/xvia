package tunnel

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"
)

// tcpPair returns two connected TCP connections.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		ch <- c
	}()
	c1, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return c1, <-ch
}

// TestPipeBidirectionalHalfClose sends a payload through the pipe, half-closes,
// and expects the reply to arrive after the half-close.
func TestPipeBidirectionalHalfClose(t *testing.T) {
	client, proxyIn := tcpPair(t)
	proxyOut, server := tcpPair(t)
	done := make(chan struct{})
	go func() {
		Pipe(proxyIn, proxyOut)
		close(done)
	}()

	payload := make([]byte, 4<<20)
	rand.Read(payload)

	// Server: read everything until EOF (half-close), then reply with the length.
	go func() {
		got, err := io.ReadAll(server)
		if err != nil {
			t.Error(err)
		}
		if !bytes.Equal(got, payload) {
			t.Error("server received corrupted payload")
		}
		server.Write([]byte("received"))
		server.Close()
	}()

	if _, err := client.Write(payload); err != nil {
		t.Fatal(err)
	}
	client.(*net.TCPConn).CloseWrite()
	client.SetReadDeadline(time.Now().Add(10 * time.Second))
	reply, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	if string(reply) != "received" {
		t.Fatalf("reply %q", reply)
	}
	client.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Pipe did not return after both sides closed")
	}
}

func TestPipeClosesOnReset(t *testing.T) {
	client, proxyIn := tcpPair(t)
	proxyOut, server := tcpPair(t)
	done := make(chan struct{})
	go func() {
		Pipe(proxyIn, proxyOut)
		close(done)
	}()
	// Abortive close of the server side.
	server.(*net.TCPConn).SetLinger(0)
	server.Close()
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(client); err != nil {
		t.Fatalf("client should observe close, got %v", err)
	}
	client.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Pipe did not return")
	}
}
