package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo"

	"zeolite/internal/configstore"
)

// The data directory has one name, whoever asks: the config store, and
// the UI toolkit for what it keeps there itself.
func TestDataDirectoryIsOneDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZEOLITE_DATA_DIR", "")
	dir, err := dataDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "Library", "Application Support", "zeolite"); dir != want {
		t.Errorf("the data directory is %s, want %s", dir, want)
	}
	if toolkit, err := mygo.App.Path(mygo.PathUserData); err != nil || toolkit != dir {
		t.Errorf("the UI toolkit keeps its data in %s (%v), want %s", toolkit, err, dir)
	}
	if mygo.App.Name() != configstore.DirName {
		t.Errorf("the app is named %q, want %q", mygo.App.Name(), configstore.DirName)
	}
}

func TestDataDirectoryIsTheUsersAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZEOLITE_DATA_DIR", dir)
	got, err := dataDir()
	if err != nil || got != dir {
		t.Fatalf("dataDir() = %s, %v", got, err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("an existing data directory was left %o, want 700", mode)
	}
}
