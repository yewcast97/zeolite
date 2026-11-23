package sandbox

import (
	"regexp"
	"strings"
	"testing"
)

func TestKnownAgents(t *testing.T) {
	name := regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	titles, executables := map[string]bool{}, map[string]bool{}
	for _, a := range KnownAgents {
		if a.Title == "" || titles[a.Title] || executables[a.Executable] {
			t.Errorf("%+v has no title, or is listed twice", a)
		}
		titles[a.Title], executables[a.Executable] = true, true
		// A known agent is known by the name of its program, wherever a
		// workstation has it: a plain word, and no path.
		if !name.MatchString(a.Executable) {
			t.Errorf("%s: its executable %q is no plain name", a.Title, a.Executable)
		}
	}
	// Each is found by its executable, and nothing else is.
	for _, a := range KnownAgents {
		if got, ok := KnownAgent(a.Executable); !ok || got != a {
			t.Errorf("%s is not found by its executable: %+v", a.Title, got)
		}
	}
	for _, other := range []string{"", "Claude Code", "/usr/local/bin/claude", "claude ", "vi", "bash", "node"} {
		if got, ok := KnownAgent(other); ok {
			t.Errorf("%q is taken for the agent %s", other, got.Title)
		}
	}
	// Each has a registry of its own: a file under the home, or one of
	// the app's own that a variable names; no two agents share a file.
	files := map[string]string{}
	for _, a := range KnownAgents {
		r := a.Registry
		if other, twice := files[r.File]; twice || r.File == "" {
			t.Errorf("%s keeps its registry in %q, as %s does", a.Title, r.File, other)
		}
		files[r.File] = a.Title
		if r.Servers == "" || (r.Format != RegistryJSON && r.Format != RegistryTOML) {
			t.Errorf("%s: its registry is %+v", a.Title, r)
		}
		own := strings.HasPrefix(r.File, SupervisorDir+"/")
		switch {
		case own != (r.Variable != ""):
			t.Errorf("%s: a registry of the app's own is named by a variable, and no other is: %+v", a.Title, r)
		case !own && (strings.HasPrefix(r.File, "/") || strings.Contains(r.File, "..")):
			t.Errorf("%s: its registry %q is not under the home", a.Title, r.File)
		}
		if (r.TimeoutKey == "") != (r.TimeoutUnit == 0) {
			t.Errorf("%s: a timeout has a key and a unit, or neither: %+v", a.Title, r)
		}
		if r.Format == RegistryTOML && (r.CommandList || r.Enabled || r.Type != "") {
			t.Errorf("%s: a TOML registry's server is its command and its arguments: %+v", a.Title, r)
		}
	}
	if got := RegistryEnvironment(); len(got) != 1 || got["OPENCODE_CONFIG"] != "/tmp/zeolite/opencode.json" {
		t.Errorf("a workstation's environment names the registries %v", got)
	}
	// The agents known, in their order.
	var known []string
	for _, a := range KnownAgents {
		known = append(known, a.Title+"="+a.Executable)
	}
	const want = "Claude Code=claude Codex=codex Grok=grok OpenCode=opencode Kimi Code=kimi Pi=omp Antigravity=agy"
	if got := strings.Join(known, " "); got != want {
		t.Errorf("the known agents are\n%s\nwant\n%s", got, want)
	}
}
