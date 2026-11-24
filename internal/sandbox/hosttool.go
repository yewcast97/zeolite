package sandbox

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// HostToolServer is the name of the MCP server by which a sandbox's
// agent runs Host Tools, and HostToolRun the name of its one tool. The
// server is named for what it is to an agent, a place where a program
// runs under macOS. No Capability Pack's server has that name.
const (
	HostToolServer = "macos"
	HostToolRun    = "run"
)

// What bounds a Host Tool's run.
const (
	// HostToolMaxArgs and HostToolMaxArgBytes bound a request's command
	// line and what it adds to the environment, together.
	HostToolMaxArgs     = 256
	HostToolMaxArgBytes = 64 << 10
	// HostToolDefaultSeconds is how long a run may take when its request
	// does not say, and HostToolMaxSeconds the most a request may ask
	// for: less than an agent's MCP client waits for an answer.
	HostToolDefaultSeconds = 600
	HostToolMaxSeconds     = 1500
	// HostToolMaxOutput is how much of what a run printed, on each of
	// its two streams, is sent back: the start of it. A tool that prints
	// more writes it to a file in the Shared Folder.
	HostToolMaxOutput = 256 << 10
	// HostToolMaxRuns is how many of a sandbox's Host Tools run at once.
	HostToolMaxRuns = 4
)

// HostToolRequest asks for one run of a Host Tool. It crosses from a
// workstation to the app in a LinkRequest, on the sandbox's Link.
type HostToolRequest struct {
	// Argv is the program and its arguments. The program is a name the
	// Wall's PATH finds, or a path.
	Argv []string `json:"argv,omitempty"`
	// Dir is the folder the program starts in, inside the Shared Folder
	// and named from it: empty for the Shared Folder itself.
	Dir string `json:"dir,omitempty"`
	// Env is added to the environment every Host Tool has.
	Env map[string]string `json:"env,omitempty"`
	// Seconds is how long the run may take, HostToolDefaultSeconds where
	// it is zero.
	Seconds int `json:"seconds,omitempty"`
}

// HostToolResult answers a HostToolRequest.
type HostToolResult struct {
	// Refused is why the program was not run, empty where it was.
	Refused string `json:"refused,omitempty"`
	// ExitCode is the program's exit status, or -1 where a signal ended
	// it: the Wall's own, when the run took too long.
	ExitCode int `json:"exitCode"`
	// TimedOut says that the run was ended for taking too long.
	TimedOut bool `json:"timedOut,omitempty"`
	// Stdout and Stderr are the start of what the program printed.
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	// Truncated says that the program printed more than was sent back.
	Truncated bool `json:"truncated,omitempty"`
}

// hostToolFixedEnvironment are the variables every Host Tool is given.
// A request does not set them: they keep a tool's files in the Host Tool
// Scratch, and its programs those the Wall lets it run. The last two
// name the Shell Startup File to bash and to zsh, where the sandbox is
// given the GPU.
var hostToolFixedEnvironment = map[string]bool{
	"HOME": true, "TMPDIR": true, "PATH": true, "XDG_CACHE_HOME": true, "TZ": true, "LANG": true,
	"DEVELOPER_DIR": true, "SDKROOT": true, HostToolCacheEnv: true,
	"BASH_ENV": true, "ZDOTDIR": true,
}

// HostToolCacheEnv names the variable by which the GPU Shim is told
// where the user's cache folder is: a folder of the Host Tool Scratch.
const HostToolCacheEnv = "ZEOLITE_USER_CACHE_DIR"

// Check reports why a request cannot be run, or nil. It is checked in
// the workstation, to tell the agent at once, and again by the app,
// which trusts nothing a workstation sends.
func (r HostToolRequest) Check() error {
	if len(r.Argv) == 0 || r.Argv[0] == "" {
		return errors.New("argv is empty: it is the program and its arguments")
	}
	if len(r.Argv) > HostToolMaxArgs {
		return fmt.Errorf("argv has more than %d arguments", HostToolMaxArgs)
	}
	// The program is handed to env, which would take a leading hyphen
	// for an option of its own and an equals sign for a variable.
	if strings.HasPrefix(r.Argv[0], "-") || strings.Contains(r.Argv[0], "=") {
		return errors.New("the program's name begins with a hyphen or holds an equals sign: set variables in env")
	}
	size := 0
	for _, arg := range r.Argv {
		if strings.ContainsRune(arg, 0) {
			return errors.New("an argument holds a NUL character")
		}
		size += len(arg) + 1
	}
	for name, value := range r.Env {
		switch {
		case !EnvironmentName(name):
			return fmt.Errorf("%q cannot name an environment variable", name)
		case hostToolFixedEnvironment[name]:
			return fmt.Errorf("%s is set for every run, and a call does not change it", name)
		case strings.HasPrefix(name, "DYLD_"):
			return fmt.Errorf("%s: the loader's variables are not a call's to set", name)
		case strings.ContainsRune(value, 0):
			return fmt.Errorf("the value of %s holds a NUL character", name)
		}
		size += len(name) + len(value) + 2
	}
	if len(r.Env) > HostToolMaxArgs || size > HostToolMaxArgBytes {
		return fmt.Errorf("argv and env together are larger than %d bytes", HostToolMaxArgBytes)
	}
	if r.Seconds < 0 || r.Seconds > HostToolMaxSeconds {
		return fmt.Errorf("seconds is %d: a run takes at most %d", r.Seconds, HostToolMaxSeconds)
	}
	return checkHostToolDir(r.Dir)
}

// checkHostToolDir reports why dir does not name a folder inside the
// Shared Folder: it is a path from the Shared Folder, with no step out.
func checkHostToolDir(dir string) error {
	switch {
	case dir == "":
		return nil
	case strings.ContainsRune(dir, 0):
		return errors.New("dir holds a NUL character")
	case path.IsAbs(dir):
		return errors.New("dir is a path from " + SharedFolderPath + ", such as build/out: it does not begin with a slash")
	case strings.HasPrefix(dir, "-"):
		return errors.New("dir begins with a hyphen")
	}
	for _, step := range strings.Split(dir, "/") {
		if step == ".." {
			return errors.New("dir steps out of " + SharedFolderPath)
		}
	}
	return nil
}
