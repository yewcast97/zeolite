//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"zeolite/internal/orchestrator"
	"zeolite/internal/pack"
	"zeolite/internal/sandbox"
)

// A sandbox's Home is its own folder in the VM, which its workstation
// mounts at /workspace: what its user installs there, and logs in to,
// is there at the next start. It is deleted when the sandbox is
// destroyed, by its ID, and by nothing else.

// removeHome deletes a sandbox's Home as the app does.
func removeHome(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return orch.RemoveHome(ctx, id)
}

// tmpVolume is the name Docker gave the volume a sandbox's workstation
// mounts at /tmp: one it made with the workstation, which has no name
// of the app's.
func tmpVolume(t *testing.T, id string) string {
	t.Helper()
	out, code := docker("inspect", "-f", `{{range .Mounts}}{{if eq .Destination "/tmp"}}{{.Type}} {{.Name}}{{end}}{{end}}`, orchestrator.WorkstationContainer(id))
	kind, name, _ := strings.Cut(trimmed(out), " ")
	if code != 0 || kind != "volume" || name == "" || strings.HasPrefix(name, "zeolite-") {
		t.Fatalf("the workstation's /tmp is %q (%d), want a volume with a name of Docker's", trimmed(out), code)
	}
	return name
}

func TestHomeIsTheAgentsAndOnDisk(t *testing.T) {
	cfg := lockedConfig("home-own", "Home")
	start(t, cfg, nil, nil)
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)

	// The agent's, with the mode of a home directory: Docker gave the
	// empty volume what the image's /workspace has.
	if out, _ := inSandbox(cfg.ID, "stat -c '%u:%g %a' /workspace"); trimmed(out) != "1000:1000 755" {
		t.Errorf("/workspace is %q, want 1000:1000 755", trimmed(out))
	}
	// A volume on the VM's disk, and /tmp another: neither is memory,
	// which /dev/shm is.
	if out, _ := inSandbox(cfg.ID, "stat -f -c %T /workspace /tmp /dev/shm | tr '\\n' ' '"); strings.Count(out, "tmpfs") != 1 || !strings.HasSuffix(trimmed(out), "tmpfs") {
		t.Errorf("the filesystems of /workspace, /tmp and /dev/shm are %q, want a disk's twice and tmpfs", trimmed(out))
	}
	// /tmp is anyone's to write, as /tmp is, and holds what the
	// supervisor keeps there and nothing of the image.
	if out, _ := inSandbox(cfg.ID, "stat -c '%u:%g %a' /tmp; ls -A /tmp | tr '\\n' ' '"); trimmed(out) != "0:0 1777\nzeolite" {
		t.Errorf("/tmp is %q, want 0:0 1777 with the supervisor's folder in it", trimmed(out))
	}
	if out, _ := inSandbox(cfg.ID, "printf '#!/bin/sh\\necho ran-from-tmp\\n' > /tmp/mine && chmod +x /tmp/mine && /tmp/mine"); trimmed(out) != "ran-from-tmp" {
		t.Errorf("a program in /tmp printed %q", trimmed(out))
	}
	// Its volume is the sandbox's by its label, and has no name of the
	// app's: Docker made it with the workstation.
	if out, code := docker("volume", "inspect", "-f", `{{index .Labels "zeolite.sandbox"}}`, tmpVolume(t, cfg.ID)); code != 0 || trimmed(out) != cfg.ID {
		t.Errorf("the label of /tmp's volume is %q (%d), want the sandbox's ID", trimmed(out), code)
	}
	// Nothing of the image is in a new Home but what the supervisor
	// writes at every start: the agents' MCP registries. No file tells
	// agents how to work there: they are told by MCP.
	if out, _ := inSandbox(cfg.ID, "ls -A /workspace | sort | tr '\\n' ' '"); trimmed(out) != newHome {
		t.Errorf("a new Home holds %q", trimmed(out))
	}
	// Programs run from it.
	if out, _ := inSandbox(cfg.ID, "mkdir -p ~/.local/bin && printf '#!/bin/sh\\necho ran-from-home\\n' > ~/.local/bin/mine && chmod +x ~/.local/bin/mine && ~/.local/bin/mine"); trimmed(out) != "ran-from-home" {
		t.Errorf("a program in the Home printed %q", trimmed(out))
	}
	// The volume is the sandbox's own, by its name and its label, and
	// only its workstation mounts it.
	if out, code := docker("volume", "inspect", "-f", `{{index .Labels "zeolite.sandbox"}}`, orchestrator.HomeVolume(cfg.ID)); code != 0 || trimmed(out) != cfg.ID {
		t.Errorf("the Home's label is %q (%d), want the sandbox's ID", trimmed(out), code)
	}
	if out, _ := docker("ps", "-a", "--filter", "volume="+orchestrator.HomeVolume(cfg.ID), "--format", "{{.Names}}"); trimmed(out) != orchestrator.WorkstationContainer(cfg.ID) {
		t.Errorf("the Home is mounted by %q", trimmed(out))
	}
}

// newHome is what a Home holds after its sandbox's first start: the
// MCP registries of the agents the app knows, each where its agent
// looks for it.
const newHome = ".claude.json .codex .gemini .grok .kimi-code .omp"

func TestHomeOutlivesStop(t *testing.T) {
	cfg := lockedConfig("home-stop", "Home over Stop")
	start(t, cfg, nil, nil)
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)
	script := "mkdir -p ~/.local/bin ~/.config/agent && printf '#!/bin/sh\\necho kept-agent\\n' > ~/.local/bin/kept-agent && chmod +x ~/.local/bin/kept-agent && echo logged-in > ~/.config/agent/login"
	if out, code := inSandbox(cfg.ID, script); code != 0 {
		t.Fatalf("writing into the Home: %s", out)
	}
	if where := whereInstalled(cfg.ID, "kept-agent"); where != "/workspace/.local/bin/kept-agent" {
		t.Fatalf("before the stop a login shell finds the executable at %q", where)
	}

	// While it runs, its Home is not deleted: not by the app, and not by
	// Docker, which refuses a volume a container mounts.
	if err := removeHome(cfg.ID); err == nil || !strings.Contains(err.Error(), "running") {
		t.Errorf("deleting the Home of a running sandbox: %v", err)
	}
	if out, code := docker("volume", "rm", orchestrator.HomeVolume(cfg.ID)); code == 0 {
		t.Errorf("Docker removed a Home its workstation mounts: %s", out)
	}

	// What is in /tmp is on the VM's disk until the stop, and no longer.
	if out, code := inSandbox(cfg.ID, "echo left-in-tmp > /tmp/left"); code != 0 {
		t.Fatalf("writing into /tmp: %s", out)
	}
	tmp := tmpVolume(t, cfg.ID)

	orch.Stop(context.Background(), cfg.ID)
	if _, code := docker("inspect", orchestrator.WorkstationContainer(cfg.ID)); code == 0 {
		t.Fatal("the workstation survived the stop")
	}
	if _, code := docker("volume", "inspect", orchestrator.HomeVolume(cfg.ID)); code != 0 {
		t.Fatal("the Home did not survive the stop")
	}
	if out, code := docker("volume", "inspect", "-f", "{{.Name}}", tmp); code == 0 {
		t.Errorf("the volume of /tmp survived the stop: %s", trimmed(out))
	}

	// Started again, it holds what was installed and logged in to, the
	// login shell finds the executable by its name.
	startAgain(t, cfg, nil, orchestrator.StartOptions{})
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)
	if out, _ := inLoginShell(cfg.ID, "kept-agent; cat ~/.config/agent/login"); trimmed(out) != "kept-agent\nlogged-in" {
		t.Errorf("after a stop and a start the Home gives %q", trimmed(out))
	}
	if where := whereInstalled(cfg.ID, "kept-agent"); where != "/workspace/.local/bin/kept-agent" {
		t.Errorf("after a stop and a start a login shell finds the executable at %q", where)
	}
	// Its /tmp is another, and empty but for the supervisor's folder.
	if again := tmpVolume(t, cfg.ID); again == tmp {
		t.Error("the sandbox was started again on the /tmp it had")
	}
	if out, _ := inSandbox(cfg.ID, "ls -A /tmp | tr '\\n' ' '"); trimmed(out) != "zeolite" {
		t.Errorf("after a stop and a start /tmp holds %q", trimmed(out))
	}

	// Another sandbox has another Home, with none of it.
	other := lockedConfig("home-other", "Another Home")
	start(t, other, nil, nil)
	waitState(t, other.ID, sandbox.StateGateLocked, 20*time.Second)
	if out, _ := inSandbox(other.ID, "ls ~/.local/bin ~/.config 2>&1; ls -A ~ | wc -l"); strings.Contains(out, "kept-agent") || strings.Contains(out, "agent") {
		t.Errorf("another sandbox sees %q of this one's Home", trimmed(out))
	}

	// Destroyed: once it is stopped its Home is deleted, and a second
	// deletion finds nothing to do.
	orch.Stop(context.Background(), cfg.ID)
	if err := removeHome(cfg.ID); err != nil {
		t.Fatalf("deleting the Home of a stopped sandbox: %v", err)
	}
	if _, code := docker("volume", "inspect", orchestrator.HomeVolume(cfg.ID)); code == 0 {
		t.Error("the Home is still there after it was deleted")
	}
	if err := removeHome(cfg.ID); err != nil {
		t.Errorf("deleting a Home that is not there: %v", err)
	}
	// The other's is untouched.
	if _, code := docker("volume", "inspect", orchestrator.HomeVolume(other.ID)); code != 0 {
		t.Error("deleting one sandbox's Home deleted another's")
	}
	// And a start after it is a start on an empty Home.
	startAgain(t, cfg, nil, orchestrator.StartOptions{})
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)
	if out, _ := inSandbox(cfg.ID, "ls ~/.local 2>&1"); !strings.Contains(out, "No such file") {
		t.Errorf("after its Home was deleted the sandbox still holds %q", trimmed(out))
	}
}

// What a Capability Pack writes under /workspace when its image is
// built is not in a sandbox's Home: Docker would copy it into an empty
// Home once, and never again when the pack changed.
func TestAPacksFilesAreNotCopiedIntoAHome(t *testing.T) {
	m, err := pack.Parse([]byte("name: test-writes-home\ntitle: Writes Home\nruntimeLayer:\n  run: ['mkdir -p /workspace/from-pack && echo stale > /workspace/from-pack/file']\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := lockedConfig("home-pack", "Home with a pack")
	cfg.Packs = []string{m.Name}
	start(t, cfg, []pack.Manifest{m}, nil)
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)
	if out, _ := inSandbox(cfg.ID, "ls -A /workspace | sort | tr '\\n' ' '; stat -c '%u:%g %a' /workspace"); trimmed(out) != newHome+" 1000:1000 755" {
		t.Errorf("the Home of a sandbox whose pack wrote under /workspace holds %q", trimmed(out))
	}
}

// A sandbox that lost its Shared Folder has no /workspace/shared: the
// mountpoint Docker left in its Home is removed at the next start, and
// a folder its user made there with something in it is not.
func TestALostSharedFolderLeavesNoFolderInTheHome(t *testing.T) {
	_, path := aSharedFolder(t, "test-home-shared")
	cfg := lockedConfig("home-shared", "Home and Shared Folder")
	cfg.SharedFolder = "test-home-shared"
	startWith(t, cfg, nil, orchestrator.StartOptions{SharedFolder: path})
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)
	if out, code := inSandbox(cfg.ID, "grep -c ' /workspace/shared ' /proc/self/mountinfo"); code != 0 || trimmed(out) != "1" {
		t.Fatalf("the sandbox's Shared Folder is mounted %q times", trimmed(out))
	}
	orch.Stop(context.Background(), cfg.ID)

	cfg.SharedFolder = ""
	startAgain(t, cfg, nil, orchestrator.StartOptions{})
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)
	if out, _ := inSandbox(cfg.ID, "ls -d /workspace/shared 2>&1"); !strings.Contains(out, "No such file") {
		t.Errorf("a sandbox with no Shared Folder has %q", trimmed(out))
	}
	// A folder of that name its user made, with something in it, stays.
	if out, code := inSandbox(cfg.ID, "mkdir /workspace/shared && echo mine > /workspace/shared/notes"); code != 0 {
		t.Fatalf("making a folder in the Home: %s", out)
	}
	orch.Stop(context.Background(), cfg.ID)
	startAgain(t, cfg, nil, orchestrator.StartOptions{})
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)
	if out, _ := inSandbox(cfg.ID, "cat /workspace/shared/notes"); trimmed(out) != "mine" {
		t.Errorf("the user's own folder in the Home holds %q after a start", trimmed(out))
	}
}
