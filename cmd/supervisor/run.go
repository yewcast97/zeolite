package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"zeolite/internal/sandbox"
)

const (
	// mcpTimeout is how long an agent is told to wait for an MCP server,
	// where its registry has a place to say so: one call can take
	// minutes, where an agent would by itself give up after seconds.
	mcpTimeout = 30 * time.Minute
	// reportInterval is how often the workstation's status is reported.
	reportInterval = 2 * time.Second
	// restartDelay is how long a daemon that exited stays down.
	restartDelay = 2 * time.Second
)

// daemon is a pack's DaemonProcess and what is known of it.
type daemon struct {
	sandbox.Daemon

	mu      sync.Mutex
	process *os.Process
	lastErr string
}

func run() error {
	var cfg sandbox.SupervisorConfig
	if raw := os.Getenv(sandbox.SupervisorConfigEnv); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return fmt.Errorf("%s is not JSON: %w", sandbox.SupervisorConfigEnv, err)
		}
	}
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	// What the supervisor was told, where its other commands find it: an
	// agent may start an MCP server with an environment of its own
	// making, which holds none of this.
	if err := writeConfig(configPath, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "supervisor:", err)
	}
	for _, err := range prepareHome(homeFiles{
		home:          sandbox.HomePath,
		ownRegistries: sandbox.SupervisorDir,
		sharedFolder:  sandbox.SharedFolderPath,
	}, self, cfg) {
		// The workstation runs all the same: a Home that is full must
		// still be entered, to make room in it.
		fmt.Fprintln(os.Stderr, "supervisor:", err)
	}

	out := json.NewEncoder(os.Stdout)
	var outMu sync.Mutex
	report := func(r sandbox.SupervisorReport) {
		outMu.Lock()
		defer outMu.Unlock()
		out.Encode(r)
	}

	calls, err := listenMCPCalls()
	if err != nil {
		return err
	}
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := calls.Read(buf)
			if err != nil {
				return
			}
			var call sandbox.MCPCall
			if json.Unmarshal(buf[:n], &call) == nil {
				report(sandbox.SupervisorReport{MCPCall: &call})
			}
		}
	}()

	// As PID 1 the supervisor inherits every orphan in the workstation,
	// so one loop reaps all children and tells the daemons' goroutines
	// about their own.
	exits := newExitTable()
	go exits.reap()

	daemons := make([]*daemon, len(cfg.Daemons))
	for i, d := range cfg.Daemons {
		daemons[i] = &daemon{Daemon: d}
		go daemons[i].keepRunning(exits)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	ticker := time.NewTicker(reportInterval)
	defer ticker.Stop()
	for {
		status := sandbox.WorkstationStatus{Clock: time.Now().Format(time.RFC3339)}
		for _, d := range daemons {
			status.Daemons = append(status.Daemons, d.state())
		}
		status.Sessions = sessionStates()
		status.MemoryUsed, status.MemoryLimit = memoryIn(controlGroup)
		report(sandbox.SupervisorReport{Status: &status})
		select {
		case <-ticker.C:
		case <-stop:
			for _, d := range daemons {
				d.terminate()
			}
			return nil
		}
	}
}

// keepRunning starts the daemon, and again each time it exits.
func (d *daemon) keepRunning(exits *exitTable) {
	for {
		cmd := exec.Command(d.Command[0], d.Command[1:]...)
		cmd.Dir = sandbox.HomePath
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		// Its own process group, so that terminate reaches its children.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		done := exits.start(cmd)
		if done == nil {
			d.set(nil, "it did not start")
		} else {
			d.set(cmd.Process, "")
			status := <-done
			d.set(nil, "it exited: "+status)
		}
		time.Sleep(restartDelay)
	}
}

func (d *daemon) set(p *os.Process, err string) {
	d.mu.Lock()
	d.process, d.lastErr = p, err
	d.mu.Unlock()
}

// state is whether the daemon accepts connections on its readiness port.
func (d *daemon) state() sandbox.DaemonState {
	d.mu.Lock()
	running, lastErr := d.process != nil, d.lastErr
	d.mu.Unlock()
	s := sandbox.DaemonState{Pack: d.Pack}
	if !running {
		s.Error = lastErr
		return s
	}
	c, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(d.ReadinessPort)), 500*time.Millisecond)
	if err != nil {
		s.Error = "not accepting connections yet"
		return s
	}
	c.Close()
	s.Ready = true
	return s
}

func (d *daemon) terminate() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.process != nil {
		syscall.Kill(-d.process.Pid, syscall.SIGTERM)
	}
}

// exitTable tells whoever started a child when it exits. Children are
// reaped in one place because wait4(-1) takes any child, including the
// ones a goroutine is waiting for.
type exitTable struct {
	mu      sync.Mutex
	waiting map[int]chan string
}

func newExitTable() *exitTable { return &exitTable{waiting: map[int]chan string{}} }

// start starts cmd and returns a channel that gets its exit status, or
// nil when it did not start.
func (t *exitTable) start(cmd *exec.Cmd) <-chan string {
	// Held across Start so that the reaper cannot see the exit before
	// the process is registered.
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return nil
	}
	done := make(chan string, 1)
	t.waiting[cmd.Process.Pid] = done
	return done
}

func (t *exitTable) reap() {
	sig := make(chan os.Signal, 16)
	signal.Notify(sig, syscall.SIGCHLD)
	for range sig {
		for {
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
			if err != nil || pid <= 0 {
				break
			}
			t.mu.Lock()
			done := t.waiting[pid]
			delete(t.waiting, pid)
			t.mu.Unlock()
			if done != nil {
				if status.Signaled() {
					done <- "signal " + status.Signal().String()
				} else {
					done <- "status " + strconv.Itoa(status.ExitStatus())
				}
			}
		}
	}
}

// homeFiles are the places in a sandbox's Home that the supervisor
// writes, and the ones it tidies.
type homeFiles struct {
	// home is the home of the agent's user, under which the agents keep
	// their MCP registries.
	home string
	// ownRegistries is where the registries are that the app writes
	// whole, for the agents that are sent to one by a variable.
	ownRegistries string
	// sharedFolder is where the workstation mounts a Shared Folder.
	sharedFolder string
}

// serverNames are the names of the MCP servers the sandbox has: the one
// by which an agent tells its user of its state, which every sandbox
// has; its packs'; and the one by which an agent runs Host Tools where
// the sandbox is given some.
func serverNames(cfg sandbox.SupervisorConfig) []string {
	names := []string{sandbox.StateServer}
	for _, s := range cfg.MCPServers {
		names = append(names, s.Name)
	}
	if cfg.HostTools != nil {
		names = append(names, sandbox.HostToolServer)
	}
	return names
}

// prepareHome writes into the sandbox's Home what every start writes
// there, which is the agents' MCP registries, and returns what could not
// be written: one file's failure does not stop the next, and none stops
// the workstation. What an agent is told, it is told by MCP: no file
// that tells it is written here.
//
// Where the sandbox has no Shared Folder, it removes the empty folder
// one left behind: Docker makes the mountpoint in the Home and leaves
// it there, where it would look like a Shared Folder. A folder that
// holds anything is somebody's, and stays.
func prepareHome(f homeFiles, self string, cfg sandbox.SupervisorConfig) []error {
	errs := writeRegistries(f.home, f.ownRegistries, self, serverNames(cfg))
	if !cfg.SharedFolder {
		// Rmdir removes an empty directory and nothing else.
		if err := syscall.Rmdir(f.sharedFolder); err != nil && !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) && !errors.Is(err, syscall.ENOTDIR) {
			errs = append(errs, fmt.Errorf("removing the empty %s: %w", f.sharedFolder, err))
		}
	}
	return errs
}

// configPath is where the supervisor keeps what it was told, for its
// other commands to read.
var configPath = filepath.Join(sandbox.SupervisorDir, "config.json")

// writeConfig keeps what the supervisor was told where its other
// commands find it.
func writeConfig(file string, cfg sandbox.SupervisorConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	return os.WriteFile(file, data, 0o600)
}

// readConfig reads what the supervisor was told.
func readConfig(file string) (sandbox.SupervisorConfig, error) {
	var cfg sandbox.SupervisorConfig
	data, err := os.ReadFile(file)
	if err != nil {
		return cfg, fmt.Errorf("what this sandbox's supervisor was told is not there: %w", err)
	}
	return cfg, json.Unmarshal(data, &cfg)
}
