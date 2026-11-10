// Package proto implements the small framed protocol spoken between ffshim
// (inside the Jellyfin container) and ffagent (on the Windows host).
//
// Every message is a frame: 1 byte type, 4 byte big-endian length, payload.
//
//	agent → shim   Challenge (32 random bytes)
//	shim  → agent  Hello (JSON, includes HMAC-SHA256(token, challenge))
//	shim  → agent  Stdin / StdinEOF            (repeated)
//	agent → shim   Stdout / Stderr             (repeated)
//	agent → shim   Exit (int32 exit code) or Error (UTF-8 message)
//
// The token itself never crosses the network.
package proto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Version is bumped on incompatible protocol changes.
const Version = 1

// Frame types.
const (
	Challenge byte = 1
	Hello     byte = 2
	Stdin     byte = 3
	StdinEOF  byte = 4
	Stdout    byte = 5
	Stderr    byte = 6
	Exit      byte = 7
	Error     byte = 8
)

// MaxFrame limits a single frame's payload size.
const MaxFrame = 8 << 20

// NonceSize is the size of the challenge sent by the agent.
const NonceSize = 32

// Kinds of requests a shim can make.
const (
	KindRun  = "run"
	KindPing = "ping"
)

// HelloMsg is the first message a shim sends after receiving the challenge.
type HelloMsg struct {
	Proto         int      `json:"proto"`
	Kind          string   `json:"kind"`
	Args          []string `json:"args,omitempty"`
	Cwd           string   `json:"cwd,omitempty"`
	MAC           string   `json:"mac"`
	FFmpegVersion string   `json:"ffmpeg_version,omitempty"`
	ShimVersion   string   `json:"shim_version,omitempty"`
}

// ErrFrameTooLarge is returned when a peer announces an oversized frame.
var ErrFrameTooLarge = errors.New("proto: frame too large")

// WriteFrame writes a single frame to w.
func WriteFrame(w io.Writer, t byte, p []byte) error {
	if len(p) > MaxFrame {
		return ErrFrameTooLarge
	}
	buf := make([]byte, 5+len(p))
	buf[0] = t
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(p)))
	copy(buf[5:], p)
	_, err := w.Write(buf)
	return err
}

// ReadFrame reads a single frame from r.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:5])
	if n > MaxFrame {
		return 0, nil, ErrFrameTooLarge
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	return hdr[0], p, nil
}

// Writer serialises frame writes from multiple goroutines.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

// NewWriter wraps w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Send writes one frame. Payloads larger than MaxFrame are split.
func (w *Writer) Send(t byte, p []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	for {
		chunk := p
		if len(chunk) > MaxFrame {
			chunk = p[:MaxFrame]
		}
		if err := WriteFrame(w.w, t, chunk); err != nil {
			return err
		}
		p = p[len(chunk):]
		if len(p) == 0 {
			return nil
		}
	}
}

// StreamWriter adapts a Writer into an io.Writer emitting frames of type T.
// Write errors are swallowed so the producer (ffmpeg's pipe) keeps draining;
// a broken connection is detected and handled by the reading side.
type StreamWriter struct {
	W *Writer
	T byte
}

func (s StreamWriter) Write(p []byte) (int, error) {
	_ = s.W.Send(s.T, p)
	return len(p), nil
}

// Sign returns the hex HMAC-SHA256 of nonce keyed with token.
func Sign(token string, nonce []byte) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write(nonce)
	return hex.EncodeToString(m.Sum(nil))
}

// Verify checks a MAC produced by Sign in constant time.
func Verify(token string, nonce []byte, mac string) bool {
	got, err := hex.DecodeString(mac)
	if err != nil || token == "" {
		return false
	}
	m := hmac.New(sha256.New, []byte(token))
	m.Write(nonce)
	return hmac.Equal(got, m.Sum(nil))
}

// ExitPayload encodes an exit code.
func ExitPayload(code int) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(int32(code)))
	return b[:]
}

// ParseExit decodes an exit code.
func ParseExit(p []byte) (int, error) {
	if len(p) != 4 {
		return 0, fmt.Errorf("proto: bad exit payload length %d", len(p))
	}
	return int(int32(binary.BigEndian.Uint32(p))), nil
}
