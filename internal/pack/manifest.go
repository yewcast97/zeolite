// Package pack reads Capability Packs: manifests that add a tool to a
// sandbox without a change to the app.
package pack

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"zeolite/internal/sandbox"
)

// Manifest is one Capability Pack. A manifest is trusted input: its
// RuntimeLayer runs as root when the workstation image is built.
type Manifest struct {
	// Name identifies the pack in a sandbox's config.
	Name        string `yaml:"name"`
	Title       string `yaml:"title"`
	Description string `yaml:"description"`

	RuntimeLayer   RuntimeLayer   `yaml:"runtimeLayer"`
	DaemonProcess  *DaemonProcess `yaml:"daemonProcess"`
	MCPBridge      *MCPBridge     `yaml:"mcpBridge"`
	ViewportStream ViewportStream `yaml:"viewportStream"`
}

// RuntimeLayer is what the pack adds to the workstation image and to the
// workstation's environment.
type RuntimeLayer struct {
	// Packages are Debian packages installed when the image is built.
	Packages []string `yaml:"packages"`
	// Run are shell commands run as root when the image is built, after
	// the packages are installed.
	Run []string `yaml:"run"`
	// Env is added to the workstation's environment.
	Env map[string]string `yaml:"env"`
	// Volumes are Docker named volumes mounted read-only, for tools
	// installed outside the image.
	Volumes []Volume `yaml:"volumes"`
	// Files are added to the image: absolute path to content.
	Files map[string]string `yaml:"files"`
}

// Volume is a Docker named volume and where the workstation sees it.
type Volume struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

// DaemonProcess is the pack's background process in the workstation and
// how to tell that it is ready.
type DaemonProcess struct {
	Command []string `yaml:"command"`
	// ReadinessPort is the port on the sandbox's 127.0.0.1 that accepts
	// connections once the daemon is ready.
	ReadinessPort int `yaml:"readinessPort"`
}

// MCPBridge is the pack's MCP server, registered for the agent in the
// sandbox.
type MCPBridge struct {
	Name    string            `yaml:"name"`
	Command []string          `yaml:"command"`
	Env     map[string]string `yaml:"env"`
}

// StreamType is how a pack's display reaches the dashboard.
type StreamType string

const (
	// StreamNone: the pack has nothing to show.
	StreamNone StreamType = "none"
	// StreamCDP: frames from a Chromium's screencast, and input into it,
	// over the Chrome DevTools Protocol.
	StreamCDP StreamType = "cdp_screencast"
)

// ViewportStream is the pack's display.
type ViewportStream struct {
	Type StreamType `yaml:"type"`
	// Port is the port on the sandbox's 127.0.0.1 the stream is read from.
	Port int `yaml:"port"`
}

// Parse reads and validates a manifest. Unknown keys are errors, so that
// a misspelled one is not silently ignored.
func Parse(data []byte) (Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, err
	}
	if m.ViewportStream.Type == "" {
		m.ViewportStream.Type = StreamNone
	}
	if err := m.validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

var (
	namePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
	packagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9+.:=~-]*$`)
	volumePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

// reservedVolumePrefix begins the names of what the app makes in Docker,
// a sandbox's Home among it. A pack's volume has no such name: it would
// otherwise mount another sandbox's Home.
const reservedVolumePrefix = "zeolite-"

func (m Manifest) validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if !namePattern.MatchString(m.Name) {
		bad("name %q must be lower-case letters, digits and hyphens", m.Name)
	}
	if strings.TrimSpace(m.Title) == "" {
		bad("title is empty")
	}

	for _, p := range m.RuntimeLayer.Packages {
		if !packagePattern.MatchString(p) {
			bad("runtimeLayer.packages: %q is not a package name", p)
		}
	}
	checkEnv := func(where string, env map[string]string) {
		for k := range env {
			if !sandbox.EnvironmentName(k) {
				bad("%s: %q is not a variable name", where, k)
			} else if sandbox.ReservedEnvironment[k] {
				bad("%s: %s is set by the app for every sandbox", where, k)
			}
		}
	}
	checkEnv("runtimeLayer.env", m.RuntimeLayer.Env)
	for _, v := range m.RuntimeLayer.Volumes {
		if !volumePattern.MatchString(v.Name) {
			bad("runtimeLayer.volumes: %q is not a volume name", v.Name)
		} else if strings.HasPrefix(v.Name, reservedVolumePrefix) {
			bad("runtimeLayer.volumes: %q begins with %s, as the sandboxes' Homes do: a pack mounts none of them", v.Name, reservedVolumePrefix)
		}
		if !path.IsAbs(v.Path) || path.Clean(v.Path) != v.Path || v.Path == "/" {
			bad("runtimeLayer.volumes: %q is not a clean absolute path", v.Path)
		}
	}

	for name := range m.RuntimeLayer.Files {
		if !path.IsAbs(name) || path.Clean(name) != name || strings.HasSuffix(name, "/") {
			bad("runtimeLayer.files: %q is not a clean absolute path", name)
		}
	}

	port := func(where string, p int) {
		if p < 1 || p > 65535 {
			bad("%s must be a port from 1 to 65535", where)
		}
	}
	if d := m.DaemonProcess; d != nil {
		if len(d.Command) == 0 {
			bad("daemonProcess.command is empty")
		}
		port("daemonProcess.readinessPort", d.ReadinessPort)
	}
	if b := m.MCPBridge; b != nil {
		if !namePattern.MatchString(b.Name) {
			bad("mcpBridge.name %q must be lower-case letters, digits and hyphens", b.Name)
		}
		if b.Name == sandbox.HostToolServer {
			bad("mcpBridge.name %q is the server by which an agent runs Host Tools: a pack's has another name", b.Name)
		}
		if b.Name == sandbox.StateServer {
			bad("mcpBridge.name %q is the server by which an agent tells its user of its state: a pack's has another name", b.Name)
		}
		if len(b.Command) == 0 {
			bad("mcpBridge.command is empty")
		}
		checkEnv("mcpBridge.env", b.Env)
	}

	switch s := m.ViewportStream; s.Type {
	case StreamNone:
		if s.Port != 0 {
			bad("viewportStream.port applies only with a stream")
		}
	case StreamCDP:
		port("viewportStream.port", s.Port)
		if m.DaemonProcess == nil {
			bad("viewportStream needs a daemonProcess to stream from")
		}
	default:
		bad("viewportStream.type %q is not a stream type", s.Type)
	}
	return errors.Join(errs...)
}
