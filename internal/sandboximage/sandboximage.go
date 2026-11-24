// Package sandboximage assembles what Docker needs to build a sandbox's
// two images: the gatekeeper's, and the workstation's with the layers of
// the Capability Packs a sandbox enables.
//
// The Linux binaries that run in those images are built from cmd/ by
// `go generate` and embedded here, so the app carries them.
package sandboximage

//go:generate env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/gatekeeper-arm64 ../../cmd/gatekeeper
//go:generate env GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/gatekeeper-amd64 ../../cmd/gatekeeper
//go:generate env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/tun-arm64 ../../cmd/tun
//go:generate env GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/tun-amd64 ../../cmd/tun
//go:generate env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/supervisor-arm64 ../../cmd/supervisor
//go:generate env GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/supervisor-amd64 ../../cmd/supervisor

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"zeolite/internal/pack"
)

//go:embed bin/*
var binaries embed.FS

// Image is a Docker image to build: the tag names its content, so an
// image that exists under the tag never needs building again.
type Image struct {
	Tag string
	// Context is the build context, a tar archive with a Dockerfile.
	Context []byte
}

// Paths in the images that the app refers to when it runs them.
const (
	GatekeeperPath = "/usr/local/bin/gatekeeper"
	// TunPath is the program that makes a sandbox's TUN device and moves
	// its packets. It is in the gatekeeper's image.
	TunPath        = "/usr/local/bin/tun"
	SupervisorPath = "/usr/local/bin/supervisor"
	// WorkstationUser is the user everything in a workstation runs as.
	WorkstationUser = "1000:1000"
)

const gatekeeperDockerfile = `FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates nftables \
 && rm -rf /var/lib/apt/lists/*
COPY gatekeeper ` + GatekeeperPath + `
COPY tun ` + TunPath + `
ENTRYPOINT ["` + GatekeeperPath + `", "run"]
`

// The workstation: Node.js and Python for the agent's own work, and what
// every sandbox needs whatever its packs: the timezones' data for the
// Clock Policy, socat for the Viewport Stream's connection, and the
// supervisor.
//
// A workstation's user is not root and its root is read-only: what a
// package of Debian's holds and the image lacks, nothing installs while
// the sandbox runs. So the image has what that work takes for granted
// where one develops: the compiler, make and Python's headers, with
// which npm and pip build a package that none is built of for the
// workstation's architecture; the ssh client, by which git reaches a
// remote that is named for it; and unzip and xz, which installers and
// archives ask for.
const workstationDockerfile = `FROM node:22-bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      bash build-essential ca-certificates curl git less openssh-client procps \
      python3 python3-dev python3-pip python3-venv socat tzdata unzip vim-tiny xz-utils \
 && rm -rf /var/lib/apt/lists/* \
 && userdel --remove node \
 && useradd --uid 1000 --home-dir /workspace --no-create-home --shell /bin/bash agent
COPY supervisor ` + SupervisorPath + `
COPY profile ` + profilePath + `
`

// The workstation's root is read-only, so what its user installs goes
// under the home: npm's global packages are sent there.
//
// The home is the sandbox's Home, a volume mounted at /workspace. Docker
// gives an empty volume the owner and the mode of the image's folder it
// is mounted over, and copies what that folder holds into it: so the
// last step as root leaves /workspace empty and the agent's, after every
// pack's layer. What a pack wrote there would otherwise be copied into
// a Home once, and never updated.
//
// /tmp is a volume too, made with each workstation: the same step
// leaves the image's empty, and anyone's to write as /tmp is.
const workstationDockerfileEnd = `RUN rm -rf /workspace && install -d -o 1000 -g 1000 -m 0755 /workspace
RUN rm -rf /tmp && install -d -m 1777 /tmp
ENV HOME=/workspace LANG=C.UTF-8 NPM_CONFIG_PREFIX=` + npmPrefix + `
USER ` + WorkstationUser + `
WORKDIR /workspace
ENTRYPOINT ["` + SupervisorPath + `", "run"]
`

// npmPrefix is where npm puts global packages in a workstation.
const npmPrefix = "/workspace/.npm-global"

// profilePath is the file every login shell of a workstation reads.
const profilePath = "/etc/profile.d/zeolite.sh"

// profile puts on a login shell's PATH the places under the home where
// an agent's own installer leaves its executable, and reads the
// ~/.bashrc such an installer may have written its own place in. A
// Terminal Session is a login shell: what its user installed there is
// started there by its name, with no restart.
//
// It has a prompt begin a line of its own, too: a program that ended
// without ending its line, as one that prints an address and nothing
// after it, would have the prompt stand behind what it printed. Before
// each prompt the shell tells the terminal to begin a line where it is
// not at the beginning of one (OSC 133;L), which the dashboard's
// terminal does itself, with nothing read from it. That comes before
// what a ~/.bashrc has the shell do at a prompt, and keeps the status
// of what ran for it.
const profile = `# Where what the workstation's user installs is found.
for dir in \
  "$HOME/.local/bin" \
  "$HOME/bin" \
  "` + npmPrefix + `/bin" \
  "$HOME/.bun/bin" \
  "$HOME/.opencode/bin" \
  "$HOME/.grok/bin" \
  "$HOME/.kimi-code/bin"
do
  case ":$PATH:" in
    *":$dir:"*) ;;
    *) PATH="$dir:$PATH" ;;
  esac
done
export PATH
if [ -n "$BASH_VERSION" ]; then
  if [ -r "$HOME/.bashrc" ]; then
    . "$HOME/.bashrc"
  fi
  # A prompt begins a line of its own.
  ` + beginLine + `() {
    local status=$?
    printf '\033]133;L\007'
    return $status
  }
  case ";${PROMPT_COMMAND-};" in
    *";` + beginLine + `;"*) ;;
    *) PROMPT_COMMAND="` + beginLine + `${PROMPT_COMMAND:+;$PROMPT_COMMAND}" ;;
  esac
fi
`

// beginLine is the profile's function that has the terminal begin a line
// for a prompt.
const beginLine = "zeolite_begin_line"

// Gatekeeper returns the gatekeeper's image for a CPU architecture as
// Docker names it ("arm64", "amd64").
func Gatekeeper(arch string) (Image, error) {
	binary, err := binaries.ReadFile("bin/gatekeeper-" + normalize(arch))
	if err != nil {
		return Image{}, fmt.Errorf("no gatekeeper built for %s: run `go generate ./internal/sandboximage`", arch)
	}
	tun, err := binaries.ReadFile("bin/tun-" + normalize(arch))
	if err != nil {
		return Image{}, fmt.Errorf("no tun built for %s: run `go generate ./internal/sandboximage`", arch)
	}
	return build("zeolite-gatekeeper", gatekeeperDockerfile, contextFile{"gatekeeper", 0o755, binary}, contextFile{"tun", 0o755, tun})
}

// Workstation returns the workstation's image with the layers of packs.
// The packs' order does not change the image.
func Workstation(arch string, packs []pack.Manifest) (Image, error) {
	binary, err := binaries.ReadFile("bin/supervisor-" + normalize(arch))
	if err != nil {
		return Image{}, fmt.Errorf("no supervisor built for %s: run `go generate ./internal/sandboximage`", arch)
	}
	packs = append([]pack.Manifest(nil), packs...)
	sort.Slice(packs, func(i, j int) bool { return packs[i].Name < packs[j].Name })

	var b strings.Builder
	var files []contextFile
	b.WriteString(workstationDockerfile)
	for _, p := range packs {
		layer := p.RuntimeLayer
		if len(layer.Packages) == 0 && len(layer.Run) == 0 && len(layer.Files) == 0 {
			continue
		}
		fmt.Fprintf(&b, "# Capability Pack %s\n", p.Name)
		paths := make([]string, 0, len(layer.Files))
		for path := range layer.Files {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			// In the context a file is named by its content, so that two
			// packs cannot collide and a changed file changes the tag.
			sum := sha256.Sum256([]byte(layer.Files[path]))
			name := "file-" + hex.EncodeToString(sum[:8])
			files = append(files, contextFile{name, 0o644, []byte(layer.Files[path])})
			fmt.Fprintf(&b, "COPY %s %s\n", name, quote(path))
		}
		if len(layer.Packages) > 0 {
			fmt.Fprintf(&b, "RUN apt-get update \\\n && apt-get install -y --no-install-recommends %s \\\n && rm -rf /var/lib/apt/lists/*\n",
				strings.Join(layer.Packages, " "))
		}
		for _, command := range layer.Run {
			// The JSON form of RUN, so that the command reaches the
			// shell as the manifest wrote it.
			fmt.Fprintf(&b, "RUN [\"/bin/sh\", \"-c\", %s]\n", quote(command))
		}
	}
	b.WriteString(workstationDockerfileEnd)
	return build("zeolite-workstation", b.String(), append(files, contextFile{"supervisor", 0o755, binary}, contextFile{"profile", 0o644, []byte(profile)})...)
}

// contextFile is a file of a build context.
type contextFile struct {
	name string
	mode int64
	data []byte
}

// build makes the context and names the image after its content.
func build(repository, dockerfile string, files ...contextFile) (Image, error) {
	files = append([]contextFile{{"Dockerfile", 0o644, []byte(dockerfile)}}, files...)
	sum := sha256.New()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		sum.Write([]byte(f.name))
		sum.Write(f.data)
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data))}); err != nil {
			return Image{}, err
		}
		if _, err := tw.Write(f.data); err != nil {
			return Image{}, err
		}
	}
	if err := tw.Close(); err != nil {
		return Image{}, err
	}
	tag := repository + ":" + hex.EncodeToString(sum.Sum(nil))[:16]
	return Image{Tag: tag, Context: buf.Bytes()}, nil
}

// normalize maps the names Docker and the kernel use for an architecture
// to Go's.
func normalize(arch string) string {
	switch arch {
	case "aarch64", "arm64":
		return "arm64"
	case "x86_64", "amd64":
		return "amd64"
	}
	return arch
}

// quote writes s as a JSON string, which is what a Dockerfile's exec
// form takes.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
