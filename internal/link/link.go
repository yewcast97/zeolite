// Package link is the app's end of a sandbox's Link: it reads the asks
// that arrive on the stream, hands each to what answers its kind, and
// writes the answers back.
//
// The stream comes from a workstation, which is not trusted: a line is
// bounded before it is read, an ask that is none ends the stream, and
// what an ask asks for is its handler's to check. The package runs
// nothing and opens nothing itself.
package link

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"zeolite/internal/sandbox"
)

// Handlers answer the asks of one sandbox's Link, by kind. A kind whose
// handler is nil the sandbox is not given: such an ask is refused.
type Handlers struct {
	// HostTool runs one Host Tool to its end, or until ctx is done: the
	// ask was withdrawn, or the Link ended.
	HostTool func(ctx context.Context, r sandbox.HostToolRequest) sandbox.HostToolResult
	// Hand takes note that the agent of a Terminal Session raised its
	// hand, and returns why it did not: the session is none the app
	// opened in this sandbox. Every sandbox has it.
	Hand func(r sandbox.HandRequest) (refused string)
}

// Serve answers the asks that arrive on a sandbox's Link until the
// stream ends: then everything an ask started is ended, and it returns
// why the stream ended. A sandbox's asks are worked on side by side.
func Serve(stream io.ReadWriteCloser, h Handlers) error {
	defer stream.Close()
	ctx, cancelAll := context.WithCancel(context.Background())
	var (
		asks    sync.WaitGroup
		mu      sync.Mutex // guards the stream's writes and asked
		out     = json.NewEncoder(stream)
		asked   = map[uint64]context.CancelFunc{}
		working = make(chan struct{}, sandbox.LinkMaxAsks)
	)
	// When the stream ends, so does every ask: nobody is left to hear
	// its answer.
	defer asks.Wait()
	defer cancelAll()
	answer := func(a sandbox.LinkAnswer) {
		mu.Lock()
		defer mu.Unlock()
		out.Encode(a)
	}

	in := bufio.NewReaderSize(stream, 64<<10)
	for {
		line, err := readLine(in, sandbox.LinkMaxLine)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var r sandbox.LinkRequest
		if err := json.Unmarshal(line, &r); err != nil {
			return fmt.Errorf("an ask that is not JSON: %w", err)
		}
		if r.Cancel {
			mu.Lock()
			if withdraw := asked[r.ID]; withdraw != nil {
				withdraw()
			}
			mu.Unlock()
			continue
		}
		work, refused := h.work(r)
		if refused != "" {
			answer(sandbox.LinkAnswer{ID: r.ID, Refused: refused})
			continue
		}
		select {
		case working <- struct{}{}:
		default:
			answer(sandbox.LinkAnswer{ID: r.ID, Refused: fmt.Sprintf("%d calls are being answered, which is as many as are at once: wait for one to end", sandbox.LinkMaxAsks)})
			continue
		}
		askCtx, withdraw := context.WithCancel(ctx)
		mu.Lock()
		asked[r.ID] = withdraw
		mu.Unlock()
		asks.Add(1)
		go func() {
			defer asks.Done()
			a := work(askCtx)
			a.ID = r.ID
			withdrawn := askCtx.Err() != nil
			mu.Lock()
			delete(asked, r.ID)
			mu.Unlock()
			withdraw()
			<-working
			// An ask that was withdrawn has nobody waiting.
			if !withdrawn {
				answer(a)
			}
		}()
	}
}

// work is what answers an ask, or why the ask is refused: it is of no
// kind, or of one the sandbox is not given. A refusal is read by an
// agent, as its tool's answer: it says what the agent can do about it,
// in the agent's words and none of the app's.
func (h Handlers) work(r sandbox.LinkRequest) (work func(context.Context) sandbox.LinkAnswer, refused string) {
	switch {
	case r.HostTool != nil:
		if h.HostTool == nil {
			return nil, "running programs on the Mac is not allowed here: ask your user to allow it"
		}
		request := *r.HostTool
		return func(ctx context.Context) sandbox.LinkAnswer {
			result := h.HostTool(ctx, request)
			return sandbox.LinkAnswer{HostTool: &result}
		}, ""
	case r.Hand != nil:
		if h.Hand == nil {
			return nil, "your user cannot be called from here"
		}
		request := *r.Hand
		return func(context.Context) sandbox.LinkAnswer {
			return sandbox.LinkAnswer{Refused: h.Hand(request)}
		}, ""
	}
	return nil, "a call of this kind is not answered"
}

// readLine reads one line, without its end, and fails on one longer
// than max.
func readLine(in *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		part, err := in.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > max {
			return nil, fmt.Errorf("an ask longer than %d bytes", max)
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
