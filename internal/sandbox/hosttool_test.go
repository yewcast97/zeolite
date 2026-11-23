package sandbox

import (
	"strings"
	"testing"
)

func TestAHostToolRequestIsChecked(t *testing.T) {
	ok := []HostToolRequest{
		{Argv: []string{"ffmpeg", "-i", "in.mov", "out.mp4"}},
		{Argv: []string{"/Applications/Blender.app/Contents/MacOS/Blender", "-b"}, Dir: "scenes/one", Seconds: HostToolMaxSeconds},
		{Argv: []string{"go", "build", "./..."}, Env: map[string]string{"GOFLAGS": "-mod=mod", "CGO_ENABLED": "1"}},
		{Argv: []string{"ls"}, Dir: "a/./b"},
	}
	for _, r := range ok {
		if err := r.Check(); err != nil {
			t.Errorf("%+v is refused: %v", r, err)
		}
	}

	long := make([]string, HostToolMaxArgs+1)
	for i := range long {
		long[i] = "x"
	}
	bad := []struct {
		r   HostToolRequest
		why string
	}{
		{HostToolRequest{}, "argv is empty"},
		{HostToolRequest{Argv: []string{""}}, "argv is empty"},
		{HostToolRequest{Argv: long}, "more than"},
		// env, which starts the program, would read these as its own.
		{HostToolRequest{Argv: []string{"-i", "ls"}}, "hyphen"},
		{HostToolRequest{Argv: []string{"-S", "ls -l"}}, "hyphen"},
		{HostToolRequest{Argv: []string{"DYLD_INSERT_LIBRARIES=/x.dylib", "ls"}}, "equals sign"},
		{HostToolRequest{Argv: []string{"ls", "a\x00b"}}, "NUL"},
		{HostToolRequest{Argv: []string{"ls", strings.Repeat("x", HostToolMaxArgBytes)}}, "larger than"},
		// What keeps a tool's files in its Scratch is not a request's.
		{HostToolRequest{Argv: []string{"ls"}, Env: map[string]string{"HOME": "/Users/someone"}}, "every Host Tool"},
		{HostToolRequest{Argv: []string{"ls"}, Env: map[string]string{"TMPDIR": "/tmp"}}, "every Host Tool"},
		{HostToolRequest{Argv: []string{"ls"}, Env: map[string]string{"PATH": "/x"}}, "every Host Tool"},
		{HostToolRequest{Argv: []string{"ls"}, Env: map[string]string{HostToolCacheEnv: "/x"}}, "every Host Tool"},
		// Nor is where bash and zsh find the Shell Startup File.
		{HostToolRequest{Argv: []string{"ls"}, Env: map[string]string{"BASH_ENV": "/workspace/shared/mine"}}, "every Host Tool"},
		{HostToolRequest{Argv: []string{"ls"}, Env: map[string]string{"ZDOTDIR": "/workspace/shared"}}, "every Host Tool"},
		{HostToolRequest{Argv: []string{"ls"}, Env: map[string]string{"DYLD_INSERT_LIBRARIES": "/x.dylib"}}, "loader"},
		{HostToolRequest{Argv: []string{"ls"}, Env: map[string]string{"A B": "x"}}, "cannot name"},
		{HostToolRequest{Argv: []string{"ls"}, Env: map[string]string{"A": "x\x00y"}}, "NUL"},
		{HostToolRequest{Argv: []string{"ls"}, Seconds: -1}, "seconds"},
		{HostToolRequest{Argv: []string{"ls"}, Seconds: HostToolMaxSeconds + 1}, "seconds"},
		// A folder is named from the Shared Folder, and stays in it.
		{HostToolRequest{Argv: []string{"ls"}, Dir: "/Users"}, "slash"},
		{HostToolRequest{Argv: []string{"ls"}, Dir: ".."}, "steps out"},
		{HostToolRequest{Argv: []string{"ls"}, Dir: "a/../../b"}, "steps out"},
		{HostToolRequest{Argv: []string{"ls"}, Dir: "-C"}, "hyphen"},
		{HostToolRequest{Argv: []string{"ls"}, Dir: "a\x00"}, "NUL"},
	}
	for _, c := range bad {
		if err := c.r.Check(); err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%.80q: %v, want an error about %q", c.r.Argv, err, c.why)
		}
	}
}

// Host Tools run in a Shared Folder: a sandbox that has none has no Host
// Tools, and nothing is added to a Wall no tool runs under.
func TestHostToolsNeedASharedFolder(t *testing.T) {
	said := func(c Config) string {
		for _, e := range c.Validate() {
			if e.Field == FieldHostTools {
				return e.Message
			}
		}
		return ""
	}
	cfg := gated()
	if cfg.HostTools != (HostTools{}) {
		t.Errorf("a new sandbox has Host Tools: %+v", cfg.HostTools)
	}
	cfg.HostTools.Enabled = true
	if got := said(cfg); !strings.Contains(got, "Shared Folder") {
		t.Errorf("Host Tools with no Shared Folder: %q", got)
	}
	cfg.SharedFolder = "research"
	cfg.HostTools.GPU, cfg.HostTools.MacName = true, true
	if got := said(cfg); got != "" {
		t.Errorf("Host Tools in a Shared Folder: %q", got)
	}
	cfg.HostTools.Enabled = false
	if got := said(cfg); !strings.Contains(got, "does not have") {
		t.Errorf("the GPU for Host Tools the sandbox does not have: %q", got)
	}
}
