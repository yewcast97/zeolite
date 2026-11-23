package main

import (
	"os"
	"strings"
	"testing"

	"zeolite/internal/pack"
	"zeolite/internal/sandbox"
	"zeolite/internal/sharedfolder"
)

// The VM's size is said in two places, the README and the banner that
// tells how to start a runtime that does not answer: they give one
// command, and it asks for 8 GiB of memory.
func TestTheBannerStartsTheVMAsTheReadmeDoes(t *testing.T) {
	catalog, err := pack.NewCatalog()
	if err != nil {
		t.Fatal(err)
	}
	b := newBackend(t.TempDir(), sharedfolder.DefaultRoot, t.TempDir(), catalog, func(sandbox.Update) {})
	command := b.howToStartColima()
	if !strings.Contains(command, " --memory 8 ") {
		t.Errorf("the banner's command is %q: the VM's default memory is 8 GiB", command)
	}
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	// The README breaks the command over two lines.
	said := strings.ReplaceAll(string(readme), " \\\n ", "")
	if !strings.Contains(said, "\n"+command+"\n") {
		t.Errorf("the README does not give the banner's command, %q", command)
	}
}
