//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"zeolite/internal/orchestrator"
	"zeolite/internal/sandbox"
)

// waitLockedFor waits for a sandbox's gate to be locked for a reason.
func waitLockedFor(t *testing.T, id string, reason sandbox.LockReason, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, fmt.Sprintf("sandbox %s to be locked for %q", id, reason), func() bool {
		s := updates.statusOf(id)
		return s.State == sandbox.StateGateLocked && s.LockReason == reason
	})
}

func TestSleepLocksEveryGate(t *testing.T) {
	cfg := lockedConfig("sleep", "Sleep")
	start(t, cfg, nil, nil)
	waitLockedFor(t, cfg.ID, sandbox.LockProbeUnanswered, 20*time.Second)

	// The Mac says it is going to sleep: the gate is told so without
	// waiting for the next heartbeat's turn, and says why it is locked.
	orch.SetSuspended(true)
	defer orch.SetSuspended(false)
	waitLockedFor(t, cfg.ID, sandbox.LockHostCondition, 4*time.Second)
	if got := attempt(t, cfg.ID, "1.1.1.1", 80); got == answered {
		t.Error("a public address answered while the Mac was going to sleep")
	}
	orch.SetSuspended(false)
	waitLockedFor(t, cfg.ID, sandbox.LockProbeUnanswered, 10*time.Second)
}

func TestWorkstationIsolation(t *testing.T) {
	cfg := lockedConfig("isolation", "Isolation")
	cfg.Clock = sandbox.ClockPolicy{Timezone: "Asia/Tokyo"}
	start(t, cfg, nil, nil)
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)

	check := func(what, script, want string) {
		t.Helper()
		out, _ := inSandbox(cfg.ID, script)
		if !strings.Contains(out, want) {
			t.Errorf("%s: %q, want it to contain %q", what, trimmed(out), want)
		}
	}
	check("the user", "id -u", "1000")
	check("capabilities", "grep CapEff /proc/self/status", "0000000000000000")
	check("a write to the root filesystem", "touch /etc/x 2>&1", "Read-only file system")
	// No program gains privileges, whatever a file's mode says: the Home
	// is a volume, which a set-user-ID bit could be written on.
	check("new privileges", "grep NoNewPrivs /proc/self/status", "1")
	check("a program run from /workspace", "printf '#!/bin/sh\\necho ran\\n' > /workspace/x && chmod +x /workspace/x && /workspace/x", "ran")
	check("/dev/shm", "df -m /dev/shm | tail -1", "2048")
	check("the home directory", "echo $HOME", "/workspace")
	check("Node.js", "node --version", "v22.")
	check("Python", "python3 --version", "Python 3.")
	// Nothing installs a package of Debian's while the sandbox runs, its
	// user not being root and its root read-only. What its work takes for
	// granted is in the image: a C and a C++ program are built and run, a
	// file is compiled against Python's headers, the ssh client makes a
	// key, and an archive of zip's and one of xz's are unpacked.
	check("a C program", `cd /tmp && printf '#include <stdio.h>\nint main(void) { puts("built in C"); return 0; }\n' > a.c && gcc a.c -o a && ./a`, "built in C")
	check("a C++ program", `cd /tmp && printf '#include <iostream>\nint main() { std::cout << "built in C++" << std::endl; }\n' > b.cc && g++ b.cc -o b && ./b`, "built in C++")
	check("make", `cd /tmp && printf 'all:\n\t@echo made\n' > Makefile && make`, "made")
	check("Python's headers", `cd /tmp && printf '#include <Python.h>\nint version(void) { return PY_MAJOR_VERSION; }\n' > p.c && gcc $(python3-config --includes) -c p.c -o p.o && echo compiled`, "compiled")
	check("the ssh client", "cd /tmp && ssh -V 2>&1 && ssh-keygen -q -t ed25519 -N '' -f key && cat key.pub", "ssh-ed25519")
	check("an archive of zip's", `cd /tmp && python3 -c 'import zipfile; zipfile.ZipFile("a.zip", "w").writestr("in.txt", "unzipped")' && unzip -p a.zip in.txt`, "unzipped")
	check("an archive of xz's", "cd /tmp && echo unpacked > x.txt && tar -cJf x.tar.xz x.txt && rm x.txt && tar -xJf x.tar.xz && cat x.txt", "unpacked")
	check("the MCP registry OpenCode is sent to", `cat "$OPENCODE_CONFIG"`, `"mcp"`)

	// The clock is this Mac's, in Tokyo, for every program alike. A date
	// read as the day turns in Tokyo is read again.
	want := time.Now().In(time.FixedZone("JST", 9*3600))
	if next := want.Add(time.Minute); next.Day() != want.Day() {
		time.Sleep(time.Until(next))
		want = next
	}
	check("the date", "date '+%Y-%m-%d %Z'", want.Format("2006-01-02")+" JST")
	check("Python's date", `python3 -c 'import datetime; print(datetime.date.today())'`, want.Format("2006-01-02"))
	check("Node's date", `node -e 'const d = new Date(); console.log(d.getFullYear() + "-" + String(d.getMonth()+1).padStart(2, "0") + "-" + String(d.getDate()).padStart(2, "0"))'`, want.Format("2006-01-02"))
	waitFor(t, 8*time.Second, "the workstation to report its clock", func() bool {
		_, ok := updates.workstationOf(cfg.ID)
		return ok
	})
	if w, _ := updates.workstationOf(cfg.ID); !strings.HasPrefix(w.Clock, want.Format("2006-01-02")) || !strings.HasSuffix(w.Clock, "+09:00") {
		t.Errorf("the reported clock is %q, want %s in +09:00", w.Clock, want.Format("2006-01-02"))
	}
}

func TestGatekeeperLossEndsTheSandbox(t *testing.T) {
	cfg := lockedConfig("gk-loss", "Gatekeeper loss")
	start(t, cfg, nil, nil)
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)

	// Docker must not bring a dead gatekeeper back, and the workstation
	// must not be left for anything to restart into a new namespace.
	if out, _ := docker("inspect", "-f", "{{.HostConfig.RestartPolicy.Name}}", orchestrator.GatekeeperContainer(cfg.ID)); trimmed(out) != "no" {
		t.Errorf("the gatekeeper's restart policy is %q", trimmed(out))
	}
	docker("kill", orchestrator.GatekeeperContainer(cfg.ID))
	status := waitState(t, cfg.ID, sandbox.StateError, 15*time.Second)
	if !strings.Contains(status.Error, "gatekeeper") {
		t.Errorf("error %q", status.Error)
	}
	if _, code := docker("inspect", orchestrator.WorkstationContainer(cfg.ID)); code == 0 {
		t.Error("the workstation outlived its gatekeeper")
	}
}

func TestOrphansAreRemovedAtLaunch(t *testing.T) {
	cfg := lockedConfig("orphan", "Orphan")
	start(t, cfg, nil, nil)
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)

	tmp := tmpVolume(t, cfg.ID)

	// A second launch of the app finds what the first one left.
	second, err := orchestrator.New(context.Background(), orchestrator.ColimaEndpoint(), func(sandbox.Update) {})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.RemoveOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{orchestrator.WorkstationContainer(cfg.ID), orchestrator.GatekeeperContainer(cfg.ID)} {
		if _, code := docker("inspect", name); code == 0 {
			t.Errorf("%s survived", name)
		}
	}
	// Its Home did: what a launch removes is containers, and a sandbox
	// whose app died is still its user's, with what they installed.
	if out, code := docker("volume", "inspect", "-f", "{{.Name}}", orchestrator.HomeVolume(cfg.ID)); code != 0 {
		t.Errorf("the orphan's Home did not survive the launch: %s", trimmed(out))
	}
	// Its /tmp did not: Docker removed it with the workstation.
	if out, code := docker("volume", "inspect", "-f", "{{.Name}}", tmp); code == 0 {
		t.Errorf("the orphan's /tmp survived the launch: %s", trimmed(out))
	}
	// Its Carrier went when its Packet stream did.
	waitFor(t, 15*time.Second, "the orphan's sandbox to be known to have ended", func() bool {
		return updates.statusOf(cfg.ID).State == sandbox.StateError
	})
}

// A sandbox is not started behind a backstop other than the one the app
// loaded: someone, or something that got into the VM, changed it.
func TestNoSandboxStartsBehindAnotherBackstop(t *testing.T) {
	// A rule that lets every bridge through, slipped in ahead of the rest.
	if out, code := docker("exec", helper, "nft", "insert", "rule", "inet", "zeolite_backstop", "bridged_forward", "accept"); code != 0 {
		t.Fatalf("changing the backstop: %s", out)
	}
	// Loading it afresh puts it right, whatever this test finds.
	t.Cleanup(func() {
		if err := orch.LoadBackstop(context.Background()); err != nil {
			t.Errorf("reloading the backstop: %v", err)
		}
	})
	cfg := lockedConfig("backstop-changed", "Backstop changed")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	freshHome(t, cfg.ID)
	t.Cleanup(func() { orch.Stop(context.Background(), cfg.ID) })
	err := orch.Start(ctx, cfg, nil, orchestrator.StartOptions{Carry: carry(t, cfg.ID)})
	if err == nil || !strings.Contains(err.Error(), "backstop") {
		t.Fatalf("with a changed backstop, starting a sandbox gave %v, want a refusal that names the backstop", err)
	}
	if _, code := docker("inspect", orchestrator.GatekeeperContainer(cfg.ID)); code == 0 {
		t.Error("a gatekeeper was left behind a changed backstop")
	}
	if _, code := docker("inspect", orchestrator.WorkstationContainer(cfg.ID)); code == 0 {
		t.Error("a workstation was left behind a changed backstop")
	}

	// Reloaded, it is the app's again, and the sandbox starts.
	if err := orch.LoadBackstop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := orch.Start(ctx, cfg, nil, orchestrator.StartOptions{Carry: carry(t, cfg.ID)}); err != nil {
		t.Fatalf("after the backstop was reloaded: %v", err)
	}
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)
}
