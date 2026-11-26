package hosttool

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"zeolite/internal/sandbox"
)

// rules are the rules of a profile, each on one line, without comments.
func rules(t *testing.T, profile string) []string {
	t.Helper()
	var text strings.Builder
	for _, line := range strings.Split(profile, "\n") {
		if at := strings.Index(line, ";;"); at >= 0 {
			line = line[:at]
		}
		text.WriteString(line + " ")
	}
	var out []string
	depth, start := 0, 0
	for i, c := range text.String() {
		switch c {
		case '(':
			if depth == 0 {
				start = i
			}
			depth++
		case ')':
			depth--
			if depth == 0 {
				out = append(out, strings.Join(strings.Fields(text.String()[start:i+1]), " "))
			}
		}
	}
	if depth != 0 || len(out) == 0 {
		t.Fatalf("the profile's parentheses do not match")
	}
	return out
}

var quoted = regexp.MustCompile(`"[^"]*"`)

// The Wall as it is written: everything is denied but what its rules
// name, and they name no network, no service of the system's beyond the
// GPU's own, and nothing to write but the two folders.
func TestTheWallAsItIsWritten(t *testing.T) {
	base, gpu, name := rules(t, wall), rules(t, gpuRules), rules(t, macNameRules)
	if base[0] != "(version 1)" || base[1] != "(deny default)" {
		t.Errorf("the Wall begins %q, %q", base[0], base[1])
	}
	all := append(append(append([]string{}, base...), gpu...), name...)
	for _, rule := range all {
		if !strings.HasPrefix(rule, "(allow ") {
			continue
		}
		for _, never := range []string{"(allow default", "network", "trustd", "opendirectoryd", "notification_center", "pasteboard", "windowserver", "coreservicesd", "securityd", "launchservicesd", "appleevent", "nvram", "system-socket", "(subpath \"/\")", "(subpath \"/Users\")", "(subpath \"/Volumes\")", "(subpath \"/private\")", "(subpath \"/Applications\")", "ipc-sysv", "necp"} {
			if strings.Contains(rule, never) {
				t.Errorf("the Wall allows %s: %s", never, rule)
			}
		}
	}

	// Nothing is written but the Shared Folder and the Scratch, and three
	// devices that keep nothing.
	for _, rule := range all {
		if !strings.HasPrefix(rule, "(allow ") || !strings.Contains(rule, "file-write") {
			continue
		}
		switch rule {
		case `(allow file-write* (subpath (param "SHARED")) (subpath (param "SCRATCH")))`,
			`(allow file-read* file-write-data file-ioctl (literal "/dev/null") (literal "/dev/zero") (literal "/dev/dtracehelper"))`:
		default:
			t.Errorf("the Wall lets a tool write: %s", rule)
		}
	}

	// A service of the system's is reached only with the GPU, and then
	// these: the device's own, the shader compiler, the video encoder
	// and decoder.
	services := map[string]bool{
		`"com.apple.gpumemd.source"`: true, `"com.apple.cvmsServ"`: true, `"com.apple.MTLCompilerService"`: true,
		`"com.apple.coremedia.videoencoder"`: true, `"com.apple.coremedia.videodecoder"`: true,
	}
	for _, rule := range append(append([]string{}, base...), name...) {
		if strings.Contains(rule, "mach-lookup") && strings.HasPrefix(rule, "(allow ") {
			t.Errorf("the Wall reaches a service without the GPU: %s", rule)
		}
	}
	for _, rule := range gpu {
		if !strings.Contains(rule, "mach-lookup") {
			continue
		}
		for _, service := range quoted.FindAllString(rule, -1) {
			if !services[service] {
				t.Errorf("with the GPU the Wall reaches %s", service)
			}
		}
	}

	// An extension hands a service of the system's a folder, to read or
	// to write for the tool: a tool issues one only with the GPU, to read
	// what it may itself read and to write its two folders. One for more
	// would make the shader compiler read this Mac for the tool.
	for _, rule := range append(append([]string{}, base...), name...) {
		if strings.Contains(rule, "file-issue-extension") {
			t.Errorf("the Wall issues an extension without the GPU: %s", rule)
		}
	}
	issued := 0
	for _, rule := range gpu {
		if !strings.Contains(rule, "file-issue-extension") {
			continue
		}
		issued++
		switch rule {
		case `(allow file-issue-extension (require-all (extension-class "com.apple.app-sandbox.read") (require-any (subpath (param "SHARED")) (subpath (param "SCRATCH")))))`,
			`(allow file-issue-extension (require-all (extension-class "com.apple.app-sandbox.read-write") (require-any (subpath (param "SHARED")) (subpath (param "SCRATCH")))))`:
		default:
			t.Errorf("with the GPU the Wall issues an extension: %s", rule)
		}
	}
	if issued != 2 {
		t.Errorf("%d rules issue an extension, want the two", issued)
	}

	// What a tool is given with the GPU, beside the device: one folder of
	// the app's, to read and to run from, and the names that lead to it.
	// It is the one folder a tool runs programs from that is added to
	// the Wall, and nothing is written there.
	var given []string
	for _, rule := range all {
		if strings.Contains(rule, `(param "GPU")`) {
			given = append(given, rule)
		}
	}
	if len(given) != 2 ||
		given[0] != `(allow file-read* file-test-existence file-map-executable process-exec (subpath (param "GPU")))` ||
		given[1] != `(allow file-read-metadata file-test-existence (path-ancestors (param "GPU")))` {
		t.Errorf("the GPU's folder is given by %q", given)
	}
	executes := 0
	for _, rule := range all {
		if strings.HasPrefix(rule, "(allow ") && strings.Contains(rule, "process-exec") {
			executes++
		}
	}
	if executes != 2 {
		t.Errorf("%d rules let a tool run a program, want the Wall's own and the GPU's folder", executes)
	}

	// This Mac's name is one rule, and the Wall has it only with it.
	if len(name) != 1 || name[0] != `(allow sysctl-read (sysctl-name "kern.hostname"))` {
		t.Errorf("this Mac's name adds %q", name)
	}
	for _, rule := range append(append([]string{}, base...), gpu...) {
		if strings.Contains(rule, "kern.hostname") || strings.Contains(rule, "kern.proc.") || strings.Contains(rule, "kern.uuid") || strings.Contains(rule, "kern.boottime") {
			t.Errorf("the Wall tells of this Mac: %s", rule)
		}
	}

	// Of /Applications the Wall gives the folder's name, and no listing:
	// what is in it is given by the rules of the Allowed Applications.
	for _, rule := range all {
		if strings.HasPrefix(rule, "(allow ") && strings.Contains(rule, `"/Applications"`) && !strings.HasPrefix(rule, "(allow file-read-metadata file-test-existence (literal ") {
			t.Errorf("the Wall gives /Applications: %s", rule)
		}
	}

	// The hardware's registry: its port is a tool's only with the GPU,
	// and its properties then are a few, of the graphics device and its
	// surfaces alone. A socket is asked what a pipe would answer.
	for _, rule := range base {
		if strings.HasPrefix(rule, "(allow ") && (strings.Contains(rule, "syscall-mig") || strings.Contains(rule, "iokit")) {
			t.Errorf("the Wall reaches the hardware's registry without the GPU: %s", rule)
		}
	}
	for _, rule := range all {
		if !strings.HasPrefix(rule, "(allow ") {
			continue
		}
		switch {
		case strings.Contains(rule, "syscall-mig") && rule != "(allow syscall-mig (kernel-mig-routine host_get_io_master))":
			t.Errorf("the Wall allows a message to the kernel: %s", rule)
		case strings.Contains(rule, "iokit-get-properties") && !strings.HasPrefix(rule, `(allow iokit-get-properties (require-all (iokit-registry-entry-class "IOAccelerator" "IOSurfaceRoot") (iokit-property `):
			t.Errorf("the Wall reads properties of more than the graphics device: %s", rule)
		case strings.Contains(rule, "socket-ioctl") && rule != "(allow socket-ioctl (ioctl-command FIONBIO) (ioctl-command FIONREAD) (ioctl-command FIOCLEX) (ioctl-command FIONCLEX) (ioctl-command FIOASYNC))":
			t.Errorf("the Wall lets a socket be asked: %s", rule)
		case strings.Contains(rule, "syscall-unix"):
			t.Errorf("the Wall allows a call by its number: %s", rule)
		}
	}

	// What "deny default" leaves open is closed by name, each of them.
	for _, closed := range []string{
		"(deny process-info*)", "(deny iokit-get-properties)", "(deny iokit-open)", "(deny nvram*)", "(deny system-info)",
		"(deny syscall-unix (syscall-number SYS_gethostuuid))",
		"(deny syscall-unix (syscall-number SYS_getfsstat) (syscall-number SYS_getfsstat64))",
		"(deny syscall-unix (syscall-number SYS_getlogin))",
		"(deny syscall-unix (syscall-number SYS_fsgetpath) (syscall-number SYS_fsgetpath_ext))",
		"(deny syscall-unix (syscall-number SYS_necp_open) (syscall-number SYS_necp_client_action) (syscall-number SYS_necp_match_policy) (syscall-number SYS_necp_session_open) (syscall-number SYS_necp_session_action))",
		"(deny syscall-unix (syscall-number SYS_shmget) (syscall-number SYS_semget) (syscall-number SYS_msgget) (syscall-number SYS_shmsys) (syscall-number SYS_semsys) (syscall-number SYS_msgsys))",
		"(deny socket-ioctl)",
		"(deny syscall-mig (kernel-mig-routine host_get_io_master))",
		"(deny syscall-mig (kernel-mig-routine io_registry_get_root_entry io_registry_create_iterator io_registry_entry_create_iterator io_registry_iterator_enter_entry io_registry_iterator_exit_entry io_registry_entry_from_path io_registry_entry_from_path_ool io_registry_entry_get_name io_registry_entry_get_name_in_plane io_registry_entry_get_path io_registry_entry_get_path_ool io_registry_entry_get_location_in_plane io_registry_entry_get_child_iterator io_registry_entry_get_parent_iterator io_registry_entry_in_plane io_registry_entry_get_registry_entry_id io_object_get_class io_object_get_superclass io_object_get_bundle_identifier io_catalog_get_data io_catalog_get_gen_count))",
		`(deny file-read* file-test-existence file-map-executable (subpath "/System/Volumes"))`,
		`(deny file-read* file-test-existence (regex #"^/opt/homebrew/Cellar/[^/]+/[^/]+/(INSTALL_RECEIPT\.json|homebrew\.[^/]+\.(plist|service))$"))`,
	} {
		found := false
		for _, rule := range base {
			found = found || rule == closed
		}
		if !found {
			t.Errorf("the Wall does not say %s", closed)
		}
	}
}

// hasCompiler reports whether the sandbox's Host Tools have a compiler
// of that name, in the folders their PATH begins with.
func (w *walled) hasCompiler(name string) bool {
	for _, dir := range w.tools.compilersOf(w.settings).dirs {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// What a Setup adds to the Wall for its Allowed Applications: one rule
// for each, of one shape, and with the GPU one more; the application is
// the rule's parameter, and its name is in no rule.
func TestTheRulesOfTheAllowedApplications(t *testing.T) {
	w := newWalled(t)
	names := []string{"Blender.app", `It's "quoted") (allow default).app`, "微信.app"}
	sc := w.tools.scratchOf("s1")
	for _, gpu := range []bool{false, true} {
		plain := Settings{SandboxID: "s1", SharedFolder: w.shared, GPU: gpu}
		given := plain
		given.Applications = names
		before, after := rules(t, profile(plain)), rules(t, profile(given))
		var want []string
		for n := range names {
			want = append(want, fmt.Sprintf(`(allow file-read* file-test-existence file-map-executable process-exec (subpath (param "APPLICATION_%d")))`, n))
			if gpu {
				want = append(want, fmt.Sprintf(`(allow file-issue-extension (require-all (extension-class "com.apple.app-sandbox.read") (subpath (param "APPLICATION_%d"))))`, n))
			}
		}
		if added := after[len(before):]; !slices.Equal(after[:len(before)], before) || !slices.Equal(added, want) {
			t.Errorf("with the GPU %v, the Allowed Applications add %q, want %q", gpu, added, want)
		}
		for _, name := range names {
			if strings.Contains(profile(given), name) {
				t.Errorf("the Wall's rules hold the name %s", name)
			}
		}
		// The parameters name the two folders, the GPU's, and each
		// application's folder under /Applications, in their order.
		wantParameters := []string{"-D", "SHARED=" + w.shared, "-D", "SCRATCH=" + sc.dir, "-D", "GPU=" + w.tools.gpu.dir}
		for n, name := range names {
			wantParameters = append(wantParameters, "-D", fmt.Sprintf("APPLICATION_%d=/Applications/%s", n, name))
		}
		if got := w.tools.parameters(given, sc, w.shared); !slices.Equal(got, wantParameters) {
			t.Errorf("the Wall's parameters are %q, want %q", got, wantParameters)
		}
	}
	// A sandbox with no Allowed Application has the Wall, and what its
	// Setup adds of the GPU and of this Mac's name, and nothing else.
	if got := profile(Settings{GPU: true, MacName: true}); got != wall+gpuRules+macNameRules {
		t.Error("the Wall of a sandbox with no Allowed Application is not the Wall, the GPU's rules and the name's")
	}

	// An Allowed Application is a folder of /Applications itself: one
	// that is a link, or is not there, is not served, and a tool that
	// would read the link would read where it leads.
	program := w.ownApplications("Given.app")
	if err := os.Symlink(w.outside, filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(program("Given.app"))))), "Linked.app")); err != nil {
		t.Fatal(err)
	}
	if listed, err := w.tools.Applications(); err != nil || !slices.Equal(listed, []string{"Given.app"}) {
		t.Errorf("the applications a Setup can name are %q (%v), want the one folder", listed, err)
	}
	for _, bad := range []string{"Linked.app", "Missing.app", "../outside", ""} {
		settings := w.settings
		settings.Applications = []string{"Given.app", bad}
		if served, err := w.tools.Serve(settings); err == nil {
			served.Close()
			t.Errorf("Host Tools were served with %q as an Allowed Application", bad)
		}
	}
	settings := w.settings
	settings.Applications = []string{"Given.app"}
	served, err := w.tools.Serve(settings)
	if err != nil {
		t.Fatalf("serving Host Tools with an Allowed Application: %v", err)
	}
	defer served.Close()
	if result := served.Run(context.Background(), sandbox.HostToolRequest{Argv: []string{program("Given.app")}}); result.Stdout != "a tool of Given.app\n" {
		t.Errorf("a program of an Allowed Application: %+v", result)
	}
	// The link itself is not read under the Wall, whoever is given what.
	if result := w.sh(settings, "readlink "+shellQuote(filepath.Join(w.tools.applicationsDir, "Linked.app"))); result.ExitCode == 0 || strings.Contains(result.Stdout, w.outside) {
		t.Errorf("a link beside an Allowed Application was read: %+v", result)
	}
}

// metalProbe builds the test's Metal program in the Shared Folder, as a
// Host Tool, or skips the test where this Mac has no Swift compiler.
func (w *walled) metalProbe() {
	w.t.Helper()
	if testing.Short() {
		w.t.Skip("builds a Swift program")
	}
	if !w.hasCompiler("swiftc") {
		w.t.Skip("this Mac has no Swift compiler: the GPU is not tried")
	}
	source, err := os.ReadFile("testdata/metalprobe.swift")
	if err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.shared, "metalprobe.swift"), source, 0o600); err != nil {
		w.t.Fatal(err)
	}
	// Without this Mac's name the compiler cannot ask what it runs on:
	// it is told.
	target := "arm64-apple-macos13.0"
	if runtime.GOARCH == "amd64" {
		target = "x86_64-apple-macos13.0"
	}
	built := w.run(w.settings, sandbox.HostToolRequest{Argv: []string{"swiftc", "-target", target, "-O", "-o", "metalprobe", "metalprobe.swift"}, Seconds: 600})
	if built.ExitCode != 0 {
		w.t.Fatalf("building a Swift program under the Wall: %+v", built)
	}
}

// With the GPU a Host Tool computes on this Mac's graphics device, keeps
// what the compiler built in a binary archive, and has the compiler read
// nothing outside the Wall. Without it, it has no device.
func TestAHostToolComputesOnTheGPU(t *testing.T) {
	w := newWalled(t)
	w.metalProbe()
	probe := func(s Settings, args ...string) sandbox.HostToolResult {
		return w.run(s, sandbox.HostToolRequest{Argv: append([]string{"./metalprobe"}, args...), Seconds: 120})
	}
	if result := probe(w.settings); !strings.Contains(result.Stdout, "NO DEVICE") {
		t.Errorf("without the GPU: %+v", result)
	}
	gpu := w.settings
	gpu.GPU = true
	result := probe(gpu)
	if strings.Contains(result.Stdout, "NO DEVICE") {
		t.Skip("this Mac has no Metal device")
	}
	if !strings.Contains(result.Stdout, "GPU OK") {
		t.Fatalf("with the GPU: %+v", result)
	}

	// A binary archive, which ends the program where the cache folder is
	// not known: the GPU Shim names one in the Scratch, and the archive
	// is made, filled, saved in the Shared Folder and loaded again.
	archive := filepath.Join(w.shared, "kernels.archive")
	if result := probe(gpu, "--archive", "--save", archive); !strings.Contains(result.Stdout, "archive filled") || !strings.Contains(result.Stdout, "GPU OK") || !strings.Contains(result.Stdout, "archive saved") {
		t.Errorf("a binary archive with the GPU: %+v", result)
	}
	if info, err := os.Stat(archive); err != nil || info.Size() == 0 {
		t.Errorf("the archive in the Shared Folder: %v", err)
	}
	if result := probe(gpu, "--load", archive); !strings.Contains(result.Stdout, "GPU OK") {
		t.Errorf("loading the archive: %+v", result)
	}
	// What Metal keeps of what it compiled is in the Scratch.
	if entries, err := os.ReadDir(w.tools.scratchOf("s1").userCache); err != nil || len(entries) == 0 {
		t.Errorf("Metal's caches are not in the Scratch: %v, %v", entries, err)
	}

	// The compiler prints the lines it could not compile: asked to
	// include a file outside the Wall, it finds none, and nothing of the
	// file comes back. Nor is an archive written outside.
	if result := probe(gpu, "--include", w.canary); strings.Contains(result.Stdout+result.Stderr, "CANARY") || !strings.Contains(result.Stdout, "INCLUDE ERROR") {
		t.Errorf("the compiler, asked to include a file outside the Wall: %+v", result)
	}
	if result := probe(gpu, "--archive", "--save", filepath.Join(w.outside, "kernels.archive")); strings.Contains(result.Stdout, "archive saved") {
		t.Errorf("an archive saved outside the Wall: %+v", result)
	}
	if entries, _ := os.ReadDir(w.outside); len(entries) != 1 {
		t.Errorf("outside the Wall there is now %v", entries)
	}
	// It is in a file the tool may read that the compiler does look.
	if err := os.WriteFile(filepath.Join(w.shared, "mine.h"), []byte("MINE-in-the-shared-folder\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := probe(gpu, "--include", filepath.Join(w.shared, "mine.h")); !strings.Contains(result.Stdout, "MINE") {
		t.Errorf("the compiler, asked to include a file of the Shared Folder: %+v", result)
	}
}

// With the GPU a Host Tool encodes and decodes video with the hardware:
// the GPU Shim answers what the system would ask its trust service,
// which the Wall refuses. Without the GPU the hardware is not found.
func TestAHostToolEncodesVideoWithTheHardware(t *testing.T) {
	if testing.Short() {
		t.Skip("encodes video")
	}
	if _, err := os.Stat("/opt/homebrew/bin/ffmpeg"); err != nil {
		t.Skip("this Mac has no ffmpeg: the hardware's video encoder is not tried")
	}
	w := newWalled(t)
	ffmpeg := func(s Settings, args ...string) sandbox.HostToolResult {
		return w.run(s, sandbox.HostToolRequest{Argv: append([]string{"ffmpeg", "-hide_banner", "-loglevel", "error", "-y"}, args...), Seconds: 300})
	}
	if result := ffmpeg(w.settings, "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30", "-t", "1", "-c:v", "libx264", "-pix_fmt", "yuv420p", "source.mp4"); result.ExitCode != 0 {
		t.Fatalf("encoding in software under the Wall: %+v", result)
	}
	if result := ffmpeg(w.settings, "-i", "source.mp4", "-c:v", "h264_videotoolbox", "-b:v", "1M", "no-gpu.mp4"); result.ExitCode == 0 {
		t.Errorf("the hardware encoded without the GPU: %+v", result)
	}
	gpu := w.settings
	gpu.GPU = true
	for _, encoder := range []string{"h264_videotoolbox", "hevc_videotoolbox"} {
		out := encoder + ".mp4"
		if result := ffmpeg(gpu, "-i", "source.mp4", "-c:v", encoder, "-b:v", "1M", out); result.ExitCode != 0 {
			if strings.Contains(result.Stderr, "Unknown encoder") {
				t.Skipf("this ffmpeg has no %s", encoder)
			}
			t.Errorf("%s with the GPU: %+v", encoder, result)
			continue
		}
		probe := w.run(gpu, sandbox.HostToolRequest{Argv: []string{"ffprobe", "-hide_banner", "-loglevel", "error", "-select_streams", "v:0", "-show_entries", "stream=codec_name,nb_frames", "-of", "csv=p=0", out}})
		if want := strings.TrimSuffix(encoder, "_videotoolbox") + ",30"; strings.TrimSpace(probe.Stdout) != want {
			t.Errorf("%s wrote %q, want %s", encoder, strings.TrimSpace(probe.Stdout), want)
		}
	}
	// Decoded by the hardware and encoded again.
	if result := ffmpeg(gpu, "-hwaccel", "videotoolbox", "-i", "hevc_videotoolbox.mp4", "-c:v", "h264_videotoolbox", "-b:v", "1M", "round.mp4"); result.ExitCode != 0 {
		t.Errorf("decoding and encoding with the hardware: %+v", result)
	}

	// The hardware is there too when a shell starts ffmpeg, or a program
	// of this Mac's own that is no shell: each of them drops the loader's
	// variable, which bash sets again from the Shell Startup File and
	// ffmpeg's Homebrew Wrapper sets for ffmpeg.
	const encode = "ffmpeg -hide_banner -loglevel error -y -i source.mp4 -c:v h264_videotoolbox -b:v 1M "
	for out, argv := range map[string][]string{
		"by-sh.mp4":   {"sh", "-c", encode + "by-sh.mp4"},
		"by-bash.mp4": {"bash", "-c", encode + "by-bash.mp4"},
		"by-zsh.mp4":  {"zsh", "-c", encode + "by-zsh.mp4"},
		"by-find.mp4": append(append([]string{"find", "source.mp4", "-exec"}, strings.Fields(encode)...), "by-find.mp4", ";"),
		"by-env.mp4":  append(append([]string{"env"}, strings.Fields(encode)...), "by-env.mp4"),
	} {
		if result := w.run(gpu, sandbox.HostToolRequest{Argv: argv, Seconds: 300}); result.ExitCode != 0 {
			t.Errorf("the hardware's encoder, started by %q: %+v", argv[0], result)
			continue
		}
		probe := w.run(gpu, sandbox.HostToolRequest{Argv: []string{"ffprobe", "-hide_banner", "-loglevel", "error", "-select_streams", "v:0", "-show_entries", "stream=codec_name,nb_frames", "-of", "csv=p=0", out}})
		if strings.TrimSpace(probe.Stdout) != "h264,30" {
			t.Errorf("started by %q, the hardware's encoder wrote %q, want h264,30", argv[0], strings.TrimSpace(probe.Stdout))
		}
	}
	// Without the GPU a shell finds no hardware either.
	if result := w.run(w.settings, sandbox.HostToolRequest{Argv: []string{"sh", "-c", encode + "no-gpu-by-sh.mp4"}, Seconds: 300}); result.ExitCode == 0 {
		t.Errorf("the hardware encoded for a shell without the GPU: %+v", result)
	}
}

// registryProbe builds the test's registry program in the Shared Folder,
// as a Host Tool, or skips the test where this Mac has no C compiler.
func (w *walled) registryProbe() {
	w.t.Helper()
	if testing.Short() {
		w.t.Skip("builds a C program")
	}
	if !w.hasCompiler("cc") {
		w.t.Skip("this Mac has no C compiler: the hardware's registry is not tried")
	}
	source, err := os.ReadFile("testdata/registryprobe.c")
	if err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.shared, "registryprobe.c"), source, 0o600); err != nil {
		w.t.Fatal(err)
	}
	built := w.run(w.settings, sandbox.HostToolRequest{Argv: []string{"cc", "-O", "-framework", "IOKit", "-framework", "CoreFoundation", "-o", "registryprobe", "registryprobe.c"}, Seconds: 300})
	if built.ExitCode != 0 {
		w.t.Fatalf("building a C program under the Wall: %+v", built)
	}
}

// The hardware's registry names every device and interface of this Mac.
// A Host Tool reaches none of it; with the GPU it finds a device by its
// class, as Metal does the graphics device, and still walks nothing and
// reads no name, place or class of what it found. With no Wall the same
// program does all of it, so each refusal is the Wall's.
func TestTheHardwareRegistryIsNotAHostTools(t *testing.T) {
	w := newWalled(t)
	w.registryProbe()
	said := func(out string) map[string]string {
		got := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if name, verdict, ok := strings.Cut(line, " "); ok {
				got[name] = verdict
			}
		}
		return got
	}
	all := []string{
		"walk-the-registry", "find-a-device-by-its-class", "name-a-device", "path-of-a-device", "class-of-a-device",
		"number-of-a-device", "parent-of-a-device", "children-of-a-device", "class-of-a-device-by-a-property", "serial-number-of-this-mac",
	}
	request := sandbox.HostToolRequest{Argv: []string{"./registryprobe"}}

	result := w.run(w.settings, request)
	got := said(result.Stdout)
	if result.ExitCode != 0 || len(got) != len(all) {
		t.Fatalf("the probe under the Wall: %+v", result)
	}
	for _, name := range all {
		if got[name] != "refused" {
			t.Errorf("under the Wall, %s: %q, want refused", name, got[name])
		}
	}

	gpu := w.settings
	gpu.GPU = true
	result = w.run(gpu, request)
	got = said(result.Stdout)
	if result.ExitCode != 0 || len(got) != len(all) {
		t.Fatalf("the probe with the GPU: %+v", result)
	}
	for _, name := range all {
		want := "refused"
		if name == "find-a-device-by-its-class" {
			want = "ok"
		}
		if got[name] != want {
			t.Errorf("with the GPU, %s: %q, want %s", name, got[name], want)
		}
	}

	control := exec.Command(filepath.Join(w.shared, "registryprobe"))
	out, err := control.Output()
	if err != nil {
		t.Fatalf("the probe without the Wall: %v", err)
	}
	if got := said(string(out)); len(got) != len(all) {
		t.Fatalf("the probe without the Wall said %v", got)
	} else {
		for name, verdict := range got {
			if verdict != "ok" {
				t.Errorf("without the Wall, %s: %q: the test cannot tell what the Wall refuses", name, verdict)
			}
		}
	}
}

// What Homebrew wrote down of an installation, at the top of a
// package's folder, holds paths of the home it was installed from, and
// so its user's name: its receipt, and the files that start a service
// of it. A Host Tool reads neither, and the rest of the package as
// before.
func TestHomebrewsReceiptsAreNotRead(t *testing.T) {
	receipts, _ := filepath.Glob("/opt/homebrew/Cellar/*/*/INSTALL_RECEIPT.json")
	if len(receipts) == 0 {
		t.Skip("this Mac has no receipt of Homebrew's")
	}
	services, _ := filepath.Glob("/opt/homebrew/Cellar/*/*/homebrew.*.plist")
	more, _ := filepath.Glob("/opt/homebrew/Cellar/*/*/homebrew.*.service")
	w := newWalled(t)
	for _, file := range append(append([]string{receipts[0]}, first(services)...), first(more)...) {
		if _, err := os.Stat(file); err != nil {
			t.Fatalf("without the Wall: %v", err)
		}
		if result := w.run(w.settings, sandbox.HostToolRequest{Argv: []string{"/bin/cat", file}}); result.ExitCode == 0 || result.Stdout != "" {
			t.Errorf("a Host Tool read %s (exit %d, %d bytes)", file, result.ExitCode, len(result.Stdout))
		}
		if result := w.run(w.settings, sandbox.HostToolRequest{Argv: []string{"/usr/bin/stat", file}}); result.ExitCode == 0 {
			t.Errorf("a Host Tool was told of %s", file)
		}
	}
	// The package's folder is read as before, by its own path and by
	// the one Homebrew links to it.
	if result := w.run(w.settings, sandbox.HostToolRequest{Argv: []string{"/bin/ls", filepath.Dir(receipts[0])}}); result.ExitCode != 0 {
		t.Errorf("a package's folder under the Wall: %+v", result)
	}
}

// first is the first of some names, or none.
func first(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	return names[:1]
}
