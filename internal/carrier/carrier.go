// Package carrier is the app's side of a sandbox's Carrier. It holds
// the Carrier's program, starts one for a sandbox under the Carrier
// Wall, hands it the sandbox's Packet stream as opaque bytes, passes on
// what the gatekeeper says of the gate, and hears which Interface is in
// use.
//
// The app itself reads no packet and makes no connection for a sandbox:
// the Carrier does, in a process of its own that can do nothing else of
// this Mac. If that process ends, the sandbox's packets go nowhere.
//
// The Carrier and its wall are built from cmd/carrier by `go generate`
// and embedded here, so the app carries them.
package carrier

//go:generate env GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/carrier-arm64 ../../cmd/carrier
//go:generate env GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/carrier-amd64 ../../cmd/carrier
//go:generate cp ../../cmd/carrier/carrier.sb bin/carrier.sb

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"

	"zeolite/internal/sandbox"
)

//go:embed bin/*
var embedded embed.FS

// sandboxExec is the system's program that starts another under a
// Seatbelt profile.
const sandboxExec = "/usr/bin/sandbox-exec"

// Program is the Carrier's program on this Mac's disk, with its wall.
type Program struct {
	path string
	wall string
}

// Install writes the Carrier's program under the app's data directory,
// or finds it there as this build of the app has it.
func Install(dataDir string) (*Program, error) {
	binary, err := embedded.ReadFile("bin/carrier-" + runtime.GOARCH)
	if err != nil {
		return nil, errors.New("no Carrier built for this Mac: run `go generate ./internal/carrier`")
	}
	wall, err := embedded.ReadFile("bin/carrier.sb")
	if err != nil {
		return nil, errors.New("the Carrier Wall is not built in: run `go generate ./internal/carrier`")
	}
	if _, err := os.Stat(sandboxExec); err != nil {
		return nil, fmt.Errorf("this Mac has no %s: a Carrier is never run without the Carrier Wall", sandboxExec)
	}
	dir := filepath.Join(dataDir, "bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "carrier")
	// Seatbelt names the program by the path it resolves to.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		path = filepath.Join(resolved, "carrier")
	}
	if have, err := os.ReadFile(path); err != nil || !bytes.Equal(have, binary) {
		tmp := path + ".tmp"
		os.Remove(tmp)
		if err := os.WriteFile(tmp, binary, 0o755); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, path); err != nil {
			return nil, err
		}
	}
	if err := os.Chmod(path, 0o755); err != nil {
		return nil, err
	}
	return &Program{path: path, wall: string(wall)}, nil
}

// Carrier is one sandbox's Carrier, running.
type Carrier struct {
	cmd     *exec.Cmd
	control *os.File
	stream  io.Closer

	mu      sync.Mutex
	stopped bool
	done    chan struct{}
}

// Events are what a Carrier tells the app. Each is called from a
// goroutine of the Carrier's.
type Events struct {
	// Report is called when the Interface in use changes.
	Report func(sandbox.CarrierReport)
	// Said is called with each line the Carrier writes for the log.
	Said func(line string)
	// Ended is called once, when the Carrier's process is gone: by Stop,
	// or by itself, which is the end of the sandbox's way out.
	Ended func(err error)
}

// Start runs a Carrier for one sandbox, under the Carrier Wall, and
// gives it the sandbox's Packet stream. The stream's bytes pass through
// the app unread, both ways.
func (p *Program) Start(cfg sandbox.CarrierConfig, stream io.ReadWriteCloser, events Events) (*Carrier, error) {
	first, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	reportsRead, reportsWrite, err := os.Pipe()
	if err != nil {
		controlRead.Close()
		controlWrite.Close()
		return nil, err
	}
	cmd := exec.Command(sandboxExec, "-D", "CARRIER="+p.path, "-p", p.wall, p.path)
	// The Carrier is told nothing of the app's own environment.
	cmd.Env = []string{}
	cmd.Dir = "/"
	cmd.ExtraFiles = []*os.File{controlRead, reportsWrite} // descriptors 3 and 4
	toCarrier, err := cmd.StdinPipe()
	if err == nil {
		var fromCarrier io.ReadCloser
		if fromCarrier, err = cmd.StdoutPipe(); err == nil {
			var said io.ReadCloser
			if said, err = cmd.StderrPipe(); err == nil {
				if err = cmd.Start(); err == nil {
					c := &Carrier{cmd: cmd, control: controlWrite, stream: stream, done: make(chan struct{})}
					controlRead.Close()
					reportsWrite.Close()
					go c.run(first, stream, toCarrier, fromCarrier, said, reportsRead, events)
					return c, nil
				}
			}
		}
	}
	for _, f := range []*os.File{controlRead, controlWrite, reportsRead, reportsWrite} {
		f.Close()
	}
	return nil, fmt.Errorf("starting the Carrier: %w", err)
}

// run moves the Packet stream between the sandbox and the Carrier, and
// hears the Carrier, until its process is gone.
func (c *Carrier) run(first []byte, stream io.ReadWriteCloser, toCarrier io.WriteCloser, fromCarrier, said io.Reader, reports *os.File, events Events) {
	c.control.Write(append(first, '\n'))
	var copies sync.WaitGroup
	copies.Add(4)
	go func() {
		defer copies.Done()
		io.Copy(toCarrier, stream)
		// The sandbox's end of the stream is gone: so is what it carried.
		toCarrier.Close()
	}()
	go func() {
		defer copies.Done()
		io.Copy(stream, fromCarrier)
		// The Carrier's end is gone: the sandbox's device goes with it.
		stream.Close()
	}()
	go func() {
		defer copies.Done()
		lines := bufio.NewScanner(said)
		for lines.Scan() {
			if events.Said != nil {
				events.Said(lines.Text())
			}
		}
	}()
	go func() {
		defer copies.Done()
		defer reports.Close()
		lines := bufio.NewScanner(reports)
		for lines.Scan() {
			var r sandbox.CarrierReport
			if json.Unmarshal(lines.Bytes(), &r) == nil && events.Report != nil {
				events.Report(r)
			}
		}
	}()
	copies.Wait()
	err := c.cmd.Wait()
	c.mu.Lock()
	if c.stopped {
		err = nil
	}
	c.mu.Unlock()
	c.control.Close()
	close(c.done)
	if events.Ended != nil {
		events.Ended(err)
	}
}

// Judge passes on what the gatekeeper last said of the gate.
func (c *Carrier) Judge(j sandbox.CarrierJudgement) {
	line, err := json.Marshal(j)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.stopped {
		c.control.Write(append(line, '\n'))
	}
}

// Stop ends the Carrier: it resets every connection it holds and exits.
// The sandbox's Packet stream is closed.
func (c *Carrier) Stop() {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		<-c.done
		return
	}
	c.stopped = true
	c.mu.Unlock()
	// The end of what the app tells it is how the Carrier is told to end.
	c.control.Close()
	c.stream.Close()
	<-c.done
}
