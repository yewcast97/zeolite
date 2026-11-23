package hosttool

import (
	"os"
	"strings"
	"time"

	"zeolite/internal/sandbox"
)

// capture keeps the start of what a Host Tool writes on one of its
// streams, and drops the rest. The app reads none of it: it is sent back
// to the agent as it came.
type capture struct {
	r, w *os.File
	kept []byte
	// more says that the tool wrote more than is kept.
	more bool
	done chan struct{}
}

func newCapture() *capture { return &capture{done: make(chan struct{})} }

// open makes the pipe the tool writes to.
func (c *capture) open() (err error) {
	c.r, c.w, err = os.Pipe()
	return err
}

// abandon closes a pipe whose tool did not start.
func (c *capture) abandon() {
	if c.r != nil {
		c.r.Close()
		c.w.Close()
	}
}

// started is called once the tool holds the pipe's writing end: the app
// closes its own, and keeps what comes until every holder is gone.
func (c *capture) started() {
	c.w.Close()
	go func() {
		defer close(c.done)
		defer c.r.Close()
		buf := make([]byte, 32<<10)
		for {
			n, err := c.r.Read(buf)
			if room := sandbox.HostToolMaxOutput - len(c.kept); n > room {
				c.kept = append(c.kept, buf[:room]...)
				c.more = true
			} else {
				c.kept = append(c.kept, buf[:n]...)
			}
			if err != nil {
				return
			}
		}
	}()
}

// leftBehind is how long the app waits, after a run's process group was
// ended, for the stream to end with it.
const leftBehind = 2 * time.Second

// text is what was kept, once the tool's every process is gone. A
// process that left the run's group is not ended with it and may hold
// the stream open: the app then stops listening, and what that process
// writes later goes nowhere. What is not text is replaced, since it is
// sent on as a JSON string.
func (c *capture) text() string {
	select {
	case <-c.done:
	case <-time.After(leftBehind):
		c.r.Close()
		<-c.done
	}
	return strings.ToValidUTF8(string(c.kept), "\uFFFD")
}
