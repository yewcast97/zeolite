// Package configstore keeps what the user configured in a file of the
// app's data directory: the sandboxes' configs.
package configstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"zeolite/internal/sandbox"
)

// DirName names the app's data directory. It is the one place that
// does: the main package gives the UI toolkit the same name, so that
// what the toolkit keeps for the app lands in the same directory.
const DirName = "zeolite"

// Dir is the app's data directory, under the user's Application Support.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, DirName), nil
}

// Secure creates the data directory, for the user alone, and takes it
// from everyone else when it already exists: what it says of the user's
// sandboxes and networks is nobody else's to read.
func Secure(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// Store is the app's file in its data directory: the sandboxes'
// configs. Only the user can read it. It holds no secret: the app takes
// no API key and keeps none.
type Store struct {
	dir string
}

// sandboxesFile is the file of a Store.
const sandboxesFile = "sandboxes.json"

// Open returns the store in dir.
func Open(dir string) *Store {
	return &Store{dir: dir}
}

// Load returns the saved configs, none when nothing was saved yet.
func (s *Store) Load() ([]sandbox.Config, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, sandboxesFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var configs []sandbox.Config
	if err := json.Unmarshal(data, &configs); err != nil {
		return nil, err
	}
	return configs, nil
}

// Save replaces the saved configs. The file is written beside its place
// and moved there, so that it is never seen half written.
func (s *Store) Save(configs []sandbox.Config) error {
	if configs == nil {
		configs = []sandbox.Config{}
	}
	data, err := json.MarshalIndent(configs, "", "  ")
	if err != nil {
		return err
	}
	if err := Secure(s.dir); err != nil {
		return err
	}
	path := filepath.Join(s.dir, sandboxesFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
