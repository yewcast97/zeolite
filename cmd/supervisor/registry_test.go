package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"zeolite/internal/sandbox"
)

const testSelf = "/usr/local/bin/supervisor"

// homeOf is a home of a test's, with a folder for the registries that
// are the app's own, and the registries written into it.
type homeOf struct {
	t         *testing.T
	home, own string
}

func newHome(t *testing.T) *homeOf {
	dir := t.TempDir()
	return &homeOf{t: t, home: filepath.Join(dir, "home"), own: filepath.Join(dir, "own")}
}

func (h *homeOf) write(servers ...string) []error {
	return writeRegistries(h.home, h.own, testSelf, servers)
}

func (h *homeOf) path(executable string) string {
	a, ok := sandbox.KnownAgent(executable)
	if !ok {
		h.t.Fatalf("%s is no agent the app knows", executable)
	}
	if a.Registry.Variable != "" {
		return filepath.Join(h.own, filepath.Base(a.Registry.File))
	}
	return filepath.Join(h.home, a.Registry.File)
}

func (h *homeOf) read(executable string) string {
	h.t.Helper()
	data, err := os.ReadFile(h.path(executable))
	if err != nil {
		h.t.Fatalf("the registry of %s: %v", executable, err)
	}
	return string(data)
}

func (h *homeOf) put(executable, text string) {
	h.t.Helper()
	file := h.path(executable)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// Every agent the app knows finds the sandbox's servers in its own
// registry, in the shape its maker gave it: each server is this program
// as the bridge, with the server's name.
func TestEveryAgentsRegistryIsWritten(t *testing.T) {
	h := newHome(t)
	if errs := h.write("browser", "host"); len(errs) != 0 {
		t.Fatal(errs)
	}
	// The JSON registries: the server under its name, with the program
	// and its arguments apart.
	for executable, wantType := range map[string]string{"claude": "stdio", "kimi": "", "omp": "", "agy": ""} {
		var registry struct {
			MCPServers map[string]map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(h.read(executable)), &registry); err != nil {
			t.Fatalf("%s: %v", executable, err)
		}
		for _, name := range []string{"browser", "host"} {
			entry := registry.MCPServers[name]
			args, _ := entry["args"].([]any)
			kind, _ := entry["type"].(string)
			if entry["command"] != testSelf || len(args) != 2 || args[0] != "mcp-bridge" || args[1] != name || kind != wantType {
				t.Errorf("%s: its server %s is %v", executable, name, entry)
			}
			if _, has := entry["type"]; has != (wantType != "") {
				t.Errorf("%s: its server %s says its type: %v", executable, name, has)
			}
		}
		if len(registry.MCPServers) != 2 {
			t.Errorf("%s has %d servers", executable, len(registry.MCPServers))
		}
	}
	// OpenCode's: one command line, switched on, with minutes to wait.
	var opencode struct {
		MCP map[string]struct {
			Type    string
			Command []string
			Enabled bool
			Timeout int64
		}
	}
	if err := json.Unmarshal([]byte(h.read("opencode")), &opencode); err != nil {
		t.Fatal(err)
	}
	if got := opencode.MCP["host"]; got.Type != "local" || strings.Join(got.Command, " ") != testSelf+" mcp-bridge host" || !got.Enabled || got.Timeout <= int64(sandbox.HostToolMaxSeconds)*1000 || len(opencode.MCP) != 2 {
		t.Errorf("OpenCode's registry is %+v: the agent must wait longer than a run may take", opencode.MCP)
	}
	// The TOML registries: a table for each server, and Codex is told
	// how long to wait.
	for executable, timed := range map[string]bool{"codex": true, "grok": false} {
		var registry struct {
			Servers map[string]struct {
				Command string
				Args    []string
				Timeout int64 `toml:"tool_timeout_sec"`
			} `toml:"mcp_servers"`
		}
		if _, err := toml.Decode(h.read(executable), &registry); err != nil {
			t.Fatalf("%s: %v", executable, err)
		}
		got := registry.Servers["browser"]
		if got.Command != testSelf || strings.Join(got.Args, " ") != "mcp-bridge browser" || len(registry.Servers) != 2 || (got.Timeout > int64(sandbox.HostToolMaxSeconds)) != timed || (got.Timeout != 0) != timed {
			t.Errorf("%s: its registry is %+v", executable, registry.Servers)
		}
	}
	// A new registry is its user's alone.
	for _, a := range sandbox.KnownAgents {
		if info, err := os.Stat(h.path(a.Executable)); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: its registry is %v (%v)", a.Title, info, err)
		}
	}
	// Written again, nothing changes; with one server fewer, that one
	// goes from every registry, by what its entry is: no list is kept.
	before := map[string]string{}
	for _, a := range sandbox.KnownAgents {
		before[a.Executable] = h.read(a.Executable)
	}
	if errs := h.write("browser", "host"); len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, a := range sandbox.KnownAgents {
		if h.read(a.Executable) != before[a.Executable] {
			t.Errorf("%s: written again, its registry changed", a.Title)
		}
	}
	if errs := h.write("browser"); len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, a := range sandbox.KnownAgents {
		if text := h.read(a.Executable); strings.Contains(text, `"host"`) || strings.Contains(text, ".host]") || !strings.Contains(text, "browser") {
			t.Errorf("%s: with one server fewer its registry is:\n%s", a.Title, text)
		}
	}
}

// What an agent and its user keep in a JSON registry stays as it is:
// every other key and server keeps its place and its text, and a server
// of the user's own that has a name of the app's is not written over.
func TestAJSONRegistryKeepsWhatIsNotTheApps(t *testing.T) {
	h := newHome(t)
	const theirs = `{
  "numStartups": 12,
  "oauthAccount": {"emailAddress": "someone@example.com",   "odd  spacing": [1,2 ,3]},
  "mcpServers": {
    "mine": {"command": "npx", "args": ["-y", "my-server"], "env": {"TOKEN": "t"}},
    "stale": {"type":"stdio","command":"/usr/local/bin/supervisor","args":["mcp-bridge","stale"]},
    "host": {"command": "my-own-host"}
  },
  "projects": {"/workspace": {"allowedTools": []}}
}
`
	h.put("claude", theirs)
	os.Chmod(h.path("claude"), 0o640)
	errs := h.write("browser", "host")
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "servers of its own named host") {
		t.Fatalf("a server of the user's under a name of the app's: %v", errs)
	}
	got := h.read("claude")
	for _, kept := range []string{
		`"numStartups": 12,`,
		`"oauthAccount": {"emailAddress": "someone@example.com",   "odd  spacing": [1,2 ,3]},`,
		`"mine": {"command": "npx", "args": ["-y", "my-server"], "env": {"TOKEN": "t"}},`,
		`"host": {"command": "my-own-host"},`,
		`"projects": {"/workspace": {"allowedTools": []}}`,
	} {
		if !strings.Contains(got, kept) {
			t.Errorf("the registry lost %s:\n%s", kept, got)
		}
	}
	if strings.Contains(got, "stale") || !strings.Contains(got, `"browser": {"args":["mcp-bridge","browser"],"command":"/usr/local/bin/supervisor","type":"stdio"}`) {
		t.Errorf("the app's servers in the registry are:\n%s", got)
	}
	// The keys keep their order, and the file its mode.
	if !(strings.Index(got, "numStartups") < strings.Index(got, "oauthAccount") && strings.Index(got, "oauthAccount") < strings.Index(got, "mcpServers") && strings.Index(got, "mcpServers") < strings.Index(got, "projects")) {
		t.Errorf("the keys changed places:\n%s", got)
	}
	if info, _ := os.Stat(h.path("claude")); info.Mode().Perm() != 0o640 {
		t.Errorf("the registry's mode is %o", info.Mode().Perm())
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Errorf("the registry is no JSON any more: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(h.path("claude"))); len(entries) > len(sandbox.KnownAgents) {
		t.Errorf("a file was left beside the registry: %v", entries)
	}

	// A registry that has no servers yet gets them, after what it holds.
	h.put("kimi", `{"theme": "dark"}`)
	if errs := h.write("browser"); len(errs) != 0 {
		t.Fatal(errs)
	}
	if got := h.read("kimi"); !strings.HasPrefix(got, "{\n  \"theme\": \"dark\",\n  \"mcpServers\": {\n    \"browser\": ") {
		t.Errorf("the registry is:\n%s", got)
	}
}

// A registry that is not strict JSON, or whose servers are no object, is
// left untouched, and said: the other agents' are written all the same.
func TestARegistryThatDoesNotReadIsLeftAsItIs(t *testing.T) {
	for name, text := range map[string]string{
		"comments":           "{\n  // my servers\n  \"mcpServers\": {}\n}\n",
		"a trailing comma":   `{"mcpServers": {},}`,
		"no object":          `["mcpServers"]`,
		"servers in a list":  `{"mcpServers": []}`,
		"two values":         `{"a": 1} {"b": 2}`,
		"half a file":        `{"mcpServers": {"mine": {"command": "x"`,
		"TOML that is wrong": "",
	} {
		h := newHome(t)
		executable, file := "claude", text
		if name == "TOML that is wrong" {
			executable, file = "codex", "model = \"gpt\"\n[mcp_servers.mine\ncommand = 1\n"
		}
		h.put(executable, file)
		errs := h.write("browser")
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), "is left as it is") || !strings.Contains(errs[0].Error(), filepath.Base(h.path(executable))) {
			t.Errorf("%s: %v", name, errs)
		}
		if got := h.read(executable); got != file {
			t.Errorf("%s: the registry was changed to:\n%s", name, got)
		}
		if !strings.Contains(h.read("kimi"), "browser") || !strings.Contains(h.read("grok"), "browser") {
			t.Errorf("%s: the other agents' registries were not written", name)
		}
	}
}

// What an agent and its user keep in a TOML registry stays as it is,
// line for line, its comments with it: the app's tables are dropped and
// put again at the end, and nothing else is touched.
func TestATOMLRegistryKeepsWhatIsNotTheApps(t *testing.T) {
	h := newHome(t)
	const theirs = `# my Codex
model = "gpt-5"   # the big one
approval_policy = "never"

[mcp_servers.mine]
command = "npx"
args = ["-y", "my-server"]   # mine

[mcp_servers.stale]
command = "/usr/local/bin/supervisor"
args = ["mcp-bridge", "stale"]
tool_timeout_sec = 1800

[mcp_servers.mine.env]
TOKEN = "t"

[profiles.fast]
model = "gpt-5-mini"
notes = """
[mcp_servers.not-a-table]
command = "/usr/local/bin/supervisor"
"""
`
	h.put("codex", theirs)
	if errs := h.write("host", "browser"); len(errs) != 0 {
		t.Fatal(errs)
	}
	got := h.read("codex")
	want := strings.Replace(theirs, "[mcp_servers.stale]\ncommand = \"/usr/local/bin/supervisor\"\nargs = [\"mcp-bridge\", \"stale\"]\ntool_timeout_sec = 1800\n\n", "", 1) +
		"\n[mcp_servers.browser]\ncommand = \"/usr/local/bin/supervisor\"\nargs = [\"mcp-bridge\", \"browser\"]\ntool_timeout_sec = 1800\n" +
		"\n[mcp_servers.host]\ncommand = \"/usr/local/bin/supervisor\"\nargs = [\"mcp-bridge\", \"host\"]\ntool_timeout_sec = 1800\n"
	if got != want {
		t.Errorf("the registry is:\n%s\nwant:\n%s", got, want)
	}
	// Written again it is the same, and with one server fewer that
	// server's table is gone, and the user's own text is as it was.
	if errs := h.write("host", "browser"); len(errs) != 0 || h.read("codex") != want {
		t.Errorf("written again the registry changed (%v)", errs)
	}
	if errs := h.write("host"); len(errs) != 0 {
		t.Fatal(errs)
	}
	if got := h.read("codex"); strings.Contains(got, "[mcp_servers.browser]") || !strings.Contains(got, "[mcp_servers.not-a-table]\ncommand = \"/usr/local/bin/supervisor\"\n\"\"\"") || !strings.Contains(got, "[mcp_servers.mine.env]\nTOKEN = \"t\"") {
		t.Errorf("with one server fewer the registry is:\n%s", got)
	}

	// A server of the user's own under a name of the app's: writing the
	// app's would say the table twice, so the registry is left as it is.
	const taken = "[mcp_servers.host]\ncommand = \"my-own-host\"\n"
	h.put("grok", taken)
	errs := h.write("host")
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "config.toml is left as it is") || h.read("grok") != taken {
		t.Errorf("a table of the user's under a name of the app's: %v\n%s", errs, h.read("grok"))
	}
	// A name that is no bare word is written as a string.
	h2 := newHome(t)
	if errs := h2.write("my server"); len(errs) != 0 {
		t.Fatal(errs)
	}
	if got := h2.read("grok"); got != "[mcp_servers.\"my server\"]\ncommand = \"/usr/local/bin/supervisor\"\nargs = [\"mcp-bridge\", \"my server\"]\n" {
		t.Errorf("the registry is:\n%s", got)
	}
}

// The bridge starts the sandbox's server of the name it is given, as
// the supervisor was told of it, and nothing else: a name is all an
// agent's registry holds.
func TestTheBridgeFindsItsServerByName(t *testing.T) {
	file := filepath.Join(t.TempDir(), "run", "config.json")
	cfg := sandbox.SupervisorConfig{
		MCPServers: []sandbox.MCPServer{{Name: "browser", Command: []string{"mcp-server-playwright", "--cdp-endpoint", "http://127.0.0.1:9222"}, Env: map[string]string{"DEBUG": "1"}}},
		HostTools:  &sandbox.HostToolsNotice{GPU: true},
	}
	if err := writeConfig(file, cfg); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("what the supervisor was told is kept as %v (%v)", info, err)
	}
	read, err := readConfig(file)
	if err != nil || len(read.MCPServers) != 1 || read.HostTools == nil || !read.HostTools.GPU {
		t.Fatalf("what the supervisor was told reads back as %+v (%v)", read, err)
	}
	command, env, err := bridged(read, testSelf, "browser")
	if err != nil || strings.Join(command, " ") != "mcp-server-playwright --cdp-endpoint http://127.0.0.1:9222" || env["DEBUG"] != "1" {
		t.Errorf("the pack's server is %q with %v (%v)", command, env, err)
	}
	command, env, err = bridged(read, testSelf, "host")
	if err != nil || strings.Join(command, " ") != testSelf+" host-tools" || len(env) != 0 {
		t.Errorf("the Host Tools' server is %q with %v (%v)", command, env, err)
	}
	// Every sandbox has the server by which an agent tells of its state.
	if command, env, err := bridged(sandbox.SupervisorConfig{}, testSelf, "sandbox"); err != nil || strings.Join(command, " ") != testSelf+" state" || len(env) != 0 {
		t.Errorf("the state server is %q with %v (%v)", command, env, err)
	}
	if got := strings.Join(serverNames(read), " "); got != "sandbox browser host" {
		t.Errorf("the sandbox's servers are %q", got)
	}
	if got := strings.Join(serverNames(sandbox.SupervisorConfig{}), " "); got != "sandbox" {
		t.Errorf("a sandbox with no pack and no Host Tools has the servers %q", got)
	}
	for _, name := range []string{"", "nothing", "browser; id", "../browser"} {
		if command, _, err := bridged(read, testSelf, name); err == nil {
			t.Errorf("the bridge would start %q for %q", command, name)
		}
	}
	// A sandbox that is given no Host Tools has no such server.
	if command, _, err := bridged(sandbox.SupervisorConfig{}, testSelf, "host"); err == nil {
		t.Errorf("with no Host Tools the bridge would start %q", command)
	}
	if _, err := readConfig(filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Error("what the supervisor was told was read where there is none")
	}
}
