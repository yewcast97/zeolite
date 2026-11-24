// Package hosttool is the app's side of a sandbox's Host Tools: programs
// of this Mac that the sandbox's agent runs. It starts each under the
// Host Tool Wall, a Seatbelt profile that leaves it this Mac's public
// software to read, the sandbox's Shared Folder and its Host Tool
// Scratch to write, and nothing else of this Mac: no file of the user's,
// no network, no other program.
//
// The launch is part of the Wall. A Host Tool has the environment this
// package gives it and none of the app's, no standard input, a process
// group of its own that ends with its run, a bound on how long it runs
// and on how much of what it prints is sent back.
//
// The package reads nothing in a Shared Folder and nothing a Host Tool
// printed: it names the one to the Wall, and passes the other on. It is
// called with one request at a time, and reads no stream: what carries
// a request here from a workstation is the sandbox's Link.
//
// The GPU Shim is loaded by a variable of the loader's, which this Mac's
// own programs drop for whatever they start: every shell, env, xargs,
// find. So a run that is given the GPU has the variable set again after
// them, three ways: bash and zsh set it from the Shell Startup File; sh
// at the head of a request, which reads no such file, is started as
// bash; and a program of Homebrew's started by its name is started by
// its Homebrew Wrapper, which sets it. All three are in gpu.go.
package hosttool

//go:generate mkdir -p bin
//go:generate clang -dynamiclib -arch arm64 -arch x86_64 -O2 -framework CoreFoundation -framework Security -o bin/gpushim.dylib gpushim/gpushim.c

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"zeolite/internal/agentfiles"
	"zeolite/internal/sandbox"
)

// The Wall, and what a sandbox's Setup may add to it.
var (
	//go:embed wall.sb
	wall string
	//go:embed gpu.sb
	gpuRules string
	//go:embed macname.sb
	macNameRules string
	//go:embed bin/gpushim.dylib
	gpuShim []byte
)

// DefaultRoot is where the Host Tools' folders are: beside the Shared
// Folders' root, which the VM mounts, and not in it. It is not under the
// user's home, whose path holds the user's name: a Host Tool is told
// where its Scratch is.
const DefaultRoot = "/Users/Shared/zeolite/hosttool.noindex"

// The system's programs that start a Host Tool: sandbox-exec puts the
// Wall around env, and env starts the tool in its folder. The loader's
// variables do not pass through sandbox-exec, so the one that loads the
// GPU Shim is set by env, inside the Wall.
const (
	sandboxExec = "/usr/bin/sandbox-exec"
	envProgram  = "/usr/bin/env"
)

// homebrewBin is where Homebrew keeps its programs on this Mac.
const homebrewBin = "/opt/homebrew/bin"

// Tools starts Host Tools on this Mac.
type Tools struct {
	root string
	// gpu is what a run that is given the GPU has beside the Wall.
	gpu gpuFiles
	// homebrewBin is Homebrew's folder of programs, and compilerDirs the
	// folders of this Mac's compilers: a Host Tool's PATH is those, then
	// Homebrew's, then the system's. developerDir and sdk are where the
	// compilers are, as every Host Tool is told.
	homebrewBin  string
	compilerDirs []string
	developerDir string
	sdk          string

	mu sync.Mutex
	// serving are the sandboxes whose Host Tools are being served: a
	// Scratch is not deleted under one.
	serving map[string]int
}

// Install makes the Host Tools' root and writes there what a run that is
// given the GPU has, the GPU Shim first, or finds them as this build of
// the app has them.
func Install(root string) (*Tools, error) { return install(root, homebrewBin) }

// install is Install with Homebrew's folder of programs named: a test
// names one of its own.
func install(root, homebrewBin string) (*Tools, error) {
	if _, err := os.Stat(sandboxExec); err != nil {
		return nil, fmt.Errorf("this Mac has no %s: Host Tools cannot be walled", sandboxExec)
	}
	if err := ownDir(root); err != nil {
		return nil, err
	}
	// Seatbelt names a file by the path it resolves to.
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	t := &Tools{root: resolved, gpu: gpuFilesIn(resolved), homebrewBin: homebrewBin, serving: map[string]int{}}
	if err := ownDir(t.scratchRoot()); err != nil {
		return nil, err
	}
	if err := t.installGPU(); err != nil {
		return nil, fmt.Errorf("what a Host Tool with the GPU is given: %w", err)
	}
	t.findCompilers()
	return t, nil
}

// ownDir makes a folder that is the user's alone, or says why the one
// that is there is not.
func ownDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a folder", dir)
	}
	return os.Chmod(dir, 0o700)
}

// findCompilers looks where this Mac keeps its compilers. The system's
// own cc and swiftc are shims that look them up through a cache outside
// the Wall, so a Host Tool is given their real folders first.
func (t *Tools) findCompilers() {
	var dirs []string
	const xcode = "/Applications/Xcode.app/Contents/Developer"
	const commandLineTools = "/Library/Developer/CommandLineTools"
	if isDir(xcode) {
		dirs = append(dirs, xcode+"/Toolchains/XcodeDefault.xctoolchain/usr/bin", xcode+"/usr/bin")
		t.developerDir = xcode
		t.sdk = xcode + "/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk"
	} else if isDir(commandLineTools) {
		t.developerDir = commandLineTools
		t.sdk = commandLineTools + "/SDKs/MacOSX.sdk"
	}
	if isDir(commandLineTools) {
		dirs = append(dirs, commandLineTools+"/usr/bin")
	}
	if !isDir(t.sdk) {
		t.sdk = ""
	}
	t.compilerDirs = dirs
}

// path is a Host Tool's PATH: this Mac's compilers, Homebrew's programs
// and the system's. With the GPU the Homebrew Wrappers stand just before
// Homebrew's programs, so that one started by its name is started by
// its wrapper.
func (t *Tools) path(s Settings) string {
	dirs := append([]string{}, t.compilerDirs...)
	if s.GPU {
		dirs = append(dirs, t.gpu.wrappers)
	}
	return strings.Join(append(dirs, t.homebrewBin, "/usr/bin", "/bin"), ":")
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func (t *Tools) scratchRoot() string { return filepath.Join(t.root, "scratch") }

// Settings are what the Host Tools of one sandbox run with.
type Settings struct {
	SandboxID string
	// SharedFolder is the sandbox's Shared Folder, as this Mac has it.
	SharedFolder string
	// GPU and MacName are what the sandbox's Setup adds to the Wall.
	GPU     bool
	MacName bool
}

// profile is the Wall for these settings.
func profile(s Settings) string {
	rules := wall
	if s.GPU {
		rules += gpuRules
	}
	if s.MacName {
		rules += macNameRules
	}
	return rules
}

// scratch is a sandbox's Host Tool Scratch, with its folders.
type scratch struct{ dir, home, tmp, cache, userCache string }

func (t *Tools) scratchOf(sandboxID string) scratch {
	dir := filepath.Join(t.scratchRoot(), sandboxID)
	return scratch{
		dir:   dir,
		home:  filepath.Join(dir, "home"),
		tmp:   filepath.Join(dir, "tmp"),
		cache: filepath.Join(dir, "cache"),
		// The folder Metal keeps its caches in, which the GPU Shim names.
		userCache: filepath.Join(dir, "cache", "user"),
	}
}

// prepare makes a sandbox's Host Tool Scratch where it is not there.
func (t *Tools) prepare(sandboxID string) (scratch, error) {
	if !sandbox.ValidID(sandboxID) {
		return scratch{}, fmt.Errorf("%q is not a sandbox's ID", sandboxID)
	}
	sc := t.scratchOf(sandboxID)
	for _, dir := range []string{sc.dir, sc.home, sc.tmp, sc.cache, sc.userCache} {
		if err := ownDir(dir); err != nil {
			return scratch{}, fmt.Errorf("the Host Tool Scratch: %w", err)
		}
	}
	return sc, nil
}

// RemoveScratch deletes a sandbox's Host Tool Scratch, as the sandbox's
// destruction does: by its ID, without following a link out of it, and
// never while the sandbox's Host Tools are served. What a Host Tool left
// there to stay is deleted with the rest: a folder it made read-only,
// and what it put under a flag that forbids deleting it, which the Wall
// lets it do in the folders it may write.
func (t *Tools) RemoveScratch(sandboxID string) error {
	if !sandbox.ValidID(sandboxID) {
		return fmt.Errorf("%q is not a sandbox's ID", sandboxID)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.serving[sandboxID] > 0 {
		return errors.New("the sandbox's Host Tools are running: stop the sandbox first")
	}
	return agentfiles.Remove(t.scratchRoot(), sandboxID)
}

// environment is what every Host Tool of a sandbox is given, and
// nothing of the app's own: where its files go, which is its Scratch,
// and where this Mac's programs are.
func (t *Tools) environment(s Settings, sc scratch) []string {
	env := []string{
		"PATH=" + t.path(s),
		"HOME=" + sc.home,
		"TMPDIR=" + sc.tmp + "/",
		"XDG_CACHE_HOME=" + sc.cache,
		"TZ=UTC",
		"LANG=en_US.UTF-8",
	}
	if t.developerDir != "" {
		env = append(env, "DEVELOPER_DIR="+t.developerDir)
	}
	if t.sdk != "" {
		env = append(env, "SDKROOT="+t.sdk)
	}
	if s.GPU {
		env = append(env,
			sandbox.HostToolCacheEnv+"="+sc.userCache+"/",
			// Blender is signed so that nothing is loaded into it, the
			// GPU Shim neither. Its renderer keeps compiled kernels in
			// Metal's binary archives, which end the program where the
			// user's cache folder is not known: this is its own switch
			// that turns them off.
			"CYCLES_METAL_DISABLE_BINARY_ARCHIVES=1",
			// Where bash and zsh find the Shell Startup File.
			"BASH_ENV="+t.gpu.startup,
			"ZDOTDIR="+t.gpu.dir,
		)
	}
	return env
}

// command is the run of a request, not yet started: sandbox-exec, which
// puts the Wall around env, which starts the program in its folder.
func (t *Tools) command(s Settings, sc scratch, shared string, r sandbox.HostToolRequest) *exec.Cmd {
	args := []string{"-p", profile(s), "-D", "SHARED=" + shared, "-D", "SCRATCH=" + sc.dir, "-D", "GPU=" + t.gpu.dir, envProgram}
	if r.Dir != "" {
		// The change of folder is env's, inside the Wall: the app
		// follows no link an agent made.
		args = append(args, "-C", r.Dir)
	}
	argv := r.Argv
	if s.GPU {
		args = append(args, shimVariable+"="+t.gpu.shim)
		argv = shellThatReadsTheStartupFile(argv)
	}
	args = append(args, argv...)
	cmd := exec.Command(sandboxExec, args...)
	cmd.Dir = shared
	cmd.Env = t.environment(s, sc)
	names := make([]string, 0, len(r.Env))
	for name := range r.Env {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cmd.Env = append(cmd.Env, name+"="+r.Env[name])
	}
	// No standard input: exec gives the null device. A process group of
	// its own, so that the run's every process ends with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// run runs one request to its end, or until ctx is done, and returns
// what it printed. shared is the Shared Folder as Seatbelt names it.
func (t *Tools) run(ctx context.Context, s Settings, sc scratch, shared string, r sandbox.HostToolRequest) sandbox.HostToolResult {
	var result sandbox.HostToolResult
	if err := r.Check(); err != nil {
		result.Refused = err.Error()
		return result
	}
	seconds := r.Seconds
	if seconds == 0 {
		seconds = sandbox.HostToolDefaultSeconds
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()

	cmd := t.command(s, sc, shared, r)
	// The tool writes to pipes of the app's own, which outlive it only
	// until its group is ended: a process it left behind holds none open.
	stdout, stderr := newCapture(), newCapture()
	if err := errors.Join(stdout.open(), stderr.open()); err != nil {
		stdout.abandon()
		stderr.abandon()
		result.Refused = "the program could not be started: " + err.Error()
		return result
	}
	cmd.Stdout, cmd.Stderr = stdout.w, stderr.w
	if err := cmd.Start(); err != nil {
		stdout.abandon()
		stderr.abandon()
		result.Refused = "the program could not be started: " + err.Error()
		return result
	}
	stdout.started()
	stderr.started()

	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()
	select {
	case <-exited:
	case <-ctx.Done():
		result.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	}
	// The run is over when its program is, or its time: whatever is left
	// of its process group ends now.
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	<-exited

	result.ExitCode = cmd.ProcessState.ExitCode()
	result.Stdout, result.Stderr = stdout.text(), stderr.text()
	result.Truncated = stdout.more || stderr.more
	return result
}
