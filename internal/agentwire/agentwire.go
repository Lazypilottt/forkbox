// Package agentwire holds what the host and the guest agent share: the vsock
// port, the protocol version and length-prefixed protobuf framing
// (proto/agent.proto).
package agentwire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"
)

const (
	// Port is the guest vsock port the agent listens on.
	Port = 5250
	// ProtocolVersion changes when either side must change in step.
	ProtocolVersion = 1
	// MaxFrame bounds a frame's payload, so a corrupt or hostile length
	// prefix cannot make either side allocate unbounded memory.
	MaxFrame = 1 << 20
	// ChunkSize is the data size of file and output frames.
	ChunkSize = 256 << 10
)

// ErrFrameTooLarge means a peer announced a frame over MaxFrame.
var ErrFrameTooLarge = errors.New("agentwire: frame too large")

// Write sends one frame.
func Write(w io.Writer, m proto.Message) error {
	b, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(b))
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

// Read receives one frame into m. A clean end of stream before the length
// prefix is io.EOF; a stream cut inside a frame is io.ErrUnexpectedEOF.
func Read(r io.Reader, m proto.Message) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrame {
		return fmt.Errorf("%w: %d bytes announced", ErrFrameTooLarge, n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	return proto.Unmarshal(b, m)
}
