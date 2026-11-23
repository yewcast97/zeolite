package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"zeolite/internal/sandbox"
)

// mcpCallSocket is where bridges send the calls they see, for the
// supervisor to report. It is a datagram socket: a bridge that cannot
// reach it loses the record and carries on.
var mcpCallSocket = filepath.Join(sandbox.SupervisorDir, "mcp-calls.sock")

// summaryLimit is how much of a message's payload is logged.
const summaryLimit = 240

func listenMCPCalls() (*net.UnixConn, error) {
	os.Remove(mcpCallSocket)
	return net.ListenUnixgram("unixgram", &net.UnixAddr{Name: mcpCallSocket, Net: "unixgram"})
}

// bridged is the command line of the MCP server of a name, and what is
// added to its environment: a pack's server as the supervisor was told
// of it, or this program as the server by which an agent tells its user
// of its state, or as the one by which it runs Host Tools, where the
// sandbox is given some. A name the sandbox has no
// server of is an error: an entry of an agent's registry names nothing
// else to run.
func bridged(cfg sandbox.SupervisorConfig, self, name string) (command []string, env map[string]string, err error) {
	for _, s := range cfg.MCPServers {
		if s.Name == name && len(s.Command) > 0 {
			return s.Command, s.Env, nil
		}
	}
	switch {
	case name == sandbox.StateServer:
		return []string{self, "state"}, nil, nil
	case name == sandbox.HostToolServer && cfg.HostTools != nil:
		return []string{self, "host-tools"}, nil, nil
	}
	return nil, nil, fmt.Errorf("this sandbox has no MCP server %q", name)
}

// mcpBridge runs the sandbox's MCP server of a name, which speaks over
// standard input and output, passing every line through untouched and
// logging what it is.
func mcpBridge(name string) error {
	cfg, err := readConfig(configPath)
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	command, env, err := bridged(cfg, self, name)
	if err != nil {
		return err
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = os.Environ()
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cmd.Env = append(cmd.Env, name+"="+env[name])
	}
	cmd.Stderr = os.Stderr
	toServer, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	fromServer, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	log, _ := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: mcpCallSocket, Net: "unixgram"})
	record := func(line []byte, fromAgent bool) {
		if log == nil {
			return
		}
		call := describeMCPMessage(line)
		call.Time, call.Server, call.FromAgent = time.Now(), name, fromAgent
		if data, err := json.Marshal(call); err == nil {
			log.Write(data)
		}
	}

	go func() {
		copyLines(toServer, os.Stdin, func(line []byte) { record(line, true) })
		toServer.Close()
	}()
	copyLines(os.Stdout, fromServer, func(line []byte) { record(line, false) })
	return cmd.Wait()
}

// copyLines copies src to dst a line at a time, showing each to see.
func copyLines(dst io.Writer, src io.Reader, see func(line []byte)) {
	r := bufio.NewReaderSize(src, 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			see(line)
			if _, werr := dst.Write(line); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// describeMCPMessage reads what a JSON-RPC message is: its method, and
// the start of its params, result or error.
func describeMCPMessage(line []byte) sandbox.MCPCall {
	var msg struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return sandbox.MCPCall{Summary: "not JSON-RPC: " + truncate(string(line))}
	}
	call := sandbox.MCPCall{Method: msg.Method}
	switch {
	case msg.Error != nil:
		call.Summary = "error " + truncate(string(msg.Error))
	case msg.Result != nil:
		call.Summary = truncate(string(msg.Result))
	case msg.Params != nil:
		call.Summary = truncate(string(msg.Params))
	}
	return call
}

func truncate(s string) string {
	r := []rune(s)
	if len(r) <= summaryLimit {
		return s
	}
	return string(r[:summaryLimit]) + "…"
}
