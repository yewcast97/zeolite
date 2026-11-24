package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
)

// The supervisor is two MCP servers to an agent, each on standard
// streams: host, by which an agent runs Host Tools, and sandbox, by
// which it tells its user of its state. Both are an mcpServer: a name,
// what an agent is told when it meets the server, and its tools.
//
// What an agent is told, it is told here, and it is how to use a tool:
// in a server's instructions, and again in each tool's description,
// since a client may show its model the one and not the other. What is
// said of a tool is true of it. Nothing describes the sandbox beyond
// that, and no file is written into its Home: where it works, an agent
// finds out.

// mcpTool is one tool of a server.
type mcpTool struct {
	name string
	// description is what an agent's client is told of the tool. It
	// stands alone: a model that is shown nothing else of the server
	// knows from it what the tool does and when to call it.
	description string
	// schema is the tool's arguments, as JSON Schema.
	schema json.RawMessage
	// call answers one call: what the tool says, and whether that is an
	// error. Closing stop withdraws the call. An rpcError is for a call
	// that is none: arguments that are no arguments.
	call func(arguments json.RawMessage, stop <-chan struct{}) (said string, failed bool, rpcErr *rpcError)
}

// mcpServer is one MCP server of the supervisor's.
type mcpServer struct {
	name string
	// instructions are what a client is told of the server when it
	// meets it, for its model to read.
	instructions string
	tools        []mcpTool
}

// rpcMessage is a JSON-RPC message as an MCP client and server send it.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// noArguments is the schema of a tool that takes none.
var noArguments = json.RawMessage(`{"type": "object", "properties": {}}`)

// serve answers an MCP client on standard streams until the client is
// gone. Calls are answered as they end, side by side.
func (s mcpServer) serve(in io.Reader, out io.Writer) error {
	var (
		mu      sync.Mutex
		enc     = json.NewEncoder(out)
		calls   sync.WaitGroup
		running = map[string]chan struct{}{}
	)
	reply := func(id json.RawMessage, result any, rpcErr *rpcError) {
		mu.Lock()
		defer mu.Unlock()
		enc.Encode(rpcMessage{JSONRPC: "2.0", ID: id, Result: result, Error: rpcErr})
	}
	defer calls.Wait()

	lines := bufio.NewReaderSize(in, 64<<10)
	for {
		line, err := lines.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var msg rpcMessage
			if json.Unmarshal(line, &msg) != nil {
				reply(nil, nil, &rpcError{Code: -32700, Message: "not JSON"})
			} else {
				switch {
				case msg.Method == "notifications/cancelled":
					var p struct {
						RequestID json.RawMessage `json:"requestId"`
					}
					json.Unmarshal(msg.Params, &p)
					mu.Lock()
					if stop := running[string(p.RequestID)]; stop != nil {
						close(stop)
						delete(running, string(p.RequestID))
					}
					mu.Unlock()
				case msg.ID == nil:
					// Another notification: nothing is answered.
				case msg.Method == "initialize":
					var p struct {
						ProtocolVersion string `json:"protocolVersion"`
					}
					json.Unmarshal(msg.Params, &p)
					if p.ProtocolVersion == "" {
						p.ProtocolVersion = "2024-11-05"
					}
					reply(msg.ID, map[string]any{
						"protocolVersion": p.ProtocolVersion,
						"capabilities":    map[string]any{"tools": map[string]any{}},
						"serverInfo":      map[string]any{"name": s.name, "version": "1"},
						"instructions":    s.instructions,
					}, nil)
				case msg.Method == "ping":
					reply(msg.ID, map[string]any{}, nil)
				case msg.Method == "tools/list":
					tools := make([]any, len(s.tools))
					for i, tool := range s.tools {
						tools[i] = map[string]any{"name": tool.name, "description": tool.description, "inputSchema": tool.schema}
					}
					reply(msg.ID, map[string]any{"tools": tools}, nil)
				case msg.Method == "tools/call":
					var p struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					}
					if err := json.Unmarshal(msg.Params, &p); err != nil {
						reply(msg.ID, nil, &rpcError{Code: -32602, Message: "the call's arguments: " + err.Error()})
						break
					}
					var tool *mcpTool
					for i := range s.tools {
						if s.tools[i].name == p.Name {
							tool = &s.tools[i]
						}
					}
					if tool == nil {
						reply(msg.ID, nil, &rpcError{Code: -32602, Message: "the server has no tool " + p.Name})
						break
					}
					stop := make(chan struct{})
					mu.Lock()
					running[string(msg.ID)] = stop
					mu.Unlock()
					calls.Add(1)
					go func() {
						defer calls.Done()
						said, failed, rpcErr := tool.call(p.Arguments, stop)
						mu.Lock()
						_, wanted := running[string(msg.ID)]
						delete(running, string(msg.ID))
						mu.Unlock()
						// A call the client cancelled is not answered.
						switch {
						case !wanted:
						case rpcErr != nil:
							reply(msg.ID, nil, rpcErr)
						default:
							reply(msg.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": said}}, "isError": failed}, nil)
						}
					}()
				default:
					reply(msg.ID, nil, &rpcError{Code: -32601, Message: "the server has no method " + msg.Method})
				}
			}
		}
		if err != nil {
			// The client is gone: what it asked for is withdrawn.
			mu.Lock()
			for id, stop := range running {
				close(stop)
				delete(running, id)
			}
			mu.Unlock()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}
