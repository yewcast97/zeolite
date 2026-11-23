package pack

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"zeolite/internal/sandbox"
)

//go:embed builtin/*.yaml
var builtin embed.FS

// Catalog holds the Capability Packs a sandbox can enable.
type Catalog struct {
	packs map[string]Manifest
	names []string // in the order the dashboard lists them
}

// NewCatalog returns a catalog of the built-in packs.
func NewCatalog() (*Catalog, error) {
	c := &Catalog{packs: map[string]Manifest{}}
	files, err := fs.Glob(builtin, "builtin/*.yaml")
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	for _, f := range files {
		data, err := builtin.ReadFile(f)
		if err != nil {
			return nil, err
		}
		if err := c.add(f, data); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// LoadDir adds the user's packs, the *.yaml files of dir, after the
// built-in ones. It returns an error for each file it left out; a missing
// directory is no error.
func (c *Catalog) LoadDir(dir string) []error {
	files, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
	sort.Strings(files)
	var errs []error
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err == nil {
			err = c.add(f, data)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func (c *Catalog) add(file string, data []byte) error {
	m, err := Parse(data)
	if err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	if _, taken := c.packs[m.Name]; taken {
		return fmt.Errorf("%s: a Capability Pack named %q already exists", file, m.Name)
	}
	c.packs[m.Name] = m
	c.names = append(c.names, m.Name)
	return nil
}

// All returns the packs, built-in ones first.
func (c *Catalog) All() []Manifest {
	all := make([]Manifest, len(c.names))
	for i, n := range c.names {
		all[i] = c.packs[n]
	}
	return all
}

// Enabled returns the packs a config enables, in the config's order,
// leaving out names the catalog does not know.
func (c *Catalog) Enabled(cfg sandbox.Config) []Manifest {
	var ms []Manifest
	for _, n := range cfg.Packs {
		if m, ok := c.packs[n]; ok {
			ms = append(ms, m)
		}
	}
	return ms
}

// Check returns what is wrong with the packs a config enables, which
// sandbox.Config.Validate cannot know: packs that do not exist, and
// packs that collide with each other.
func (c *Catalog) Check(cfg sandbox.Config) []sandbox.FieldError {
	var errs []sandbox.FieldError
	add := func(format string, args ...any) {
		errs = append(errs, sandbox.FieldError{Field: sandbox.FieldPacks, Message: fmt.Sprintf(format, args...)})
	}
	ports := map[int]string{}
	bridges := map[string]string{}
	for _, name := range cfg.Packs {
		m, ok := c.packs[name]
		if !ok {
			add("there is no Capability Pack named %q", name)
			continue
		}
		if d := m.DaemonProcess; d != nil {
			if other, taken := ports[d.ReadinessPort]; taken {
				add("%s and %s both use port %d", other, m.Title, d.ReadinessPort)
			}
			ports[d.ReadinessPort] = m.Title
		}
		if b := m.MCPBridge; b != nil {
			if other, taken := bridges[b.Name]; taken {
				add("%s and %s both register an MCP server named %q", other, m.Title, b.Name)
			}
			bridges[b.Name] = m.Title
		}
	}
	return errs
}
