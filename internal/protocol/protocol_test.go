package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := &Message{Type: TypeOpen, Port: 5173, Stream: 42, Token: "abc"}
	if err := Write(&buf, in); err != nil {
		t.Fatal(err)
	}
	buf.WriteString("trailing raw bytes")
	out, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if *out != *in {
		t.Fatalf("got %+v want %+v", out, in)
	}
	if rest, _ := io.ReadAll(&buf); string(rest) != "trailing raw bytes" {
		t.Fatalf("Read consumed bytes beyond the frame: %q", rest)
	}
}

func TestReadRejectsBadFrames(t *testing.T) {
	frame := func(payload string) []byte {
		b := make([]byte, 4+len(payload))
		binary.BigEndian.PutUint32(b, uint32(len(payload)))
		copy(b[4:], payload)
		return b
	}
	huge := make([]byte, 4)
	binary.BigEndian.PutUint32(huge, MaxFrameSize+1)

	cases := map[string][]byte{
		"oversized": huge,
		"zero":      {0, 0, 0, 0},
		"not json":  frame("\x00\x01garbage"),
		"no type":   frame(`{"port":1}`),
		"truncated": frame(`{"type":"ping"}`)[:8],
		"json list": frame(`[1,2,3]`),
	}
	for name, data := range cases {
		if _, err := Read(bytes.NewReader(data)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := Read(bytes.NewReader(huge)); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("oversized: got %v", err)
	}
}

func TestCheckVersion(t *testing.T) {
	if CheckVersion(Version) != nil {
		t.Fatal("same version rejected")
	}
	if CheckVersion(Version+1) == nil {
		t.Fatal("different version accepted")
	}
}
