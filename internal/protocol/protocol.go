// Package protocol defines the wire format spoken between the host agent and
// the container client over the shared Unix domain socket.
//
// Every Unix socket connection starts with one length-prefixed JSON message:
//
//   - "hello"  opens a control session. Control sessions keep exchanging
//     framed JSON messages (register, open, ping, ...) for their lifetime.
//   - "attach" turns the connection into a raw data stream for one forwarded
//     TCP connection. After the attach frame the connection carries the TCP
//     byte stream unmodified, so the kernel provides flow control and
//     half-close for free.
//
// A frame is a 4-byte big-endian payload length followed by a JSON object.
package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Version is the protocol version. Peers with different versions refuse to talk.
const Version = 1

// MaxFrameSize bounds a single control message.
const MaxFrameSize = 16 * 1024

// Message types.
const (
	TypeHello      = "hello"       // client -> agent: open control session
	TypeWelcome    = "welcome"     // agent -> client: session accepted (or Error set)
	TypeRegister   = "register"    // client -> agent: forward Port
	TypeRegistered = "registered"  // agent -> client: result of register (Error set on failure)
	TypeUnregister = "unregister"  // client -> agent: stop forwarding Port
	TypeOpen       = "open"        // agent -> client: new TCP connection for Port, attach Stream with Token
	TypeOpenFailed = "open_failed" // client -> agent: could not reach Port for Stream
	TypeAttach     = "attach"      // client -> agent (new socket): bind this socket to Stream
	TypePing       = "ping"
	TypePong       = "pong"
	TypeError      = "error" // fatal error, the sender closes the connection afterwards
)

// Message is the single envelope used for all control messages.
// Fields not relevant to a message type are omitted.
type Message struct {
	Type     string `json:"type"`
	Version  int    `json:"version,omitempty"`
	Software string `json:"software,omitempty"`
	Port     int    `json:"port,omitempty"`
	Stream   uint64 `json:"stream,omitempty"`
	Token    string `json:"token,omitempty"`
	Address  string `json:"address,omitempty"` // registered: host address now listening
	Error    string `json:"error,omitempty"`
}

// ErrFrameTooLarge is returned when a peer announces an oversized frame.
var ErrFrameTooLarge = errors.New("protocol: frame too large")

// Write encodes m as one frame. It performs a single Write call so that
// concurrent writers only need to serialize calls to Write itself.
func Write(w io.Writer, m *Message) error {
	payload, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(payload) > MaxFrameSize {
		return ErrFrameTooLarge
	}
	buf := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(len(payload)))
	copy(buf[4:], payload)
	_, err = w.Write(buf)
	return err
}

// Read decodes one frame. It never reads past the end of the frame, so the
// reader can be handed over to a raw byte stream afterwards.
func Read(r io.Reader) (*Message, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > MaxFrameSize {
		return nil, ErrFrameTooLarge
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	var m Message
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, fmt.Errorf("protocol: invalid message: %w", err)
	}
	if m.Type == "" {
		return nil, errors.New("protocol: message without type")
	}
	return &m, nil
}

// CheckVersion returns a user facing error if the peer version differs.
func CheckVersion(peer int) error {
	if peer != Version {
		return fmt.Errorf("protocol version mismatch (local %d, peer %d); use the same xvia release on host and in the container", Version, peer)
	}
	return nil
}

// ValidPort reports whether p is a usable TCP port number.
func ValidPort(p int) bool { return p >= 1 && p <= 65535 }
