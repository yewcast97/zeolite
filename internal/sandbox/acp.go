package sandbox

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// A sandbox's ACP Bridge passes the messages of the Agent Client
// Protocol between an ACP client on this Mac and an agent in the
// sandbox's workstation: one JSON value on a line of its own, each way,
// as on a sandbox's Link. A workstation is not trusted, so a line of
// either stream is bounded before it is read as JSON.

// ACPMaxLine bounds one message of the Agent Client Protocol as it
// arrives: a prompt may hold a picture, and no message is larger than
// a few of them.
const ACPMaxLine = 16 << 20

// ErrLineTooLong is why ReadLine gave no line: the stream's next line
// is longer than was allowed.
var ErrLineTooLong = errors.New("a line longer than is taken")

// ReadLine reads one line, without its end, and fails with
// ErrLineTooLong on one longer than max: it holds no more of a line
// than it was allowed to, and leaves the line's end unread, for
// SkipLine. It is how a sandbox's Link and an ACP Bridge read what
// arrives.
func ReadLine(in *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		part, err := in.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > max {
			if err == nil {
				in.UnreadByte()
			}
			return nil, fmt.Errorf("%w: more than %d bytes", ErrLineTooLong, max)
		}
		switch {
		case err == nil:
			return line[:len(line)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			return line, nil
		default:
			return nil, err
		}
	}
}

// SkipLine reads to the end of the line that ReadLine found too long,
// keeping none of it, so that the line after it can be read.
func SkipLine(in *bufio.Reader) error {
	for {
		_, err := in.ReadSlice('\n')
		if !errors.Is(err, bufio.ErrBufferFull) {
			return err
		}
	}
}
