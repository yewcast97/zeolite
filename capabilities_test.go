package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// No container of the app's is privileged, and the one capability any
// is given is NET_ADMIN, which a gatekeeper and the Backstop hold over
// their own rulesets. A workstation has no capability.
func TestNoContainerIsGivenMoreThanNetAdmin(t *testing.T) {
	files, given := 0, 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != "." && (strings.HasPrefix(name, ".") || name == "build" || name == "integration") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		path = filepath.ToSlash(path)
		if strings.Contains(string(source), "Privileged") {
			t.Errorf("%s names a privileged container: the app makes none", path)
		}
		for _, line := range strings.Split(string(source), "\n") {
			if !strings.Contains(line, "CapAdd") {
				continue
			}
			given++
			if strings.TrimSpace(line) != `CapAdd:         []string{"NET_ADMIN"},` {
				t.Errorf("%s gives a container %s", path, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 40 {
		t.Fatalf("read %d files: the test would prove little", files)
	}
	if given != 2 {
		t.Errorf("%d containers are given a capability, want two: a gatekeeper and the Backstop", given)
	}
}
