package sandboximage

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"zeolite/internal/pack"
)

// dockerfile is the Dockerfile of an image's context.
func dockerfile(i Image) string {
	tr := tar.NewReader(bytes.NewReader(i.Context))
	for {
		h, err := tr.Next()
		if err != nil {
			return ""
		}
		if h.Name == "Dockerfile" {
			data, _ := io.ReadAll(tr)
			return string(data)
		}
	}
}

func manifest(t *testing.T, yaml string) pack.Manifest {
	t.Helper()
	m, err := pack.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestGatekeeperImage(t *testing.T) {
	img, err := Gatekeeper("aarch64")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(img.Tag, "zeolite-gatekeeper:") {
		t.Errorf("tag %q", img.Tag)
	}
	if df := dockerfile(img); !strings.Contains(df, "nftables") || !strings.Contains(df, "COPY tun /usr/local/bin/tun") || !strings.Contains(df, `ENTRYPOINT ["/usr/local/bin/gatekeeper", "run"]`) {
		t.Errorf("Dockerfile:\n%s", df)
	}
	if _, err := Gatekeeper("riscv64"); err == nil {
		t.Error("an architecture with no binary got an image")
	}
}

func TestWorkstationImageHasThePacksLayers(t *testing.T) {
	browser := manifest(t, "name: browser\ntitle: Browser\nruntimeLayer:\n  packages: [chromium, fonts-liberation]\n  run: ['npm install --global \"some-tool\"']\n  files: {/usr/local/lib/tool/run.py: 'print(1)'}\n")
	plain := manifest(t, "name: aaa-plain\ntitle: Plain\n")

	base, err := Workstation("arm64", nil)
	if err != nil {
		t.Fatal(err)
	}
	with, err := Workstation("arm64", []pack.Manifest{browser, plain})
	if err != nil {
		t.Fatal(err)
	}
	if base.Tag == with.Tag {
		t.Error("a pack with layers did not change the image's tag")
	}
	df := dockerfile(with)
	for _, want := range []string{
		"apt-get install -y --no-install-recommends chromium fonts-liberation",
		`RUN ["/bin/sh", "-c", "npm install --global \"some-tool\""]`,
		"USER 1000:1000",
		`ENTRYPOINT ["/usr/local/bin/supervisor", "run"]`,
		"COPY profile /etc/profile.d/zeolite.sh",
		"NPM_CONFIG_PREFIX=/workspace/.npm-global",
		` "/usr/local/lib/tool/run.py"`,
	} {
		if !strings.Contains(df, want) {
			t.Errorf("the Dockerfile lacks %q:\n%s", want, df)
		}
	}
	// Packs run as root at build time, and the image ends as the agent's user.
	if strings.Index(df, "chromium") > strings.Index(df, "USER 1000:1000") {
		t.Error("the pack's layer comes after the switch to the workstation's user")
	}

	// A pack's own global packages are installed where the image keeps
	// them: npm is sent under the home only once the packs are in.
	if strings.Index(df, "some-tool") > strings.Index(df, "NPM_CONFIG_PREFIX") {
		t.Error("npm is sent under the home before the packs' layers")
	}

	// The image's /workspace is left empty and the agent's by the last
	// step as root, after every pack's layer: an empty Home takes its
	// owner and its mode, and nothing a pack wrote there.
	home := "RUN rm -rf /workspace && install -d -o 1000 -g 1000 -m 0755 /workspace"
	if at := strings.Index(df, home); at < 0 || at < strings.Index(df, "some-tool") || at > strings.Index(df, "USER 1000:1000") {
		t.Errorf("the Dockerfile does not empty /workspace for the agent after the packs' layers and before the switch of user:\n%s", df)
	}
	if bare := dockerfile(base); !strings.Contains(bare, home) || strings.Contains(bare, "--create-home") {
		t.Errorf("a workstation without packs does not leave /workspace empty and the agent's:\n%s", bare)
	}

	// The image's /tmp is left empty and anyone's to write the same way:
	// a workstation's /tmp is a volume made anew with it, which takes
	// what the image's holds.
	tmp := "RUN rm -rf /tmp && install -d -m 1777 /tmp"
	if at := strings.Index(df, tmp); at < 0 || at < strings.Index(df, "some-tool") || at > strings.Index(df, "USER 1000:1000") {
		t.Errorf("the Dockerfile does not empty /tmp after the packs' layers and before the switch of user:\n%s", df)
	}

	// A pack without layers, and the order of packs, change nothing.
	reordered, _ := Workstation("arm64", []pack.Manifest{plain, browser})
	onlyBrowser, _ := Workstation("arm64", []pack.Manifest{browser})
	if reordered.Tag != with.Tag || onlyBrowser.Tag != with.Tag {
		t.Errorf("tags %q, %q, %q differ for the same layers", with.Tag, reordered.Tag, onlyBrowser.Tag)
	}
}

// A workstation's user is not root and its root is read-only, so what
// its work needs of Debian's packages is in every workstation's image,
// whatever its packs: the compiler and make, Python's headers, the ssh
// client, unzip and xz.
func TestEveryWorkstationImageHasWhatBuildsAndUnpacks(t *testing.T) {
	base, err := Workstation("arm64", nil)
	if err != nil {
		t.Fatal(err)
	}
	df := dockerfile(base)
	// The packages of the image's own step, which is its first.
	install, _, _ := strings.Cut(df, "&& rm -rf /var/lib/apt/lists/*")
	installed := strings.Fields(install)
	for _, pkg := range []string{"build-essential", "python3-dev", "openssh-client", "unzip", "xz-utils"} {
		if !slices.Contains(installed, pkg) {
			t.Errorf("the image's own step does not install %s:\n%s", pkg, install)
		}
	}
}

// What the workstation's user installs under the home is found by a
// login shell by its name: in the places installers use, and in the one
// an installer wrote into ~/.bashrc.
func TestTheProfileFindsWhatTheUserInstalled(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash to read the profile with")
	}
	home := t.TempDir()
	file := filepath.Join(home, "profile.sh")
	if err := os.WriteFile(file, []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	put := func(dir, name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	put(".local/bin", "agy")
	put("bin", "mine")
	put(".bun/bin", "omp")
	put(".opencode/bin", "opencode")
	put(".grok/bin", "grok")
	put(".kimi-code/bin", "kimi")
	put("elsewhere/bin", "aider")
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte(`export PATH="$HOME/elsewhere/bin:$PATH"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Read twice, as a shell started from a shell reads it: the PATH does
	// not grow.
	script := `. "$1"; once=$PATH; . "$1"; for e in agy mine omp opencode grok kimi aider nothing; do type -P "$e" >/dev/null && printf '%s ' "$e"; done; [ "$once" = "${PATH#"$HOME/elsewhere/bin:"}" ] && echo same`
	cmd := exec.Command(bash, "-c", script, "bash", file)
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "agy mine omp opencode grok kimi aider same" {
		t.Errorf("a login shell finds %q", got)
	}
	if !strings.Contains(profile, `"`+npmPrefix+`/bin"`) {
		t.Error("where npm puts global packages is not on the PATH")
	}
}

// A prompt begins a line of its own: before each one the shell tells
// the terminal to begin a line where it is not at the beginning of one,
// first of what it does at a prompt, once however often the profile is
// read, and with the status of what ran kept for what a ~/.bashrc does
// there.
func TestTheProfileBeginsAPromptOnALineOfItsOwn(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash to read the profile with")
	}
	home := t.TempDir()
	file := filepath.Join(home, "profile.sh")
	if err := os.WriteFile(file, []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte("PROMPT_COMMAND='status=$?'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The shell does at a prompt what PROMPT_COMMAND holds, after the
	// command before it: here one that ended with 7.
	script := `. "$1"; . "$1"; printf '%s|' "$PROMPT_COMMAND"; (exit 7); eval "$PROMPT_COMMAND"; printf '|%s' "$status"`
	cmd := exec.Command(bash, "-c", script, "bash", file)
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if want := beginLine + ";status=$?|\x1b]133;L\a|7"; string(out) != want {
		t.Errorf("at a prompt the shell does and prints %q, want %q", out, want)
	}
}
