package hosttool

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"zeolite/internal/sandbox"
)

// The tests start this test's own program as a Host Tool, under the
// Wall: it then tries what a tool may and may not do of this Mac, and
// prints what came of each.
const (
	probeEnv       = "ZEOLITE_WALL_PROBE"
	probeCanaryEnv = "ZEOLITE_PROBE_CANARY"   // a file outside the Wall
	probeOutEnv    = "ZEOLITE_PROBE_OUTSIDE"  // a folder outside the Wall
	probeHomeEnv   = "ZEOLITE_PROBE_USERHOME" // the user's home
	probeDialEnv   = "ZEOLITE_PROBE_LISTENER" // an address this Mac listens on
)

func TestMain(m *testing.M) {
	switch os.Getenv(probeEnv) {
	case "":
		os.Exit(m.Run())
	case "environment":
		// The system's own programs are not shown the loader's
		// variables: this one is.
		for _, v := range os.Environ() {
			fmt.Println(v)
		}
	default:
		probe()
	}
	os.Exit(0)
}

// probe tries, in order, what the test expects the Wall to refuse and to
// allow, and prints a line for each: its name, and "ok" or "refused".
func probe() {
	say := func(name string, err error) {
		if err != nil {
			fmt.Printf("%s refused\n", name)
			return
		}
		fmt.Printf("%s ok\n", name)
	}
	_, err := os.ReadFile(os.Getenv(probeCanaryEnv))
	say("read-a-file-outside", err)
	_, err = os.ReadDir(os.Getenv(probeHomeEnv))
	say("list-the-users-home", err)
	say("write-outside", os.WriteFile(filepath.Join(os.Getenv(probeOutEnv), "written"), []byte("x"), 0o600))
	conn, err := net.DialTimeout("tcp", os.Getenv(probeDialEnv), 2*time.Second)
	if err == nil {
		conn.Close()
	}
	say("connect-to-this-mac", err)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		l.Close()
	}
	say("listen", err)
	_, err = os.Hostname()
	say("this-macs-name", err)
	raw, err := unix.SysctlRaw("kern.proc.pid", 1)
	if err == nil && len(raw) == 0 {
		err = os.ErrNotExist
	}
	say("name-another-process", err)
	_, err = os.ReadDir("/System/Volumes/Preboot")
	say("list-the-boot-volume", err)
	_, err = os.ReadDir("/System/Volumes/Data")
	say("list-the-data-volume", err)
	say("run-a-setuid-program", exec.Command("/bin/ps").Run())
	_, err = os.ReadDir("/Applications")
	say("list-the-applications", err)
	say("run-a-system-program", exec.Command("/bin/ls").Run())
	say("write-in-the-shared-folder", os.WriteFile("written-by-a-host-tool", []byte("x"), 0o600))
	say("write-in-the-home", os.WriteFile(filepath.Join(os.Getenv("HOME"), "written"), []byte("x"), 0o600))
	f, err := os.CreateTemp("", "probe")
	if err == nil {
		f.Close()
	}
	say("write-a-temporary-file", err)
}

// walled is a Tools on folders of the test's, with a sandbox's Shared
// Folder, and what the probes need outside the Wall.
type walled struct {
	t        *testing.T
	tools    *Tools
	shared   string // the Shared Folder, as Seatbelt names it
	outside  string
	canary   string
	settings Settings
}

func newWalled(t *testing.T) *walled {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools, err := Install(filepath.Join(base, "hosttool"))
	if err != nil {
		t.Fatal(err)
	}
	w := &walled{t: t, tools: tools, shared: filepath.Join(base, "shared", "research"), outside: filepath.Join(base, "outside")}
	for _, dir := range []string{w.shared, w.outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	w.canary = filepath.Join(w.outside, "secret")
	if err := os.WriteFile(w.canary, []byte("CANARY-outside-the-wall\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.settings = Settings{SandboxID: "s1", SharedFolder: w.shared}
	return w
}

// probeProgram puts this test's program in the Shared Folder, where a
// Host Tool may run it, and returns the request that starts it.
func (w *walled) probeProgram(env map[string]string) sandbox.HostToolRequest {
	w.t.Helper()
	self, err := os.Executable()
	if err != nil {
		w.t.Fatal(err)
	}
	program, err := os.ReadFile(self)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.shared, "probe"), program, 0o700); err != nil {
		w.t.Fatal(err)
	}
	return sandbox.HostToolRequest{Argv: []string{"./probe", "-test.run=^$"}, Env: env}
}

// run runs one request under the Wall and returns its result.
func (w *walled) run(s Settings, r sandbox.HostToolRequest) sandbox.HostToolResult {
	w.t.Helper()
	sc, err := w.tools.prepare(s.SandboxID)
	if err != nil {
		w.t.Fatal(err)
	}
	return w.tools.run(context.Background(), s, sc, w.shared, r)
}

func (w *walled) sh(s Settings, script string) sandbox.HostToolResult {
	w.t.Helper()
	return w.run(s, sandbox.HostToolRequest{Argv: []string{"/bin/sh", "-c", script}})
}

// The Wall, tried from inside: a Host Tool reads this Mac's public
// software and writes its two folders, and of everything else the tests
// name it gets nothing. Outside the Wall the same program gets all of
// it, so each refusal is the Wall's.
func TestTheWallRefusesThisMac(t *testing.T) {
	w := newWalled(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		probeEnv:       "1",
		probeCanaryEnv: w.canary,
		probeOutEnv:    w.outside,
		probeHomeEnv:   home,
		probeDialEnv:   listener.Addr().String(),
	}
	lines := func(out string) map[string]string {
		got := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if name, verdict, ok := strings.Cut(line, " "); ok {
				got[name] = verdict
			}
		}
		return got
	}

	refused := []string{
		"read-a-file-outside", "list-the-users-home", "write-outside", "connect-to-this-mac", "listen",
		"this-macs-name", "name-another-process", "list-the-boot-volume", "list-the-data-volume", "run-a-setuid-program",
	}
	allowed := []string{
		"list-the-applications", "run-a-system-program", "write-in-the-shared-folder", "write-in-the-home", "write-a-temporary-file",
	}

	request := w.probeProgram(env)
	result := w.run(w.settings, request)
	if result.Refused != "" || result.ExitCode != 0 {
		t.Fatalf("the probe under the Wall: %+v", result)
	}
	got := lines(result.Stdout)
	for _, name := range refused {
		if got[name] != "refused" {
			t.Errorf("under the Wall, %s: %q, want refused", name, got[name])
		}
	}
	for _, name := range allowed {
		if got[name] != "ok" {
			t.Errorf("under the Wall, %s: %q, want ok", name, got[name])
		}
	}
	if len(got) != len(refused)+len(allowed) {
		t.Errorf("the probe said %d things, the test expects %d:\n%s", len(got), len(refused)+len(allowed), result.Stdout)
	}
	if _, err := os.Stat(filepath.Join(w.outside, "written")); err == nil {
		t.Error("a Host Tool wrote outside its two folders")
	}
	// What it wrote is where the Wall lets it: in the Shared Folder, and
	// in its Scratch, which is where its home is.
	sc := w.tools.scratchOf("s1")
	for _, path := range []string{filepath.Join(w.shared, "written-by-a-host-tool"), filepath.Join(sc.home, "written")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("what the Host Tool wrote is not at %s: %v", path, err)
		}
	}

	// With this Mac's name added to the Wall, that one is answered.
	named := w.settings
	named.MacName = true
	if got := lines(w.run(named, request).Stdout); got["this-macs-name"] != "ok" || got["name-another-process"] != "refused" || got["read-a-file-outside"] != "refused" {
		t.Errorf("with this Mac's name added, the probe says %v", got)
	}

	// The control: with no Wall, everything the probe tries is done.
	control := exec.Command(filepath.Join(w.shared, "probe"), "-test.run=^$")
	control.Dir = w.shared
	control.Env = append(os.Environ(), "HOME="+sc.home)
	for name, value := range env {
		control.Env = append(control.Env, name+"="+value)
	}
	out, err := control.Output()
	if err != nil {
		t.Fatalf("the probe without the Wall: %v", err)
	}
	for name, verdict := range lines(string(out)) {
		if verdict != "ok" {
			t.Errorf("without the Wall, %s: %q: the test cannot tell what the Wall refuses", name, verdict)
		}
	}
}

// A Host Tool has the environment the app gives it and nothing of the
// app's own, and no standard input.
func TestAHostToolsEnvironmentIsItsOwn(t *testing.T) {
	w := newWalled(t)
	t.Setenv("ZEOLITE_SECRET_OF_THE_APPS", "a key")
	names := func(s Settings, extra map[string]string) (map[string]string, sandbox.HostToolResult) {
		env := map[string]string{probeEnv: "environment"}
		for name, value := range extra {
			env[name] = value
		}
		result := w.run(s, w.probeProgram(env))
		got := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(result.Stdout), "\n") {
			if name, value, ok := strings.Cut(line, "="); ok {
				got[name] = value
			}
		}
		return got, result
	}
	sc := w.tools.scratchOf("s1")
	got, result := names(w.settings, map[string]string{"GOFLAGS": "-mod=mod"})
	if result.ExitCode != 0 {
		t.Fatalf("env under the Wall: %+v", result)
	}
	want := map[string]string{
		"HOME": sc.home, "TMPDIR": sc.tmp + "/", "XDG_CACHE_HOME": sc.cache, "TZ": "UTC", "LANG": "en_US.UTF-8",
		"PATH": w.tools.path(w.settings), "GOFLAGS": "-mod=mod", probeEnv: "environment",
	}
	if w.tools.developerDir != "" {
		want["DEVELOPER_DIR"] = w.tools.developerDir
	}
	if w.tools.sdk != "" {
		want["SDKROOT"] = w.tools.sdk
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s is %q, want %q", name, got[name], value)
		}
	}
	for name := range got {
		// The system's own launcher adds one of its own.
		if _, expected := want[name]; !expected && name != "__CF_USER_TEXT_ENCODING" {
			t.Errorf("a Host Tool is given %s=%q", name, got[name])
		}
	}
	if path := w.tools.path(w.settings); !strings.HasSuffix(path, "/opt/homebrew/bin:/usr/bin:/bin") || strings.Contains(path, w.tools.root) {
		t.Errorf("a Host Tool's PATH is %q", path)
	}

	// With the GPU: the GPU Shim is loaded, and told where the cache
	// folder is, in the Scratch; bash and zsh are told where the Shell
	// Startup File is; and the Homebrew Wrappers stand before Homebrew's
	// programs. Nothing else is added.
	gpu := w.settings
	gpu.GPU = true
	got, _ = names(gpu, nil)
	delete(want, "GOFLAGS")
	for name, value := range map[string]string{
		"DYLD_INSERT_LIBRARIES": w.tools.gpu.shim, sandbox.HostToolCacheEnv: sc.userCache + "/", "CYCLES_METAL_DISABLE_BINARY_ARCHIVES": "1",
		"BASH_ENV": filepath.Join(w.tools.root, "gpu", ".zshenv"), "ZDOTDIR": filepath.Join(w.tools.root, "gpu"),
		"PATH": w.tools.path(gpu),
	} {
		want[name] = value
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("with the GPU, %s is %q, want %q", name, got[name], value)
		}
	}
	for name := range got {
		if _, expected := want[name]; !expected && name != "__CF_USER_TEXT_ENCODING" {
			t.Errorf("with the GPU a Host Tool is given %s=%q", name, got[name])
		}
	}
	if path := w.tools.path(gpu); !strings.HasSuffix(path, ":"+filepath.Join(w.tools.root, "gpu", "bin")+":/opt/homebrew/bin:/usr/bin:/bin") {
		t.Errorf("with the GPU a Host Tool's PATH is %q", path)
	}
	// A request names neither file to a shell.
	for _, name := range []string{"BASH_ENV", "ZDOTDIR"} {
		if result := w.run(gpu, sandbox.HostToolRequest{Argv: []string{"ls"}, Env: map[string]string{name: w.shared}}); result.Refused == "" {
			t.Errorf("a request that sets %s was run: %+v", name, result)
		}
	}

	// Standard input is the null device: a program that reads it gets
	// its end at once.
	if result := w.sh(w.settings, "cat; echo read-to-the-end"); result.Stdout != "read-to-the-end\n" {
		t.Errorf("a program that reads standard input: %+v", result)
	}
}

// What a run prints comes back, with its status; a run starts in the
// Shared Folder, or in a folder of it, and in none outside.
func TestARunsOutputAndFolder(t *testing.T) {
	w := newWalled(t)
	if result := w.sh(w.settings, "echo out; echo err >&2; exit 3"); result.ExitCode != 3 || result.Stdout != "out\n" || result.Stderr != "err\n" || result.Truncated || result.TimedOut || result.Refused != "" {
		t.Errorf("a run that prints and fails: %+v", result)
	}
	// The start of what it printed is kept, and it is told that there
	// was more.
	result := w.sh(w.settings, `head -c 1000000 /dev/zero | tr '\0' x`)
	if len(result.Stdout) != sandbox.HostToolMaxOutput || !result.Truncated || result.ExitCode != 0 {
		t.Errorf("a run that prints 1000000 bytes: %d kept, truncated %v, status %d", len(result.Stdout), result.Truncated, result.ExitCode)
	}
	// What is not text does not break the answer.
	if result := w.sh(w.settings, `printf '\377\376ok'`); !strings.HasSuffix(result.Stdout, "ok") || strings.Contains(result.Stdout, "\xff") {
		t.Errorf("a run that prints bytes that are no text: %q", result.Stdout)
	}

	if err := os.MkdirAll(filepath.Join(w.shared, "build", "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	if result := w.run(w.settings, sandbox.HostToolRequest{Argv: []string{"/bin/pwd"}}); strings.TrimSpace(result.Stdout) != w.shared {
		t.Errorf("a run starts in %q, want the Shared Folder %q", result.Stdout, w.shared)
	}
	if result := w.run(w.settings, sandbox.HostToolRequest{Argv: []string{"/bin/pwd"}, Dir: "build/out"}); strings.TrimSpace(result.Stdout) != filepath.Join(w.shared, "build", "out") {
		t.Errorf("a run in build/out starts in %q", result.Stdout)
	}
	for _, dir := range []string{"..", "/", w.outside, "build/../../outside"} {
		if result := w.run(w.settings, sandbox.HostToolRequest{Argv: []string{"/bin/ls"}, Dir: dir}); result.Refused == "" {
			t.Errorf("a run in %q was started: %+v", dir, result)
		}
	}
	// A link an agent made out of the Shared Folder leads nowhere: the
	// change of folder is made inside the Wall.
	if err := os.Symlink(w.outside, filepath.Join(w.shared, "way-out")); err != nil {
		t.Fatal(err)
	}
	if result := w.run(w.settings, sandbox.HostToolRequest{Argv: []string{"/bin/ls"}, Dir: "way-out"}); result.ExitCode == 0 || strings.Contains(result.Stdout, "secret") {
		t.Errorf("a run in a folder linked out of the Shared Folder: %+v", result)
	}
	if result := w.sh(w.settings, "cat way-out/secret"); result.ExitCode == 0 || strings.Contains(result.Stdout, "CANARY") {
		t.Errorf("reading through a link out of the Shared Folder: %+v", result)
	}
	// A request that is refused starts nothing.
	if result := w.run(w.settings, sandbox.HostToolRequest{Argv: []string{"ls"}, Env: map[string]string{"DYLD_INSERT_LIBRARIES": "/x"}}); !strings.Contains(result.Refused, "loader") {
		t.Errorf("a request that sets the loader's variables: %+v", result)
	}
	if result := w.run(w.settings, sandbox.HostToolRequest{Argv: []string{"no-such-program-on-this-mac"}}); result.ExitCode == 0 || result.Refused != "" {
		t.Errorf("a program that is not there: %+v", result)
	}
}

// alive reports whether a process is still there.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// pidIn reads the process ID a run wrote into a file of the Shared Folder.
func (w *walled) pidIn(name string) int {
	w.t.Helper()
	data, err := os.ReadFile(filepath.Join(w.shared, name))
	if err != nil {
		w.t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		w.t.Fatalf("%s holds %q", name, data)
	}
	return pid
}

func gone(t *testing.T, pid int, what string) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); alive(pid); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("%s is still running", what)
			return
		}
	}
}

// A run ends when its time is up, and when its program ends: with it
// goes every process it started.
func TestARunEndsWithEveryProcessItStarted(t *testing.T) {
	w := newWalled(t)
	start := time.Now()
	result := w.run(w.settings, sandbox.HostToolRequest{Argv: []string{"/bin/sh", "-c", "sleep 300 & echo $! > child; echo started; wait"}, Seconds: 1})
	if !result.TimedOut || result.ExitCode == 0 || result.Stdout != "started\n" {
		t.Errorf("a run that takes too long: %+v", result)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("a run of one second took %s", took)
	}
	gone(t, w.pidIn("child"), "the child of a run that was ended")

	// A program that ends and leaves another behind: the run is over,
	// and what it left is ended.
	start = time.Now()
	result = w.sh(w.settings, "sleep 300 & echo $! > left; echo leaving")
	if result.TimedOut || result.ExitCode != 0 || result.Stdout != "leaving\n" {
		t.Errorf("a run that leaves a process behind: %+v", result)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("a run whose program ended at once took %s", took)
	}
	gone(t, w.pidIn("left"), "the process a run left behind")

	// A program that puts a process in a group of its own is not ended
	// with the run: the run is over all the same, with what was printed
	// until then, and that process is still under the Wall.
	if _, err := os.Stat("/usr/bin/perl"); err != nil {
		t.Skip("this Mac has no perl to leave a process group with")
	}
	start = time.Now()
	result = w.run(w.settings, sandbox.HostToolRequest{Argv: []string{"/usr/bin/perl", "-e", `use POSIX; $| = 1; print "before\n"; exit if fork; POSIX::setsid(); open(F, ">escaped"); print F "$$\n"; close F; sleep 20; print "late\n"`}})
	if result.ExitCode != 0 || result.Stdout != "before\n" {
		t.Errorf("a run whose program left a process in another group: %+v", result)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("a run whose program left its group took %s", took)
	}
	if escaped := w.pidIn("escaped"); alive(escaped) {
		syscall.Kill(escaped, syscall.SIGKILL)
	}
}

// serve readies a sandbox's Host Tools, and ends them with the test.
func (w *walled) serve(s Settings) *Served {
	w.t.Helper()
	served, err := w.tools.Serve(s)
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(served.Close)
	return served
}

// A sandbox's Host Tools are served request by request, a few at once;
// a request whose asker left is ended; and when they are served no
// more, so is every run.
func TestRequestsAreServedSideBySide(t *testing.T) {
	w := newWalled(t)
	served := w.serve(w.settings)
	sh := func(script string) sandbox.HostToolRequest {
		return sandbox.HostToolRequest{Argv: []string{"/bin/sh", "-c", script}}
	}
	ctx := context.Background()
	if one, two, three := served.Run(ctx, sh("echo one")), served.Run(ctx, sh("echo two; exit 2")), served.Run(ctx, sandbox.HostToolRequest{Argv: []string{"ls"}, Dir: "../outside"}); one.Stdout != "one\n" || two.Stdout != "two\n" || two.ExitCode != 2 || three.Refused == "" {
		t.Errorf("three requests were answered %+v, %+v, %+v", one, two, three)
	}

	// As many run at once as HostToolMaxRuns, and one more is refused.
	type ended struct {
		id     int
		result sandbox.HostToolResult
	}
	results := make(chan ended, sandbox.HostToolMaxRuns)
	withdraw := map[int]context.CancelFunc{}
	for id := 10; id < 10+sandbox.HostToolMaxRuns; id++ {
		runCtx, cancel := context.WithCancel(ctx)
		withdraw[id] = cancel
		go func() {
			results <- ended{id, served.Run(runCtx, sh(fmt.Sprintf("echo $$ > run%d; sleep 300", id)))}
		}()
	}
	waitFor(t, "the runs to start", func() bool {
		for id := 10; id < 10+sandbox.HostToolMaxRuns; id++ {
			if _, err := os.Stat(filepath.Join(w.shared, fmt.Sprintf("run%d", id))); err != nil {
				return false
			}
		}
		return true
	})
	if got := served.Run(ctx, sh("echo too-many")); got.Refused != fmt.Sprintf("%d runs are under way, which is as many as there are at once: wait for one to end", sandbox.HostToolMaxRuns) {
		t.Errorf("one run too many was answered %+v", got)
	}
	// While they are served, the sandbox's Scratch is not deleted.
	if err := w.tools.RemoveScratch("s1"); err == nil || !strings.Contains(err.Error(), "running") {
		t.Errorf("deleting the Scratch of a sandbox whose Host Tools are served: %v", err)
	}

	// One's asker leaves: it is ended, and its place is free.
	left := w.pidIn("run10")
	withdraw[10]()
	gone(t, left, "a run whose asker left")
	if got := <-results; got.id != 10 || got.result.ExitCode == 0 || got.result.TimedOut {
		t.Errorf("the run whose asker left was answered %+v", got)
	}
	if got := served.Run(ctx, sh("echo after")); got.Stdout != "after\n" {
		t.Errorf("after a run's asker left, the next request was answered %+v", got)
	}

	// They are served no more: every run ends, and nothing more is run.
	rest := []int{w.pidIn("run11"), w.pidIn("run12"), w.pidIn("run13")}
	closed := make(chan struct{})
	go func() { served.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the Host Tools were closed and their runs did not end")
	}
	for _, pid := range rest {
		gone(t, pid, "a run whose sandbox's Host Tools were closed")
	}
	if got := served.Run(ctx, sh("echo late")); got.Refused != "no more programs are run on the Mac from here" || got.Stdout != "" {
		t.Errorf("a request after the Host Tools were closed was answered %+v", got)
	}
	// Its Scratch can be deleted now; and closing twice is closing once.
	served.Close()
	if err := w.tools.RemoveScratch("s1"); err != nil {
		t.Errorf("deleting the Scratch of a sandbox whose Host Tools were closed: %v", err)
	}
	// Two servings of one sandbox, as a sandbox that was started again
	// before its last Link was let go: the Scratch waits for both.
	first, second := w.serve(w.settings), w.serve(w.settings)
	first.Close()
	if err := w.tools.RemoveScratch("s1"); err == nil {
		t.Error("the Scratch was deleted while its sandbox's Host Tools were still served")
	}
	second.Close()
	if err := w.tools.RemoveScratch("s1"); err != nil {
		t.Errorf("deleting the Scratch once nothing was served: %v", err)
	}
}

// A sandbox's Host Tool Scratch is deleted by its ID, and nothing a link
// in it leads to. What a Host Tool left there to stay goes with it: the
// Wall lets a tool make a folder of its Scratch read-only and put a file
// there under the flag that forbids deleting it, and neither keeps a
// sandbox from being destroyed.
func TestAScratchIsDeletedByItsSandboxsID(t *testing.T) {
	w := newWalled(t)
	if result := w.sh(w.settings, `echo kept > "$HOME/file"`); result.ExitCode != 0 {
		t.Fatalf("writing into the Scratch: %+v", result)
	}
	sc := w.tools.scratchOf("s1")
	// Whatever the test leaves, the test's own folder can be deleted.
	t.Cleanup(func() {
		exec.Command("/usr/bin/chflags", "-R", "nouchg", sc.dir).Run()
		exec.Command("/bin/chmod", "-R", "u+rwx", sc.dir).Run()
	})
	// From inside the Wall, as an agent would: a build's read-only
	// folders, a folder that may not be listed, and a file, a folder and
	// a link that may not be changed.
	for what, script := range map[string]string{
		"read-only folders":                `mkdir -p "$HOME/pkg/mod" && echo x > "$HOME/pkg/mod/file" && chmod 500 "$HOME/pkg/mod" "$HOME/pkg"`,
		"a folder that may not be listed":  `mkdir -p "$TMPDIR/sealed/in" && echo x > "$TMPDIR/sealed/in/file" && chmod 000 "$TMPDIR/sealed"`,
		"a file that may not be changed":   `echo x > "$HOME/kept" && chmod 000 "$HOME/kept" && chflags uchg "$HOME/kept"`,
		"a folder that may not be changed": `mkdir "$HOME/keptdir" && echo x > "$HOME/keptdir/file" && chflags uchg "$HOME/keptdir"`,
		"a link that may not be changed":   `ln -s /etc "$HOME/keptlink" && chflags -h uchg "$HOME/keptlink"`,
	} {
		if result := w.sh(w.settings, script); result.ExitCode != 0 {
			t.Fatalf("a Host Tool leaving %s in its Scratch: %+v", what, result)
		}
	}
	// The control: as it was left, the Scratch cannot be deleted.
	if root, err := os.OpenRoot(filepath.Dir(sc.dir)); err != nil {
		t.Fatal(err)
	} else {
		err := root.RemoveAll("s1")
		root.Close()
		if err == nil {
			t.Fatal("the Scratch was deleted as it was left: the test leaves nothing to stay")
		}
	}
	if result := w.sh(w.settings, `echo again > "$HOME/file"`); result.ExitCode != 0 {
		t.Fatalf("writing into the Scratch after the control: %+v", result)
	}
	if err := os.Symlink(w.outside, filepath.Join(sc.home, "way-out")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.tools.prepare("s2"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "../s1", "S1", "s1/home", "."} {
		if err := w.tools.RemoveScratch(id); err == nil {
			t.Errorf("deleting the Scratch of %q was done", id)
		}
	}
	if err := w.tools.RemoveScratch("s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(sc.dir); !os.IsNotExist(err) {
		t.Errorf("the Scratch is still there: %v", err)
	}
	if _, err := os.Stat(w.canary); err != nil {
		t.Errorf("deleting a Scratch deleted what a link in it led to: %v", err)
	}
	if _, err := os.Stat(w.tools.scratchOf("s2").dir); err != nil {
		t.Errorf("deleting one sandbox's Scratch deleted another's: %v", err)
	}
	// One that is not there is deleted already.
	if err := w.tools.RemoveScratch("s1"); err != nil {
		t.Errorf("deleting a Scratch that is not there: %v", err)
	}
}

// The folders are the user's alone; the GPU Shim is the one this build
// of the app carries, with the Shell Startup File and the Homebrew
// Wrappers beside it: one for each program of Homebrew's that the
// loader loads, and none for a script.
func TestInstall(t *testing.T) {
	w := newWalledWithHomebrew(t)
	gpu := filepath.Join(w.tools.root, "gpu")
	for _, dir := range []string{w.tools.root, w.tools.scratchRoot(), gpu, filepath.Join(gpu, "bin")} {
		if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v, %v", dir, info, err)
		}
	}
	shim := filepath.Join(gpu, "gpushim.dylib")
	homebrew := filepath.Join(w.shared, "homebrew", "bin")
	texts := map[string]string{
		shim:                          string(gpuShim),
		filepath.Join(gpu, ".zshenv"): "export DYLD_INSERT_LIBRARIES='" + shim + "'\n",
		filepath.Join(gpu, "wrapper"): "#!/bin/sh\nexport DYLD_INSERT_LIBRARIES='" + shim + "'\nexec '" + homebrew + `'/"${0##*/}" "$@"` + "\n",
	}
	check := func(when string) {
		t.Helper()
		for path, want := range texts {
			if have, err := os.ReadFile(path); err != nil || string(have) != want {
				t.Errorf("%s, %s is not as this build has it: %v", when, path, err)
			}
		}
		if entries, _ := os.ReadDir(gpu); len(entries) != 4 {
			t.Errorf("%s, the GPU's folder holds %d things, want the four", when, len(entries))
		}
	}
	check("installed")
	wrappers := func() string {
		entries, err := os.ReadDir(filepath.Join(gpu, "bin"))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			// The stray is the test's own, and no wrapper.
			if target, err := os.Readlink(filepath.Join(gpu, "bin", e.Name())); e.Name() != "stray" && (err != nil || target != "../wrapper") {
				t.Errorf("the wrapper of %s: %q, %v", e.Name(), target, err)
			}
			names = append(names, e.Name())
		}
		return strings.Join(names, " ")
	}
	if got := wrappers(); got != "brewprobe" {
		t.Errorf("the Homebrew Wrappers are %q, want one for the program and none for the script", got)
	}

	// Files that hold something else are replaced.
	for path := range texts {
		if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := install(w.tools.root, homebrew); err != nil {
		t.Fatal(err)
	}
	check("installed again")

	// What Homebrew holds changes: the wrappers follow at the next start
	// of a sandbox's Host Tools with the GPU, and at none without.
	program, err := os.ReadFile(filepath.Join(homebrew, "brewprobe"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homebrew, "another"), program, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("another", filepath.Join(homebrew, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(homebrew, "brewprobe")); err != nil {
		t.Fatal(err)
	}
	// Something that is no wrapper, in their folder.
	if err := os.WriteFile(filepath.Join(gpu, "bin", "stray"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	serveOnce := func(s Settings) {
		t.Helper()
		served := w.serve(s)
		if got := served.Run(context.Background(), sandbox.HostToolRequest{Argv: []string{"/bin/echo", "served"}}); got.Stdout != "served\n" {
			t.Fatalf("a served request was answered %+v", got)
		}
		served.Close()
	}
	serveOnce(w.settings)
	if got := wrappers(); got != "brewprobe stray" {
		t.Errorf("without the GPU the Homebrew Wrappers were changed: %q", got)
	}
	withGPU := w.settings
	withGPU.GPU = true
	serveOnce(withGPU)
	if got := wrappers(); got != "another linked" {
		t.Errorf("after Homebrew changed, the Homebrew Wrappers are %q", got)
	}
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !done(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s", what)
		}
	}
}

// The package imports the core alone, and with it what deletes the
// agents' files, for a Scratch; it makes no socket: what it starts is
// reached by pipes, and it is called with one request at a time, which
// the sandbox's Link carried here.
func TestThePackageImportsTheCoreAlone(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		files++
		f, err := parser.ParseFile(token.NewFileSet(), e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			switch {
			case path == "zeolite/internal/sandbox", path == "zeolite/internal/agentfiles":
			case strings.HasPrefix(path, "zeolite/"):
				t.Errorf("%s imports %s", e.Name(), path)
			case strings.Contains(strings.SplitN(path, "/", 2)[0], "."):
				t.Errorf("%s imports %s, which is not the standard library's", e.Name(), path)
			case path == "net" || strings.HasPrefix(path, "net/"):
				t.Errorf("%s imports %s: a Host Tool is reached by pipes", e.Name(), path)
			}
		}
	}
	if files < 3 {
		t.Errorf("read %d files", files)
	}
}

// newWalledWithHomebrew is a walled whose Homebrew is a folder of the
// test's, in the Shared Folder, where a Host Tool may run a program:
// it holds this test's program under the name brewprobe, and a script.
func newWalledWithHomebrew(t *testing.T) *walled {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &walled{t: t, shared: filepath.Join(base, "shared", "research"), outside: filepath.Join(base, "outside")}
	homebrew := filepath.Join(w.shared, "homebrew", "bin")
	for _, dir := range []string{homebrew, w.outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	program, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string][]byte{"brewprobe": program, "brewscript": []byte("#!/bin/sh\necho a script of Homebrew's\n")} {
		if err := os.WriteFile(filepath.Join(homebrew, name), text, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if w.tools, err = install(filepath.Join(base, "hosttool"), homebrew); err != nil {
		t.Fatal(err)
	}
	w.settings = Settings{SandboxID: "s1", SharedFolder: w.shared}
	return w
}

// The GPU Shim is loaded by a variable of the loader's, which this
// Mac's own programs drop for whatever they start: every shell, env,
// xargs, find, time. So the variable is set again after them: by bash
// and zsh, from the Shell Startup File, and by the Homebrew Wrappers,
// for a program of Homebrew's started by its name. Each row is a way a
// run starts this test's program, by its path and by Homebrew's name
// for it, and says whether the program then has the Shim.
func TestTheShimReachesWhatARunStarts(t *testing.T) {
	w := newWalledWithHomebrew(t)
	request := w.probeProgram(map[string]string{probeEnv: "environment"})
	files := map[string]string{
		"viabash":  "#!/bin/bash\n\"$@\"\n",
		"viash":    "#!/bin/sh\n\"$@\"\n",
		"Makefile": "plain:\n\t@$(P)\nshell:\n\t@$(P); true\n",
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(w.shared, name), []byte(text), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	gpu := w.settings
	gpu.GPU = true
	loaded := "DYLD_INSERT_LIBRARIES=" + w.tools.gpu.shim + "\n"

	const python = "import subprocess, sys; sys.exit(subprocess.call("
	rows := []struct {
		name string
		argv func(program string) []string
		// byPath and byName say whether the program has the Shim, started
		// as ./probe and as brewprobe.
		byPath, byName bool
		// needs is a program of this Mac's compilers that the row runs.
		needs string
	}{
		{"run", func(p string) []string { return []string{p} }, true, true, ""},
		// A shell at the head of a request.
		{"sh -c", func(p string) []string { return []string{"sh", "-c", p} }, true, true, ""},
		{"/bin/sh -c", func(p string) []string { return []string{"/bin/sh", "-c", p} }, true, true, ""},
		{"bash -c", func(p string) []string { return []string{"bash", "-c", p} }, true, true, ""},
		{"bash -lc", func(p string) []string { return []string{"bash", "-lc", p} }, true, true, ""},
		{"/bin/bash -c", func(p string) []string { return []string{"/bin/bash", "-c", p} }, true, true, ""},
		{"zsh -c", func(p string) []string { return []string{"zsh", "-c", p} }, true, true, ""},
		{"a #!/bin/bash script", func(p string) []string { return []string{"./viabash", p} }, true, true, ""},
		// bash and zsh, however deep, after a program that dropped it.
		{"env bash -c", func(p string) []string { return []string{"env", "bash", "-c", p} }, true, true, ""},
		{"find -exec zsh -c", func(p string) []string { return []string{"find", ".", "-maxdepth", "0", "-exec", "zsh", "-c", p, ";"} }, true, true, ""},
		{"sh -c of a #!/bin/bash script", func(p string) []string { return []string{"bash", "-c", "sh -c './viabash " + p + "'"} }, true, true, ""},
		// This Mac's compilers' own programs pass the variable on.
		{"make", func(p string) []string { return []string{"make", "-s", "P=" + p, "plain"} }, true, true, "make"},
		{"make, with bash for its shell", func(p string) []string { return []string{"make", "-s", "P=" + p, "SHELL=/bin/bash", "shell"} }, true, true, "make"},
		{"python3, by a list", func(p string) []string { return []string{"python3", "-c", python + "[sys.argv[1]]))", p} }, true, true, "python3"},
		{"python3, by bash", func(p string) []string {
			return []string{"python3", "-c", python + "sys.argv[1], shell=True, executable='/bin/bash'))", p}
		}, true, true, "python3"},

		// What stays uncovered: a program named by its path that one of
		// this Mac's own programs starts, other than bash and zsh; and
		// the shells that are told to read no file.
		{"a #!/bin/sh script", func(p string) []string { return []string{"./viash", p} }, false, true, ""},
		{"sh -c, deeper in a run", func(p string) []string { return []string{"bash", "-c", "sh -c " + p} }, false, true, ""},
		{"env", func(p string) []string { return []string{"env", p} }, false, true, ""},
		{"xargs", func(p string) []string { return []string{"bash", "-c", "echo " + p + " | xargs -n 1 env"} }, false, true, ""},
		{"find -exec", func(p string) []string { return []string{"find", ".", "-maxdepth", "0", "-exec", p, ";"} }, false, true, ""},
		{"time", func(p string) []string { return []string{"/usr/bin/time", p} }, false, true, ""},
		{"make, with a line for its shell", func(p string) []string { return []string{"make", "-s", "P=" + p, "shell"} }, false, true, "make"},
		{"python3, by its shell", func(p string) []string { return []string{"python3", "-c", python + "sys.argv[1], shell=True))", p} }, false, true, "python3"},
		{"bash -p", func(p string) []string { return []string{"bash", "-p", "-c", p} }, false, true, ""},
		{"bash --posix", func(p string) []string { return []string{"bash", "--posix", "-c", p} }, false, true, ""},
		{"zsh -f", func(p string) []string { return []string{"zsh", "-f", "-c", p} }, false, true, ""},
	}
	for _, row := range rows {
		if row.needs != "" {
			// This Mac's own make and python3 look theirs up through a
			// cache outside the Wall: only the compilers' folders have one
			// a Host Tool runs.
			found := false
			for _, dir := range filepath.SplitList(w.tools.path(gpu)) {
				if _, err := os.Stat(filepath.Join(dir, row.needs)); err == nil {
					found = dir != "/usr/bin"
					break
				}
			}
			if !found {
				t.Logf("%s: this Mac's compilers have no %s: not tried", row.name, row.needs)
				continue
			}
		}
		for _, way := range []struct {
			program string
			want    bool
		}{{"./probe", row.byPath}, {"brewprobe", row.byName}} {
			r := request
			r.Argv = row.argv(way.program)
			result := w.run(gpu, r)
			if result.ExitCode != 0 || !strings.Contains(result.Stdout, probeEnv+"=environment\n") {
				t.Errorf("%s, of %s: %+v", row.name, way.program, result)
				continue
			}
			if got := strings.Contains(result.Stdout, loaded); got != way.want {
				t.Errorf("%s, of %s: the program has the GPU Shim: %v, want %v", row.name, way.program, got, way.want)
			}
		}
	}

	// Without the GPU nothing loads the Shim, by any way.
	for _, argv := range [][]string{{"./probe"}, {"brewprobe"}, {"bash", "-c", "./probe"}, {"zsh", "-c", "brewprobe"}, {"sh", "-c", "brewprobe"}} {
		r := request
		r.Argv = argv
		if result := w.run(w.settings, r); result.ExitCode != 0 || strings.Contains(result.Stdout, "DYLD_") || strings.Contains(result.Stdout, w.tools.gpu.shim) {
			t.Errorf("without the GPU, %q: %+v", argv, result)
		}
	}
}
