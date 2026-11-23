//go:build integration

package integration

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"zeolite/internal/orchestrator"
	"zeolite/internal/sandbox"
	"zeolite/internal/sharedfolder"
)

// A Shared Folder is a folder of this Mac that the workstations of the
// sandboxes given it mount. These tests use real ones, in the root the
// VM mounts, under names of their own, and skip where the VM does not
// mount the root: the Colima profile must have been started with
// --mount <root>:w.
//
// The app reads nothing in a Shared Folder, and neither do these tests:
// what an agent wrote is read through a sandbox, and on the Mac's side
// only a file's name and size are looked at.

// sharedFolderRoot prepares the Shared Folders' root as the app does at
// launch, and says how the VM's mount of it is known.
func sharedFolderRoot(t *testing.T) (*sharedfolder.Folders, orchestrator.SharedFolderRoot) {
	t.Helper()
	folders := sharedfolder.New(sharedfolder.DefaultRoot)
	marker, err := folders.Prepare()
	if err != nil {
		t.Fatalf("preparing the Shared Folders' root: %v", err)
	}
	return folders, orchestrator.SharedFolderRoot{Path: folders.Root(), MarkerName: sharedfolder.MarkerName, MarkerToken: marker}
}

// aSharedFolder makes a Shared Folder of a test's own, empty, and
// returns where it is on the Mac. Its name begins with "test-", which no
// folder of the user's should: what an earlier run left under it goes.
// It skips where the VM does not mount the root.
func aSharedFolder(t *testing.T, name string) (*sharedfolder.Folders, string) {
	t.Helper()
	if !strings.HasPrefix(name, "test-") {
		t.Fatalf("%q is not a name these tests may delete", name)
	}
	folders, root := sharedFolderRoot(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	_, mounted, err := orch.ExposedMacFolders(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if !mounted {
		t.Skipf("the VM does not mount %s: start the Colima profile with --mount %s:w", root.Path, root.Path)
	}
	if err := folders.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := folders.Create(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// After the sandboxes were stopped: cleanups run last in, first out.
		if err := folders.Remove(name); err != nil {
			t.Errorf("removing the test's Shared Folder: %v", err)
		}
	})
	return folders, folders.Path(name)
}

// startWithSharedFolder starts a sandbox that is given a Shared Folder,
// and whose gate stays locked: the folder needs no network.
func startWithSharedFolder(t *testing.T, id, name, sharedFolder, path string) sandbox.Config {
	t.Helper()
	cfg := lockedConfig(id, name)
	cfg.SharedFolder = sharedFolder
	startWith(t, cfg, nil, orchestrator.StartOptions{SharedFolder: path})
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)
	return cfg
}

// The VM mounts the Shared Folders' root or nothing, and a mount is the
// root only by the marker the app wrote in it.
func TestVMMountsNothingButTheSharedFolderRoot(t *testing.T) {
	_, root := sharedFolderRoot(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	exposed, mounted, err := orch.ExposedMacFolders(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(exposed) > 0 {
		t.Errorf("the VM mounts more of this Mac than the Shared Folders' root: %v", exposed)
	}
	if !mounted {
		t.Skipf("the VM mounts nothing of this Mac. To test the Shared Folder, start the Colima profile with --mount %s:w", root.Path)
	}
	root.MarkerToken = "not-the-marker"
	exposed, mounted, err = orch.ExposedMacFolders(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if mounted || len(exposed) != 1 || exposed[0] != root.Path {
		t.Errorf("a mount without the app's marker was taken for the Shared Folders' root (mounted %v, exposed %v)", mounted, exposed)
	}
}

func TestSharedFolder(t *testing.T) {
	const name = "test-shared"
	folders, folder := aSharedFolder(t, name)
	cfg := startWithSharedFolder(t, "shared-folder", "Shared Folder", name, folder)
	shared := sandbox.SharedFolderPath

	in := func(id, what, script, want string) {
		t.Helper()
		out, code := inSandbox(id, "cd "+shared+" && "+script)
		if code != 0 || trimmed(out) != want {
			t.Errorf("%s: the workstation answered %q (exit %d), want %q", what, trimmed(out), code, want)
		}
	}
	run := func(what, script, want string) {
		t.Helper()
		in(cfg.ID, what, script, want)
	}

	// The folder is the agent's to use as any folder of its own.
	run("the agent's user", "id -u", "1000")
	run("creating a file", "echo one > note.txt && cat note.txt", "one")
	run("rewriting it", "echo two > note.txt && cat note.txt", "two")
	run("appending to it", "echo three >> note.txt && wc -l < note.txt", "2")
	run("running a program", `printf '#!/bin/sh\necho ran\n' > tool.sh && chmod +x tool.sh && ./tool.sh`, "ran")
	run("folders", "mkdir -p deep/er && echo x > deep/er/file && mv deep/er/file deep/moved && rm -r deep && ls -d deep 2>/dev/null; echo gone", "gone")
	run("a read-only tree, as a module cache is", "mkdir -p ro/sub && echo x > ro/sub/f && chmod -R a-w ro && chmod -R u+w ro && rm -r ro && echo removed", "removed")
	run("a link", "ln -s note.txt link && cat link | head -1 && rm link", "two")
	// It sees its own folder and nothing else of the root: not the
	// marker, and no other Shared Folder.
	run("the root's marker", "ls -a | grep -c zeolite-shared-folder-root; true", "0")

	// What the agent wrote is on the Mac's disk, by name and size.
	if info, err := os.Lstat(filepath.Join(folder, "note.txt")); err != nil || info.Size() != int64(len("two\nthree\n")) {
		t.Errorf("the agent's file on the Mac is %v (%v)", info, err)
	}

	// What the Mac's side puts in the folder is there for the agent, and
	// what it changes reads new: the workstation does not go on seeing a
	// file as it first read it.
	fromMac := filepath.Join(folder, "from-the-mac.txt")
	run("a file before the Mac made it", "cat from-the-mac.txt 2>/dev/null; echo none", "none")
	if err := os.WriteFile(fromMac, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("a file the Mac made", "cat from-the-mac.txt", "first")
	if err := os.WriteFile(fromMac, []byte("second, and longer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("the file after the Mac rewrote it", "cat from-the-mac.txt", "second, and longer")
	if err := os.WriteFile(fromMac, []byte("3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("the file after the Mac made it shorter", "cat from-the-mac.txt", "3")
	run("the agent rewriting the Mac's file", "echo mine > from-the-mac.txt && cat from-the-mac.txt", "mine")
	if err := os.Remove(fromMac); err != nil {
		t.Fatal(err)
	}
	run("the file after the Mac deleted it", "cat from-the-mac.txt 2>/dev/null; echo gone", "gone")

	// A second sandbox given the folder works in it with the first: each
	// reads what the other wrote, and changes it.
	other := startWithSharedFolder(t, "shared-other", "Shared Folder, the other", name, folder)
	in(other.ID, "the other reading the first's file", "cat note.txt", "two\nthree")
	in(other.ID, "the other writing", "echo from-the-other > theirs.txt && echo four >> note.txt && echo ok", "ok")
	run("the first reading the other's file", "cat theirs.txt", "from-the-other")
	run("the first reading what the other appended", "tail -1 note.txt", "four")
	run("the first rewriting the other's file", "echo rewritten > theirs.txt && echo ok", "ok")
	in(other.ID, "the other reading it back", "cat theirs.txt", "rewritten")
	in(other.ID, "the other deleting the first's lines", "printf 'two\\nthree\\n' > note.txt && rm theirs.txt && echo ok", "ok")
	run("the first after the other's deletion", "cat theirs.txt 2>/dev/null; wc -l < note.txt", "2")

	// The folder is mounted while either runs, which is when the app
	// refuses to delete it.
	if !orch.SharedFolderMounted(folder) {
		t.Error("a folder two running sandboxes mount is not said to be mounted")
	}
	orch.Stop(context.Background(), other.ID)
	if !orch.SharedFolderMounted(folder) {
		t.Error("a folder is not said to be mounted while one of its sandboxes still runs")
	}

	// It is kept when the last sandbox stops, and there again at a start.
	orch.Stop(context.Background(), cfg.ID)
	if orch.SharedFolderMounted(folder) {
		t.Error("a folder is said to be mounted after its sandboxes stopped")
	}
	if info, err := os.Lstat(filepath.Join(folder, "note.txt")); err != nil || info.Size() != int64(len("two\nthree\n")) {
		t.Fatalf("after Stop the agent's file on the Mac is %v (%v)", info, err)
	}
	startWithSharedFolder(t, cfg.ID, "Shared Folder", name, folder)
	run("the file after a stop and a start", "cat note.txt", "two\nthree")
	run("the program after a stop and a start", "./tool.sh", "ran")

	// And deleted with everything in it, links and all, when the app
	// removes it.
	run("a link out of the folder", "ln -s /etc/hostname out && ln -s ../.. up && echo ok", "ok")
	orch.Stop(context.Background(), cfg.ID)
	if err := folders.Remove(name); err != nil {
		t.Fatalf("removing the Shared Folder: %v", err)
	}
	if _, err := os.Lstat(folder); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the Shared Folder is still on the Mac (%v)", err)
	}
	if names, err := folders.Names(); err != nil || slices.Contains(names, name) {
		t.Errorf("after its removal the Shared Folders are %q (%v)", names, err)
	}
}

// macUserFacts are what tells this Mac's user from any other: the login
// name and the home folder. They are looked for where a folder of the
// Mac is mounted, whose paths could carry them.
func macUserFacts(t *testing.T) []hostFact {
	t.Helper()
	var facts []hostFact
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for what, value := range map[string]string{
		"the Mac user's login name":  u.Username,
		"the Mac user's full name":   u.Name,
		"the Mac user's home folder": u.HomeDir,
	} {
		// A value this short would be found in anything.
		if len(value) >= 4 {
			facts = append(facts, hostFact{what: what, value: value})
		}
	}
	return facts
}

// A sandbox learns nothing of this Mac from its Shared Folder: not from
// how it is mounted, nor from what a file in it carries besides its
// content.
func TestSharedFolderHoldsNoHostFact(t *testing.T) {
	_, folder := aSharedFolder(t, "test-facts")
	cfg := startWithSharedFolder(t, "shared-facts", "Shared Folder facts", "test-facts", folder)
	// A file as the Mac's side leaves one.
	if err := os.WriteFile(filepath.Join(folder, "from-the-mac.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	told := map[string]string{}
	for what, script := range map[string]string{
		"the mount table":         "cat /proc/self/mountinfo /proc/mounts /proc/1/mountinfo",
		"the folder's attributes": "stat /workspace/shared /workspace/shared/from-the-mac.txt; ls -lan /workspace/shared",
		"the disk":                "df -P /workspace/shared; stat -f /workspace/shared",
		"the extended attributes": `python3 -c '
import os
for p in ("/workspace/shared", "/workspace/shared/from-the-mac.txt"):
    names = os.listxattr(p)
    print(p, names)
    for n in names:
        print(n, os.getxattr(p, n))
'`,
	} {
		out, code := inSandbox(cfg.ID, script)
		if code != 0 || trimmed(out) == "" {
			t.Fatalf("reading %s in the workstation: %q (exit %d)", what, out, code)
		}
		told[what] = out
	}

	facts := append(macFacts(t), macUserFacts(t)...)
	kinds := map[string]int{}
	for _, f := range facts {
		kinds[f.what]++
	}
	if kinds["the Mac user's login name"] == 0 || kinds["the Mac user's home folder"] == 0 {
		t.Fatalf("this Mac's user could not be read: the test would prove little (%v)", kinds)
	}
	t.Logf("looked for %d host facts: %v", len(facts), kinds)
	for what, text := range told {
		for _, f := range held(text, facts) {
			t.Errorf("%s holds %s:\n%s", what, f.what, text)
		}
	}
	// The search finds a fact where there is one.
	for _, f := range facts {
		if found := held("virtiofs "+f.value+" rw\n", facts); len(found) == 0 {
			t.Errorf("the search missed %s in a text that holds it", f.what)
		}
	}

	// What the mount does show, which docs/shared-folders.md says: by what it is
	// mounted, and whose the files are.
	for _, line := range strings.Split(told["the mount table"], "\n") {
		if strings.Contains(line, sandbox.SharedFolderPath) {
			t.Logf("the workstation's mount table: %s", line)
		}
	}
	t.Logf("the folder's attributes in the workstation:\n%s", told["the folder's attributes"])
	t.Logf("the extended attributes in the workstation:\n%s", told["the extended attributes"])
}
