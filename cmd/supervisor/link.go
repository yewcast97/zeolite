package main

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"

	"zeolite/internal/sandbox"
)

// A sandbox's Link is reached from its workstation in two steps. The
// app runs `supervisor link` in the workstation, and its standard
// streams are the Link: asks go out on it, and answers come back. The
// link listens on a socket of the workstation's own, a file under /tmp.
// Whatever asks something of the app there, as the MCP server by which
// an agent runs Host Tools does, connects to that socket, writes its
// ask and waits for its answer: one connection is one ask, and closing
// it withdraws the ask.

// linkSocket is where the link listens.
var linkSocket = filepath.Join(sandbox.SupervisorDir, "link.sock")

// link carries the asks that arrive on the socket to the app, on out,
// and each answer that comes back on in to whoever asked. It returns
// when in ends: the sandbox is stopping.
func link(socket string, in io.Reader, out io.Writer) error {
	os.Remove(socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(socket)
	// The workstation's user, and nobody else there is.
	os.Chmod(socket, 0o600)

	var (
		mu      sync.Mutex
		next    uint64
		waiting = map[uint64]chan sandbox.LinkAnswer{}
		enc     = json.NewEncoder(out)
	)
	send := func(r sandbox.LinkRequest) {
		mu.Lock()
		defer mu.Unlock()
		enc.Encode(r)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var r sandbox.LinkRequest
				if err := json.NewDecoder(io.LimitReader(conn, sandbox.LinkMaxLine)).Decode(&r); err != nil {
					return
				}
				answer := make(chan sandbox.LinkAnswer, 1)
				mu.Lock()
				next++
				r.ID, r.Cancel = next, false
				waiting[r.ID] = answer
				enc.Encode(r)
				mu.Unlock()

				// Whoever asked says nothing more: when its end of the
				// connection closes, it has stopped waiting.
				left := make(chan struct{})
				go func() {
					io.Copy(io.Discard, conn)
					close(left)
				}()
				select {
				case a := <-answer:
					json.NewEncoder(conn).Encode(a)
				case <-left:
					mu.Lock()
					delete(waiting, r.ID)
					mu.Unlock()
					send(sandbox.LinkRequest{ID: r.ID, Cancel: true})
				}
			}()
		}
	}()

	answers := json.NewDecoder(in)
	for {
		var a sandbox.LinkAnswer
		if err := answers.Decode(&a); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		mu.Lock()
		answer := waiting[a.ID]
		delete(waiting, a.ID)
		mu.Unlock()
		if answer != nil {
			answer <- a
		}
	}
}

// ask hands one ask to the link and waits for its answer. Closing stop
// withdraws it.
func ask(socket string, r sandbox.LinkRequest, stop <-chan struct{}) (sandbox.LinkAnswer, error) {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return sandbox.LinkAnswer{}, errors.New("this sandbox's Link to the app is not open")
	}
	defer conn.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-stop:
			conn.Close()
		case <-done:
		}
	}()
	if err := json.NewEncoder(conn).Encode(r); err != nil {
		return sandbox.LinkAnswer{}, err
	}
	var a sandbox.LinkAnswer
	if err := json.NewDecoder(conn).Decode(&a); err != nil {
		return sandbox.LinkAnswer{}, errors.New("the ask was ended before the app answered it")
	}
	return a, nil
}
