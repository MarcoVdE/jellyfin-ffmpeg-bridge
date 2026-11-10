package proto

import (
	"bytes"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, Stderr, []byte("frame=  10 fps=25")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buf, Exit, ExitPayload(-1)); err != nil {
		t.Fatal(err)
	}
	typ, p, err := ReadFrame(&buf)
	if err != nil || typ != Stderr || string(p) != "frame=  10 fps=25" {
		t.Fatalf("got %d %q %v", typ, p, err)
	}
	typ, p, err = ReadFrame(&buf)
	if err != nil || typ != Exit {
		t.Fatalf("got %d %v", typ, err)
	}
	code, err := ParseExit(p)
	if err != nil || code != -1 {
		t.Fatalf("exit code %d %v", code, err)
	}
}

func TestReadFrameRejectsOversized(t *testing.T) {
	hdr := []byte{Stdout, 0xff, 0xff, 0xff, 0xff}
	if _, _, err := ReadFrame(bytes.NewReader(hdr)); err != ErrFrameTooLarge {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}
}

func TestSendSplitsLargePayloads(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	big := bytes.Repeat([]byte{'x'}, MaxFrame+10)
	if err := w.Send(Stdout, big); err != nil {
		t.Fatal(err)
	}
	total := 0
	for buf.Len() > 0 {
		_, p, err := ReadFrame(&buf)
		if err != nil {
			t.Fatal(err)
		}
		total += len(p)
	}
	if total != len(big) {
		t.Fatalf("got %d bytes, want %d", total, len(big))
	}
}

func TestSignVerify(t *testing.T) {
	nonce := []byte("0123456789abcdef0123456789abcdef")
	mac := Sign("secret", nonce)
	if !Verify("secret", nonce, mac) {
		t.Fatal("valid MAC rejected")
	}
	if Verify("other", nonce, mac) {
		t.Fatal("wrong token accepted")
	}
	if Verify("secret", []byte("different nonce"), mac) {
		t.Fatal("wrong nonce accepted")
	}
	if Verify("", nonce, Sign("", nonce)) {
		t.Fatal("empty token must never verify")
	}
	if Verify("secret", nonce, "not-hex") {
		t.Fatal("garbage MAC accepted")
	}
}
