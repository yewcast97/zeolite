package pack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zeolite/internal/sandbox"
)

// renderTool is a user's pack.
const renderTool = `
name: render-tool
title: Render Tool
daemonProcess:
  command: [render-server, --port, "9876"]
  readinessPort: 9876
mcpBridge:
  name: render
  command: [render-mcp]
`

// named finds a pack of a catalog by its name.
func named(r *Catalog, name string) (Manifest, bool) {
	for _, m := range r.All() {
		if m.Name == name {
			return m, true
		}
	}
	return Manifest{}, false
}

func TestBuiltinPacksAreValid(t *testing.T) {
	r, err := NewCatalog()
	if err != nil {
		t.Fatal(err)
	}
	m, ok := named(r, "web-test-browser")
	if !ok {
		t.Fatalf("no web-test-browser among %d built-in packs", len(r.All()))
	}
	if m.ViewportStream.Type != StreamCDP {
		t.Errorf("its stream is %q", m.ViewportStream.Type)
	}
}

// No built-in pack installs an agent: an agent is its user's to install
// in the workstation, where the app finds it.
func TestNoBuiltinPackInstallsAnAgent(t *testing.T) {
	r, err := NewCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range r.All() {
		for _, a := range sandbox.KnownAgents {
			if m.Name == a.Executable || strings.EqualFold(m.Title, a.Title) {
				t.Errorf("the built-in pack %s is the agent %s", m.Name, a.Title)
			}
		}
	}
}

// A manifest cannot ask for anything to run on the Mac: the keys that
// did are gone, and unknown keys are errors.
func TestNoManifestRunsOnTheMac(t *testing.T) {
	for what, y := range map[string]string{
		"runsOn": "name: x\ntitle: X\ndaemonProcess: {runsOn: host, command: [x], readinessPort: 80}\n",
		"app":    "name: x\ntitle: X\ndaemonProcess: {command: [x], readinessPort: 80, app: /Applications/X.app}\n",
		"files":  "name: x\ntitle: X\ndaemonProcess: {command: [x], readinessPort: 80, files: {a: b}}\n",
	} {
		if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), what) {
			t.Errorf("a daemonProcess with %s: error %v, want one naming the key", what, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name, yaml, want string
	}{
		{"a misspelled key", "name: x\ntitle: X\ndeamonProcess: {}\n", "deamonProcess"},
		{"an upper-case name", "name: Web\ntitle: X\n", "name"},
		{"no title", "name: x\n", "title"},
		{"a package with a shell character", "name: x\ntitle: X\nruntimeLayer: {packages: ['curl; rm -rf /']}\n", "package name"},
		{"an env the app owns", "name: x\ntitle: X\nruntimeLayer: {env: {TZ: UTC}}\n", "set by the app"},
		{"a relative volume path", "name: x\ntitle: X\nruntimeLayer: {volumes: [{name: ue, path: opt/ue}]}\n", "absolute path"},
		{"a sandbox's Home for a volume", "name: x\ntitle: X\nruntimeLayer: {volumes: [{name: zeolite-s1-home, path: /opt/home}]}\n", "Homes"},
		{"a daemon without a command", "name: x\ntitle: X\ndaemonProcess: {readinessPort: 80}\n", "command"},
		{"a daemon without a port", "name: x\ntitle: X\ndaemonProcess: {command: [x]}\n", "readinessPort"},
		{"an image file with a relative path", "name: x\ntitle: X\nruntimeLayer: {files: {'lib/x.py': y}}\n", "absolute path"},
		{"a stream without a daemon", "name: x\ntitle: X\nviewportStream: {type: cdp_screencast, port: 9222}\n", "daemonProcess"},
		{"an unknown stream type", "name: x\ntitle: X\nviewportStream: {type: rdp}\n", "stream type"},
		{"a stream type the app never showed", "name: x\ntitle: X\ndaemonProcess: {command: [x], readinessPort: 5900}\nviewportStream: {type: vnc, port: 5900}\n", "stream type"},
	}
	for _, tt := range tests {
		_, err := Parse([]byte(tt.yaml))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error %v, want one mentioning %q", tt.name, err, tt.want)
		}
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("render.yaml", renderTool)
	write("shadow.yaml", "name: web-test-browser\ntitle: Impostor\n")
	write("broken.yaml", "name: [\n")

	r, _ := NewCatalog()
	errs := r.LoadDir(dir)
	if len(errs) != 2 {
		t.Errorf("errors %v, want one for the shadowing pack and one for the broken file", errs)
	}
	if _, ok := named(r, "render-tool"); !ok {
		t.Error("the valid user pack was not added")
	}
	if m, _ := named(r, "web-test-browser"); m.Title != "Web Test Browser" {
		t.Errorf("a user pack replaced a built-in one: title %q", m.Title)
	}
	if errs := r.LoadDir(filepath.Join(dir, "missing")); errs != nil {
		t.Errorf("a missing directory gave %v", errs)
	}
}

func TestCheck(t *testing.T) {
	r, _ := NewCatalog()
	for _, y := range []string{
		renderTool,
		"name: other-browser\ntitle: Other Browser\ndaemonProcess: {command: [x], readinessPort: 9222}\nmcpBridge: {name: browser, command: [x]}\n",
	} {
		if err := r.add("test", []byte(y)); err != nil {
			t.Fatal(err)
		}
	}
	messages := func(c sandbox.Config) string {
		var s []string
		for _, e := range r.Check(c) {
			if e.Field != sandbox.FieldPacks {
				t.Errorf("error on field %q", e.Field)
			}
			s = append(s, e.Message)
		}
		return strings.Join(s, "\n")
	}

	c := sandbox.NewConfig("s1", "S")
	c.Packs = []string{"web-test-browser", "render-tool"}
	if got := messages(c); got != "" {
		t.Errorf("a valid selection gave %q", got)
	}
	if got := r.Enabled(c); len(got) != 2 || got[1].Name != "render-tool" {
		t.Errorf("enabled packs %v", got)
	}

	c = sandbox.NewConfig("s1", "S")
	c.Packs = []string{"web-test-browser", "other-browser", "ghost"}
	got := messages(c)
	for _, want := range []string{"both use port 9222", `MCP server named "browser"`, `no Capability Pack named "ghost"`} {
		if !strings.Contains(got, want) {
			t.Errorf("errors %q lack %q", got, want)
		}
	}
}
