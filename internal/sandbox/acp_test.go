package sandbox

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

// A line is read without its end, whatever the reader's buffer holds of
// it at a time, and the last line needs no end.
func TestReadLine(t *testing.T) {
	in := bufio.NewReaderSize(strings.NewReader("one\n\n"+strings.Repeat("x", 100)+"\nlast"), 16)
	for _, want := range []string{"one", "", strings.Repeat("x", 100), "last"} {
		line, err := ReadLine(in, 200)
		if err != nil || string(line) != want {
			t.Fatalf("read %q (%v), want %q", line, err, want)
		}
	}
	if _, err := ReadLine(in, 200); !errors.Is(err, io.EOF) {
		t.Errorf("after the last line: %v", err)
	}
}

// A line longer than is allowed is not held: reading it fails, at once,
// and the line after it is read once it was skipped, whether the long
// line's end was in the buffer or not.
func TestALineTooLongIsNotHeldAndCanBeSkipped(t *testing.T) {
	for _, long := range []int{9, 17, 40, 4000} {
		in := bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", long)+"\nnext\n"), 16)
		if line, err := ReadLine(in, 8); !errors.Is(err, ErrLineTooLong) || line != nil {
			t.Fatalf("a line of %d bytes, with 8 allowed: %q, %v", long, line, err)
		}
		if err := SkipLine(in); err != nil {
			t.Fatalf("skipping a line of %d bytes: %v", long, err)
		}
		if line, err := ReadLine(in, 8); err != nil || string(line) != "next" {
			t.Errorf("after a line of %d bytes was skipped: %q, %v", long, line, err)
		}
	}
	// A line of just what is allowed is read: its end counts with it.
	in := bufio.NewReaderSize(strings.NewReader("1234567\n12345678\n"), 16)
	if line, err := ReadLine(in, 8); err != nil || string(line) != "1234567" {
		t.Errorf("a line of 8 bytes with its end: %q, %v", line, err)
	}
	if _, err := ReadLine(in, 8); !errors.Is(err, ErrLineTooLong) {
		t.Errorf("a line of 9 bytes with its end: %v", err)
	}
	// A stream that ends in a line too long ends there.
	in = bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", 100)), 16)
	if _, err := ReadLine(in, 8); !errors.Is(err, ErrLineTooLong) {
		t.Errorf("a last line too long: %v", err)
	}
	if err := SkipLine(in); !errors.Is(err, io.EOF) {
		t.Errorf("skipping a last line too long: %v", err)
	}
	// A message of the Agent Client Protocol may hold a picture.
	if ACPMaxLine != 16<<20 {
		t.Errorf("a message of the Agent Client Protocol is bounded at %d bytes", ACPMaxLine)
	}
}
