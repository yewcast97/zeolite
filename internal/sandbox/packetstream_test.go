package sandbox

import (
	"bytes"
	"io"
	"testing"
)

func TestPacketStreamCodec(t *testing.T) {
	packets := [][]byte{[]byte("one"), {}, bytes.Repeat([]byte{0xAB}, PacketMTU), bytes.Repeat([]byte{1}, MaxPacket)}
	var stream []byte
	for _, p := range packets {
		var err error
		if stream, err = AppendPacket(stream, p); err != nil {
			t.Fatal(err)
		}
	}
	r := bytes.NewReader(stream)
	buf := make([]byte, MaxPacket)
	for i, want := range packets {
		n, err := ReadPacket(r, buf)
		if err != nil || !bytes.Equal(buf[:n], want) {
			t.Fatalf("packet %d read back as %d bytes (%v), want %d", i, n, err, len(want))
		}
	}
	if _, err := ReadPacket(r, buf); err != io.EOF {
		t.Errorf("the end of the stream read as %v", err)
	}

	// A packet longer than any is, is not written, and a stream that
	// announces one is broken.
	if _, err := AppendPacket(nil, make([]byte, MaxPacket+1)); err != ErrPacketTooLong {
		t.Errorf("a packet too long was written: %v", err)
	}
	if _, err := ReadPacket(bytes.NewReader([]byte{0xFF, 0xFF, 1, 2, 3}), buf); err != ErrPacketTooLong {
		t.Errorf("a length too long read as %v", err)
	}
	// A stream that ends inside a packet did not end in order.
	for _, cut := range [][]byte{{0}, {0, 5, 1, 2}} {
		if _, err := ReadPacket(bytes.NewReader(cut), buf); err != io.ErrUnexpectedEOF {
			t.Errorf("a stream cut after %d bytes read as %v", len(cut), err)
		}
	}
	// The sandbox's own address is not one the internet has.
	if PublicAddress(SandboxAddress) {
		t.Errorf("%s is a public address", SandboxAddress)
	}
}
