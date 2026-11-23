package hosttool

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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
		for _, never := range []string{"(allow default", "network", "trustd", "opendirectoryd", "notification_center", "pasteboard", "windowserver", "coreservicesd", "securityd", "launchservicesd", "appleevent", "nvram", "system-socket", "(subpath \"/\")", "(subpath \"/Users\")", "(subpath \"/Volumes\")", "(subpath \"/private\")"} {
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
		case `(allow file-issue-extension (require-all (extension-class "com.apple.app-sandbox.read") (require-any (subpath "/Applications") (subpath (param "SHARED")) (subpath (param "SCRATCH")))))`,
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

	// What "deny default" leaves open is closed by name, each of them.
	for _, closed := range []string{
		"(deny process-info*)", "(deny iokit-get-properties)", "(deny iokit-open)", "(deny nvram*)", "(deny system-info)",
		"(deny syscall-unix (syscall-number SYS_gethostuuid))",
		"(deny syscall-unix (syscall-number SYS_getfsstat) (syscall-number SYS_getfsstat64))",
		`(deny file-read* file-test-existence file-map-executable (subpath "/System/Volumes"))`,
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

// metalProbe builds the test's Metal program in the Shared Folder, as a
// Host Tool, or skips the test where this Mac has no Swift compiler.
func (w *walled) metalProbe() {
	w.t.Helper()
	if testing.Short() {
		w.t.Skip("builds a Swift program")
	}
	if _, err := os.Stat(filepath.Join(w.tools.developerDir, "Toolchains/XcodeDefault.xctoolchain/usr/bin/swiftc")); err != nil {
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
