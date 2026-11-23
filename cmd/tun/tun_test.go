package main

import (
	"bytes"
	"go/parser"
	"go/token"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zeolite/internal/sandbox"
)

// device stands in for a TUN device: what the namespace sends is read
// from it a packet at a time, and what is written to it is a packet the
// namespace receives.
type device struct {
	sends    chan []byte
	received chan []byte
}

func (d *device) Read(p []byte) (int, error) {
	packet, ok := <-d.sends
	if !ok {
		return 0, io.ErrClosedPipe
	}
	return copy(p, packet), nil
}

func (d *device) Write(p []byte) (int, error) {
	d.received <- append([]byte(nil), p...)
	return len(p), nil
}

func TestPacketsCrossBetweenTheDeviceAndTheStream(t *testing.T) {
	d := &device{sends: make(chan []byte, 8), received: make(chan []byte, 8)}
	fromApp, appWrites := io.Pipe()
	appReads, toApp := io.Pipe()
	ended := make(chan error, 1)
	go func() { ended <- pump(d, fromApp, toApp) }()

	// What the namespace sends reaches the stream, each packet behind its
	// length.
	buf := make([]byte, sandbox.MaxPacket)
	for _, packet := range [][]byte{[]byte("a packet"), bytes.Repeat([]byte{7}, sandbox.PacketMTU), {}} {
		d.sends <- packet
		n, err := sandbox.ReadPacket(appReads, buf)
		if err != nil || !bytes.Equal(buf[:n], packet) {
			t.Fatalf("a packet of %d bytes reached the stream as %d (%v)", len(packet), n, err)
		}
	}
	// And what the stream carries reaches the device, a packet at a time.
	var frames []byte
	for _, packet := range []string{"first", "second"} {
		frames, _ = sandbox.AppendPacket(frames, []byte(packet))
	}
	go appWrites.Write(frames)
	for _, want := range []string{"first", "second"} {
		select {
		case got := <-d.received:
			if string(got) != want {
				t.Errorf("the device received %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a packet off the stream did not reach the device")
		}
	}

	// When the app's side of the stream ends, tun ends.
	appWrites.Close()
	select {
	case err := <-ended:
		if err != io.EOF {
			t.Errorf("the stream's end stopped tun with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tun outlived its stream")
	}
}

func TestTunEndsWhenEitherSideDoes(t *testing.T) {
	for what, end := range map[string]func(d *device, appWrites *io.PipeWriter, appReads *io.PipeReader){
		"the device is gone":      func(d *device, _ *io.PipeWriter, _ *io.PipeReader) { close(d.sends) },
		"nobody reads the stream": func(d *device, _ *io.PipeWriter, appReads *io.PipeReader) { appReads.Close(); d.sends <- []byte("x") },
		"the stream is cut in a packet": func(_ *device, appWrites *io.PipeWriter, _ *io.PipeReader) {
			appWrites.Write([]byte{0, 9, 1})
			appWrites.Close()
		},
		"the stream holds what is no packet": func(_ *device, appWrites *io.PipeWriter, _ *io.PipeReader) { appWrites.Write([]byte{0xff, 0xff}) },
	} {
		d := &device{sends: make(chan []byte, 8), received: make(chan []byte, 8)}
		fromApp, appWrites := io.Pipe()
		appReads, toApp := io.Pipe()
		ended := make(chan error, 1)
		go func() { ended <- pump(d, fromApp, toApp) }()
		go end(d, appWrites, appReads)
		select {
		case err := <-ended:
			if err == nil {
				t.Errorf("%s: tun stopped without saying why", what)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s: tun went on", what)
		}
	}
}

// tun moves packets and opens no connection: it has no code that could.
func TestTunOpensNoConnection(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		files++
		for _, imp := range file.Imports {
			switch path := strings.Trim(imp.Path.Value, `"`); {
			case path == "net" || strings.HasPrefix(path, "net/") || path == "os/exec" || path == "syscall":
				t.Errorf("%s imports %s", name, path)
			}
		}
	}
	if files < 4 {
		t.Fatalf("read %d files: the test would prove little", files)
	}
}
