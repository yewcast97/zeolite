package main

import (
	"os/exec"
	"strings"
	"testing"
)

// untrackedFiles stay on this Mac: git must ignore them and must never
// track them. They hold a key or what the fixture of the tests' virtual
// Interface reads, or are that fixture, which says how its owner made
// that Interface: no business of the repository's.
var untrackedFiles = []string{
	".env",
	"instance_test",
	"integration/virtualinterface_fixture_test.go",
}

func TestSecretFilesStayUntracked(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if out, err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Skip("not a git work tree")
	}
	for _, name := range untrackedFiles {
		// --error-unmatch fails when the path is not tracked.
		if err := exec.Command("git", "ls-files", "--error-unmatch", "--", name).Run(); err == nil {
			t.Errorf("git tracks %s: remove it with `git rm --cached %s` and rewrite any commit that holds it", name, name)
		}
		// --no-index asks the ignore rules alone, whatever is tracked.
		if err := exec.Command("git", "check-ignore", "--quiet", "--no-index", "--", name).Run(); err != nil {
			t.Errorf("git does not ignore %s: add /%s to .gitignore", name, name)
		}
	}
}

// The guard must be able to fail: a path nothing ignores is reported.
func TestSecretFilesGuardSeesAnUnignoredPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if out, err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Skip("not a git work tree")
	}
	if err := exec.Command("git", "check-ignore", "--quiet", "--no-index", "--", "main.go").Run(); err == nil {
		t.Fatal("git says main.go is ignored: the check cannot tell an ignored path from another")
	}
	if err := exec.Command("git", "ls-files", "--error-unmatch", "--", "main.go").Run(); err != nil {
		t.Fatal("git says main.go is untracked: the check cannot tell a tracked path from another")
	}
}
