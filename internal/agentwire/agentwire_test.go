package agentwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/Lazypilottt/forkbox/proto/agentpb"
	"google.golang.org/protobuf/proto"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := []*agentpb.Request{
		{Op: &agentpb.Request_Ping{Ping: &agentpb.PingRequest{}}},
		{Op: &agentpb.Request_Exec{Exec: &agentpb.ExecRequest{Args: []string{"echo", "hi"}, TimeoutMs: 5}}},
		{Op: &agentpb.Request_Chunk{Chunk: &agentpb.Chunk{Data: bytes.Repeat([]byte{7}, ChunkSize), Eof: true}}},
	}
	for _, m := range in {
		if err := Write(&buf, m); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range in {
		got := &agentpb.Request{}
		if err := Read(&buf, got); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if !proto.Equal(got, want) {
			t.Fatalf("frame %d: got %v", i, got)
		}
	}
	if err := Read(&buf, &agentpb.Request{}); err != io.EOF {
		t.Fatalf("after last frame: got %v, want io.EOF", err)
	}
}

func TestOversizedFrames(t *testing.T) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], MaxFrame+1)
	if err := Read(bytes.NewReader(hdr[:]), &agentpb.Request{}); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("read: got %v", err)
	}
	big := &agentpb.Request{Op: &agentpb.Request_Chunk{Chunk: &agentpb.Chunk{Data: make([]byte, MaxFrame)}}}
	if err := Write(io.Discard, big); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("write: got %v", err)
	}
}

func TestTruncatedFrame(t *testing.T) {
	var buf bytes.Buffer
	Write(&buf, &agentpb.Request{Op: &agentpb.Request_Ping{Ping: &agentpb.PingRequest{}}})
	Write(&buf, &agentpb.Request{Op: &agentpb.Request_Stat{Stat: &agentpb.StatRequest{Path: "/etc/hostname"}}})
	b := buf.Bytes()[:buf.Len()-3]
	r := bytes.NewReader(b)
	Read(r, &agentpb.Request{})
	if err := Read(r, &agentpb.Request{}); err != io.ErrUnexpectedEOF {
		t.Errorf("got %v, want io.ErrUnexpectedEOF", err)
	}
}
