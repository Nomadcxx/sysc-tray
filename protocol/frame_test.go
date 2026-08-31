package protocol

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func TestFramesHandleFragmentationAndPipelining(t *testing.T) {
	var wire bytes.Buffer
	for _, payload := range [][]byte{[]byte(`{"kind":"hello"}`), []byte(`{"kind":"snapshot"}`)} {
		if err := WriteFrame(&wire, payload); err != nil {
			t.Fatal(err)
		}
	}
	reader := &chunkReader{r: bytes.NewReader(wire.Bytes()), n: 1}
	for _, want := range []string{`{"kind":"hello"}`, `{"kind":"snapshot"}`} {
		got, err := ReadFrame(reader)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("ReadFrame() = %q, want %q", got, want)
		}
	}
}

func TestFramesRejectInvalidLengthsAndTruncation(t *testing.T) {
	for _, length := range []uint32{0, MaxFrameSize + 1} {
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], length)
		if _, err := ReadFrame(bytes.NewReader(header[:])); err == nil {
			t.Fatalf("ReadFrame() accepted length %d", length)
		}
	}
	if err := WriteFrame(io.Discard, nil); err == nil {
		t.Fatal("WriteFrame() accepted empty payload")
	}
	var wire bytes.Buffer
	if err := WriteFrame(&wire, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	data := wire.Bytes()
	if _, err := ReadFrame(bytes.NewReader(data[:len(data)-1])); err == nil {
		t.Fatal("ReadFrame() accepted truncation")
	}
}

func TestDecodeStrictRejectsAmbiguousJSON(t *testing.T) {
	for _, input := range [][]byte{
		[]byte(`{"kind":"hello"} {"kind":"hello"}`),
		[]byte(`{"kind":"hello","kind":"snapshot","payload":{}}`),
		[]byte(`{"kind":"hello","payload":{"major":1,"major":2}}`),
		[]byte(`{"kind":"hello","payload":{},"future":true}`),
		append([]byte(`{"kind":"`), 0xff),
	} {
		var envelope Envelope
		if err := DecodeStrict(input, &envelope); err == nil {
			t.Fatalf("DecodeStrict() accepted %q", input)
		}
	}
}

type chunkReader struct {
	r io.Reader
	n int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(p) > r.n {
		p = p[:r.n]
	}
	return r.r.Read(p)
}
