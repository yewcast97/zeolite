package acpbridge

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// A path is in the Shared Folder by its text, cleaned: ".." leads out
// of nothing, a folder whose name only begins as the Shared Folder's is
// another folder, and what is no path from the root is nowhere.
func TestAPathIsMappedByItsText(t *testing.T) {
	f, err := newFolder(mac)
	if err != nil {
		t.Fatal(err)
	}
	for at, want := range map[string]string{
		"/workspace/shared":                 mac,
		"/workspace/shared/":                mac,
		"/workspace/shared/a.txt":           mac + "/a.txt",
		"/workspace/shared/a/../b":          mac + "/b",
		"/workspace/./shared//a":            mac + "/a",
		"/workspace/shared/..":              "",
		"/workspace/shared/../shared/a":     mac + "/a",
		"/workspace/shared/../.bashrc":      "",
		"/workspace/shared/a/../../.bashrc": "",
		"/workspace/shared-other/a":         "",
		"/workspace/sharedx":                "",
		"/workspace":                        "",
		"/":                                 "",
		"":                                  "",
		"workspace/shared/a":                "",
		"shared/a":                          "",
		"~/shared/a":                        "",
		"/workspace/shared/a\x00/../../b":   "",
		mac:                                 "",
		mac + "/a":                          "",
		"/Users/someone":                    "",
		"/workspace/shared/%2e%2e/a":        mac + "/%2e%2e/a", // a name, in a path that is no URI
		`/workspace/shared/..\..\a`:         mac + `/..\..\a`,  // one name, where a backslash parts nothing
	} {
		got, ok := f.toMac(at)
		if ok != (want != "") || got != want {
			t.Errorf("the workstation's %q is this Mac's %q (%v), want %q", at, got, ok, want)
		}
	}
	for at, want := range map[string]string{
		mac:                         there,
		mac + "/a b/c":              there + "/a b/c",
		mac + "/../research/a":      there + "/a",
		mac + "/../research-2/a":    "",
		mac + "-2/a":                "",
		mac + "/..":                 "",
		"/Users/Shared":             "",
		"/workspace/shared/a":       "",
		"/private" + mac + "/a":     "",
		strings.ToUpper(mac) + "/a": "",
	} {
		got, ok := f.toWorkstation(at)
		if ok != (want != "") || got != want {
			t.Errorf("this Mac's %q is the workstation's %q (%v), want %q", at, got, ok, want)
		}
	}
}

// A URI of the agent's reaches the client where it names a file in the
// Shared Folder, by this Mac's path, or a web address; nothing else
// does. What a URI writes as %2F is a separator, and a file on another
// host is no file here.
func TestAnAgentsURIAsTheClientIsGivenIt(t *testing.T) {
	f, _ := newFolder(mac)
	for uri, want := range map[string]string{
		"file:///workspace/shared/a.txt":                "file://" + mac + "/a.txt",
		"file:///workspace/shared/a%20b.txt#L4":         "file://" + mac + "/a%20b.txt#L4",
		"file://localhost/workspace/shared/a.txt":       "file://" + mac + "/a.txt",
		"file:///workspace/shared/a.txt?x=/etc/passwd":  "file://" + mac + "/a.txt",
		"FILE:///workspace/shared/a.txt":                "file://" + mac + "/a.txt",
		"file:///workspace/shared/..%2F.bashrc":         "",
		"file:///workspace/shared/%2e%2e/.bashrc":       "",
		"file:///workspace/shared/%2E%2E%2F%2E%2E/etc":  "",
		"file:///workspace/shared%2F..%2F..%2Fetc":      "",
		"file:///workspace/shared/../shared/../.bashrc": "",
		"file:///workspace/shared-other/a":              "",
		"file:///etc/passwd":                            "",
		"file://" + mac + "/a.txt":                      "",
		"file://server/workspace/shared/a.txt":          "",
		"file://workspace/shared/a.txt":                 "",
		"file:/workspace/shared/a.txt":                  "file://" + mac + "/a.txt",
		"file:workspace/shared/a.txt":                   "",
		"file:///workspace/shared/a%zz":                 "",
		"file:///workspace/shared/a%00b":                "",
		"/workspace/shared/a.txt":                       "",
		"a.txt":                                         "",
		"":                                              "",
		"https://example.com/a?b=c#d":                   "https://example.com/a?b=c#d",
		"http://127.0.0.1:8080/x":                       "http://127.0.0.1:8080/x",
		"https://user:pw@example.com/":                  "https://example.com/",
		"HTTPS://example.com":                           "https://example.com",
		"javascript:alert(1)":                           "",
		"data:text/html,<script>":                       "",
		"zed://settings":                                "",
		"vscode://file/etc/passwd":                      "",
		"smb://server/share":                            "",
		"x-apple.systempreferences:com.apple.x":         "",
		"ssh://host/x":                                  "",
		"ftp://example.com/a":                           "",
		"mailto:someone@example.com":                    "",
		" file:///workspace/shared/a.txt":               "",
	} {
		got, ok := f.uriToClient(uri)
		if ok != (want != "") || got != want {
			t.Errorf("the agent's %q is given to the client as %q (%v), want %q", uri, got, ok, want)
		}
	}
}

// A URI of the client's reaches the agent with no path of this Mac that
// is outside the Shared Folder: a file there is named by its own name
// alone, and of a URI that is the client's own kind, the kind.
func TestAClientsURIAsTheAgentIsGivenIt(t *testing.T) {
	f, _ := newFolder(mac)
	for uri, want := range map[string]string{
		"file://" + mac + "/a.txt":                                   "file:///workspace/shared/a.txt",
		"file://" + mac + "/dir/a%20b.txt#L1:9":                      "file:///workspace/shared/dir/a%20b.txt#L1:9",
		"file://localhost" + mac:                                     "file:///workspace/shared",
		"file://" + mac + "/a.txt?secret=/Users/someone":             "file:///workspace/shared/a.txt",
		mac + "/a.txt":                                               "/workspace/shared/a.txt",
		"file:///Users/someone/Documents/taxes%202026.pdf":           "taxes%202026.pdf",
		"file:///Users/someone/Documents/taxes.pdf#/Users/someone":   "taxes.pdf",
		"file://" + mac + "/../other/notes.md":                       "notes.md",
		"file://" + mac + "%2F..%2F..%2Fsecret.md":                   "secret.md",
		"file://" + mac + "-2/notes.md":                              "notes.md",
		"file://server/Users/someone/notes.md":                       "notes.md",
		"file:Users/someone/notes.md":                                "notes.md",
		"/Users/someone/notes.md":                                    "notes.md",
		"../someone/notes.md":                                        "notes.md",
		"notes.md":                                                   "notes.md",
		"file:///Users/someone/a%2Fb":                                "b",
		"file:///":                                                   "",
		"file:///Users/someone/..":                                   "Users",
		"":                                                           "",
		"file:///Users/someone/%zz":                                  "",
		"https://example.com/a?b=c#d":                                "https://example.com/a?b=c#d",
		"https://user:pw@example.com/":                               "https://example.com/",
		"zed:///agent/selection?path=/Users/someone/secret.txt#L1:2": "zed:",
		"zed://agent/thread/7?name=/Users/someone":                   "zed:",
		"untitled:Untitled-1":                                        "untitled:",
		"vscode-remote://ssh-remote+host/Users/someone/x":            "vscode-remote:",
		"data:text/plain;base64,L1VzZXJzL3NvbWVvbmU=":                "data:",
	} {
		got, ok := f.uriToAgent(uri)
		if !ok || got != want {
			t.Errorf("the client's %q is given to the agent as %q (%v), want %q", uri, got, ok, want)
		}
		if strings.Contains(got, "someone") || strings.Contains(got, "/Users") {
			t.Errorf("the client's %q reaches the agent with a path of this Mac: %q", uri, got)
		}
	}
}

// The package imports the core and the standard library's own text and
// encoding: it opens no file, makes no socket and starts no program,
// and has a path only as its text.
func TestThePackageReadsOnlyWhatCrosses(t *testing.T) {
	allowed := map[string]bool{
		"bufio": true, "bytes": true, "encoding/json": true, "errors": true, "fmt": true, "io": true,
		"net/url": true, "path": true, "strconv": true, "strings": true, "sync": true,
		"zeolite/internal/sandbox": true,
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files, seen := 0, map[string]bool{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		files++
		file, err := parser.ParseFile(token.NewFileSet(), e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			seen[p] = true
			if !allowed[p] {
				t.Errorf("%s imports %s: the package reads what crosses, and nothing else", e.Name(), p)
			}
		}
	}
	if files < 5 || !seen["net/url"] || !seen["path"] || !seen["zeolite/internal/sandbox"] {
		t.Fatalf("read %d files, which import %v: the test would prove little", files, seen)
	}
	for _, never := range []string{"os", "os/exec", "net", "net/http", "path/filepath", "io/fs", "syscall"} {
		if allowed[never] {
			t.Errorf("the package may import %s", never)
		}
	}
}

// crossedPaths collects, from a message that was written, every value
// the protocol reads as a path or as a URI, and reports a member named
// _meta anywhere.
func crossedPaths(t *testing.T, v any, name string, paths, uris *[]string) {
	t.Helper()
	switch v := v.(type) {
	case map[string]any:
		for member, value := range v {
			if member == "_meta" {
				t.Errorf("a member named _meta crossed: %v", value)
			}
			if member == "rawInput" || member == "rawOutput" || member == "data" {
				// An agent's own account, whose shape the protocol leaves
				// open: no member of it is read as a path. It holds no
				// _meta either.
				var none []string
				crossedPaths(t, value, "", &none, &none)
				continue
			}
			crossedPaths(t, value, member, paths, uris)
		}
	case []any:
		for _, value := range v {
			crossedPaths(t, value, name, paths, uris)
		}
	case string:
		switch name {
		case "cwd", "path", "additionalDirectories":
			*paths = append(*paths, v)
		case "uri":
			*uris = append(*uris, v)
		}
	}
}

// Whatever either side sends, what is written is one JSON object on a
// line; no member named _meta crosses; a path the client is given is in
// the Shared Folder as this Mac has it, and one the agent is given is in
// it as the workstation has it; and a file: URI crosses only so.
func FuzzOnlyTheSharedFolderCrosses(f *testing.F) {
	for _, seed := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":true},"_meta":{"terminal-auth":true}}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"` + mac + `/a","additionalDirectories":["` + mac + `/b"],"mcpServers":[],"_meta":{"x":1}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/Users/someone","mcpServers":[]}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/load","params":{"sessionId":"s","cwd":"` + mac + `/../x","mcpServers":[]}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/list","params":{"cwd":"` + mac + `"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s","prompt":[{"type":"resource_link","uri":"file:///Users/someone/a.txt","name":"a"},{"type":"resource","resource":{"uri":"file://` + mac + `/b.txt","text":"b"}},{"type":"image","data":"aGk=","mimeType":"image/png","uri":"/Users/someone/c.png"}]}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"sessions":[{"sessionId":"s","cwd":"/workspace/shared/a","additionalDirectories":["/etc"]},{"sessionId":"t","cwd":"/workspace"}]}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"authMethods":[{"type":"terminal","id":"t","name":"T"}],"_meta":{"x":1}}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"outcome":{"outcome":"selected","optionId":"yes"}}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"x","data":{"_meta":{"path":"/etc"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"c","title":"t","locations":[{"path":"/workspace/shared/a"},{"path":"/etc/passwd"}],"content":[{"type":"diff","path":"/workspace/shared/../x","newText":"n"},{"type":"content","content":{"type":"resource_link","uri":"file:///etc/passwd","name":"p"}}],"rawInput":{"_meta":1,"path":"/etc/passwd"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"resource","resource":{"uri":"file:///workspace/shared/a%2F..%2F..%2Fb","text":"x"}}}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/request_permission","params":{"sessionId":"s","toolCall":{"toolCallId":"c","locations":[{"path":"/workspace/shared/a"}]},"options":[{"optionId":"y","name":"Y","kind":"allow_once"}]}}`,
		`{"jsonrpc":"2.0","id":3,"method":"fs/read_text_file","params":{"sessionId":"s","path":"/etc/passwd"}}`,
		`{"jsonrpc":"2.0","id":4,"method":"terminal/create","params":{"sessionId":"s","command":"sh"}}`,
		`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s"}}`,
		`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":1}}`,
		`[{"jsonrpc":"2.0","id":1,"method":"logout"}]`,
		`{"id":1,"id":2,"method":"logout"}`,
		`not json`,
	} {
		for _, other := range []string{seed, `{"jsonrpc":"2.0","id":1,"method":"session/list","params":{}}`} {
			f.Add(seed, other)
			f.Add(other, seed)
		}
	}
	f.Fuzz(func(t *testing.T, first, second string) {
		// Each line from the client and then from the agent, and the
		// other way about: an answer finds the request it answers.
		for _, order := range [][2]string{{first, second}, {second, first}} {
			c := newCrossing(t)
			for i, line := range []string{order[0], order[1], order[0], order[1]} {
				var err error
				if (i+len(order[0]))%2 == 0 {
					err = c.b.clientLine([]byte(line))
				} else {
					err = c.b.agentLine([]byte(line))
				}
				if err != nil {
					t.Fatalf("%q ended the bridge: %v", line, err)
				}
			}
			for _, side := range []struct {
				name   string
				lines  []string
				folder string
			}{{"the client", c.lines(&c.toClient), mac}, {"the agent", c.lines(&c.toAgent), there}} {
				for _, line := range side.lines {
					var m map[string]any
					if err := json.Unmarshal([]byte(line), &m); err != nil || m["jsonrpc"] != "2.0" {
						t.Fatalf("%s is written %q", side.name, line)
					}
					var paths, uris []string
					crossedPaths(t, m, "", &paths, &uris)
					for _, p := range paths {
						if p != side.folder && !strings.HasPrefix(p, side.folder+"/") || strings.Contains(p, "/../") || strings.HasSuffix(p, "/..") {
							t.Errorf("%s is given the path %q, which is not in %s: %s", side.name, p, side.folder, line)
						}
					}
					for _, u := range uris {
						lower := strings.ToLower(u)
						inside := strings.HasPrefix(u, "file://"+side.folder+"/") || u == "file://"+side.folder || strings.HasPrefix(u, "file://"+side.folder+"#")
						if strings.HasPrefix(lower, "file:") && (!inside || strings.Contains(lower, "/../") || strings.Contains(lower, "%2f") || strings.Contains(lower, "%2e%2e")) {
							t.Errorf("%s is given the URI %q, which names no file in %s: %s", side.name, u, side.folder, line)
						}
						// What is written as a bare path is one in the
						// folder too.
						if strings.HasPrefix(u, "/") && u != side.folder && !strings.HasPrefix(u, side.folder+"/") {
							t.Errorf("%s is given the path %q as a URI: %s", side.name, u, line)
						}
						// The client is given a file in the Shared Folder
						// or a web address, and nothing else.
						if side.folder == mac && !inside && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
							t.Errorf("the client is given the URI %q: %s", u, line)
						}
					}
				}
			}
		}
	})
}
