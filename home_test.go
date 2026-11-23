package main

import (
	"strings"
	"testing"

	"zeolite/internal/pack"
	"zeolite/internal/sandbox"
)

// Where the runtime does not answer, a sandbox's Home is not known to
// be gone: deleting it is an error, and the sandbox is then kept.
func TestAHomeIsNotTakenForDeletedWithoutTheRuntime(t *testing.T) {
	catalog, err := pack.NewCatalog()
	if err != nil {
		t.Fatal(err)
	}
	b := newBackend(t.TempDir(), t.TempDir(), t.TempDir(), catalog, func(sandbox.Update) {})
	if err := b.RemoveHome("s0a1b2c3d4e"); err == nil || !strings.Contains(err.Error(), "runtime is not available") {
		t.Errorf("deleting a Home with no runtime: %v", err)
	}
}
