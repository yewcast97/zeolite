//go:build integration

package integration

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"zeolite/internal/orchestrator"
	"zeolite/internal/pack"
	"zeolite/internal/sandbox"
)

// newPack is a Capability Pack whose one build step is new with each
// run of the tests: no image of it is there, and its step is not one
// Docker has kept. The step is given the nonce it writes as $nonce.
func newPack(name, step string) (m pack.Manifest, nonce string) {
	nonce = strconv.FormatInt(time.Now().UnixNano(), 36)
	return pack.Manifest{
		Name:         name,
		Title:        name,
		RuntimeLayer: pack.RuntimeLayer{Run: []string{"nonce=" + nonce + "; " + step}},
	}, nonce
}

// The steps of a workstation's image run in the namespace of the sandbox
// the image is built for: they have loopback and the sandbox's TUN
// device and no other device, they ask names of the sandbox's resolver,
// they are seen at the sandbox's exit address, and what its ruleset
// refuses the sandbox it refuses them, at once.
func TestAWorkstationsImageIsBuiltInItsSandboxsNamespace(t *testing.T) {
	cfg := openConfig(t, "build-ns")
	gateway := hostGateway(t)
	m, nonce := newPack("test-built-in-namespace", strings.Join([]string{
		"mkdir /built && echo $nonce > /built/nonce",
		"ls /sys/class/net | sort | tr '\\n' ' ' > /built/devices",
		"grep -v '^#' /etc/resolv.conf | tr '\\n' ';' > /built/resolver",
		"{ curl -s --max-time 30 " + publicProbeURL + " > /built/exit; echo $? > /built/public; }",
		"{ curl -s --max-time 15 -o /dev/null http://" + gateway.String() + ":22; echo $? > /built/private; }",
		// What a step holds that a workstation does not: its capabilities.
		"grep CapEff /proc/self/status > /built/capabilities",
	}, " && "))
	cfg.Packs = []string{m.Name}
	tag := workstationImage(t, []pack.Manifest{m})
	if _, code := docker("image", "inspect", tag); code == 0 {
		t.Fatalf("the image %s is there before its sandbox built it", tag)
	}
	// Registered before the sandbox's stop, so that it runs after it.
	t.Cleanup(func() { docker("rmi", tag) })
	freshHome(t, cfg.ID)
	startUnbuilt(t, cfg, []pack.Manifest{m}, orchestrator.StartOptions{})
	waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)

	read := func(file string) string {
		out, _ := inSandbox(cfg.ID, "cat /built/"+file)
		return trimmed(out)
	}
	if got := read("nonce"); got != nonce {
		t.Fatalf("the workstation's image was not built by this start: its step wrote %q, want %q", got, nonce)
	}
	if got := read("devices"); got != "lo "+sandbox.TUNDevice {
		t.Errorf("the build's step had the devices %q: the sandbox's namespace has lo and %s", got, sandbox.TUNDevice)
	}
	if got := read("resolver"); !strings.Contains(got, "nameserver "+cfg.Gate.Resolver+";") || strings.Count(got, "nameserver") != 1 {
		t.Errorf("the build's step had the resolver file %q: the sandbox's names %s alone", got, cfg.Gate.Resolver)
	}
	if got, want := read("exit"), cfg.AllowedInterfaces[0].ExpectedExitAddresses[0]; read("public") != "0" || got != want {
		t.Errorf("the build's step reached the internet with curl's code %s and was seen at %q: the sandbox is seen at %s", read("public"), got, want)
	}
	// Refused at once, as the sandbox's ruleset refuses, and not left
	// unanswered, as the Backstop leaves what is on a bridge.
	if got := read("private"); got != "7" {
		t.Errorf("the build's step tried the Mac at %s and curl ended with %s: want 7, refused at once", gateway, got)
	}
	t.Logf("a build's step holds %s", read("capabilities"))
}

// A workstation's image is built by way of its sandbox's gate: a
// sandbox whose gate does not open builds none, and says why.
func TestASandboxWhoseGateStaysLockedBuildsNoImage(t *testing.T) {
	cfg := lockedConfig("build-locked", "Locked, with no image")
	m, _ := newPack("test-built-behind-a-locked-gate", "echo $nonce > /built-nonce")
	cfg.Packs = []string{m.Name}
	tag := workstationImage(t, []pack.Manifest{m})
	t.Cleanup(func() { docker("rmi", tag) })
	freshHome(t, cfg.ID)
	t.Cleanup(func() { orch.Stop(context.Background(), cfg.ID) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	err := orch.Start(ctx, cfg, []pack.Manifest{m}, orchestrator.StartOptions{Carry: carry(t, cfg.ID)})
	if err == nil {
		t.Fatal("a sandbox whose gate stays locked started with an image that was not there: it was built by no way out of the sandbox's")
	}
	for _, want := range []string{"by way of its gate", string(sandbox.LockProbeUnanswered)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the start failed with %q, which does not say %q", err, want)
		}
	}
	if _, code := docker("image", "inspect", tag); code == 0 {
		t.Errorf("the image %s was built behind a gate that never opened", tag)
	}
	if out, _ := docker("ps", "-a", "--format", "{{.Names}}", "--filter", "name=zeolite-"+cfg.ID); trimmed(out) != "" {
		t.Errorf("the sandbox that did not start left %q", trimmed(out))
	}
}

// A sandbox that is stopped while its image is built takes the build
// with it: the start fails, the step that was running is ended, and no
// container and no image is left of it.
func TestABuildEndsWithItsSandbox(t *testing.T) {
	cfg := openConfig(t, "build-stopped")
	m, nonce := newPack("test-build-stopped", "sleep 300")
	cfg.Packs = []string{m.Name}
	tag := workstationImage(t, []pack.Manifest{m})
	t.Cleanup(func() { docker("rmi", tag) })
	freshHome(t, cfg.ID)
	t.Cleanup(func() { orch.Stop(context.Background(), cfg.ID) })
	containers := func() map[string]bool {
		out, _ := docker("ps", "-a", "--no-trunc", "--format", "{{.ID}}")
		ids := map[string]bool{}
		for _, id := range strings.Fields(out) {
			ids[id] = true
		}
		return ids
	}
	before := containers()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	started := make(chan error, 1)
	go func() {
		started <- orch.Start(ctx, cfg, []pack.Manifest{m}, orchestrator.StartOptions{Carry: carry(t, cfg.ID)})
	}()
	// The step runs: a container of the build's, which Docker made.
	waitFor(t, 2*time.Minute, "the build's step to run", func() bool {
		out, _ := docker("ps", "--no-trunc", "--format", "{{.Command}}")
		return strings.Contains(out, nonce)
	})
	orch.Stop(context.Background(), cfg.ID)
	select {
	case err := <-started:
		if err == nil {
			t.Error("a sandbox that was stopped while its image was built started")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the start of a sandbox that was stopped while its image was built did not end")
	}
	waitFor(t, 30*time.Second, "the build's step to be gone", func() bool {
		for id := range containers() {
			if !before[id] {
				return false
			}
		}
		return true
	})
	if _, code := docker("image", "inspect", tag); code == 0 {
		t.Errorf("the image %s was built though its sandbox was stopped", tag)
	}
}
