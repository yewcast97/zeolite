//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"zeolite/internal/orchestrator"
)

// Not every container in the VM is a sandbox. Nothing of the app's is on
// a bridge of Docker's, but whatever else the VM's Docker is asked to
// run is, behind no gatekeeper. The backstop is all that stands between
// it and this Mac, which the VM reaches at a private address.

// attemptFrom runs the connection attempt in a throwaway container made
// with the given `docker run` arguments, and says how it ended.
func attemptFrom(t *testing.T, image string, runArgs []string, host string, port int) string {
	t.Helper()
	args := append([]string{"run", "--rm"}, runArgs...)
	args = append(args, "--entrypoint", "python3", image, "-c", attemptScript, host, itoa(port))
	out, code := docker(args...)
	result := trimmed(out)
	switch result {
	case answered, closed, refused, timedOut, unresolved:
		return result
	}
	t.Fatalf("a connection attempt to %s:%d did not run: %q (exit %d)", host, port, out, code)
	return ""
}

func TestOtherBridgesReachNothingPrivate(t *testing.T) {
	// Any workstation image will do: it has Python.
	cfg := lockedConfig("bridges", "Bridges")
	start(t, cfg, nil, nil)
	out, code := docker("inspect", "-f", "{{.Config.Image}}", orchestrator.WorkstationContainer(cfg.ID))
	image := trimmed(out)
	if code != 0 || image == "" {
		t.Fatalf("the workstation has no image: %s", out)
	}

	port, connections := macListener(t)
	gateway := hostGateway(t).String()

	// The control: from the VM's own network the Mac's listener answers.
	// That is the path the backstop closes to every bridge.
	if got := attemptFrom(t, image, []string{"--network", "host"}, gateway, port); got != answered {
		t.Fatalf("from the VM itself the Mac's listener was %s, want %s: the test cannot tell reachable from not", got, answered)
	}
	if n := connections(); n != 1 {
		t.Fatalf("the Mac's listener counted %d connections after one", n)
	}

	// A container on Docker's default bridge.
	type target struct {
		what, host string
		port       int
	}
	targets := []target{
		{"the Mac's listener through the host gateway", gateway, port},
		{"a private address", "10.0.0.1", 80},
		{"the metadata address", "169.254.169.254", 80},
	}
	for _, addr := range vmAddresses(t) {
		targets = append(targets, target{"the VM at " + addr.String(), addr.String(), 22})
	}
	for _, addr := range macAddresses(t) {
		targets = append(targets, target{"the Mac's listener at " + addr.String(), addr.String(), port})
	}
	for _, tg := range targets {
		if got := attemptFrom(t, image, nil, tg.host, tg.port); got == answered {
			t.Errorf("%s answered a container on the default bridge", tg.what)
		}
	}
	if n := connections(); n != 1 {
		t.Errorf("the Mac's listener saw %d connections from the default bridge", n-1)
	}

	// What is on a bridge still has the public internet: names resolve
	// and it answers, where this Mac has one to offer.
	if c, err := net.DialTimeout("tcp4", "1.1.1.1:80", 5*time.Second); err != nil {
		t.Log("this Mac does not reach the public internet: not checking that a build would")
	} else {
		c.Close()
		if got := attemptFrom(t, image, nil, "one.one.one.one", 80); got != answered {
			t.Errorf("from the default bridge a public name was %s, want %s", got, answered)
		}
	}

	// The steps of a build that is given no network of its own, as one
	// run by hand is, with each builder Docker has: the one the app
	// uses, and BuildKit. The app's own builds are on no bridge.
	script, _ := json.Marshal(attemptScript)
	for builder, buildKit := range map[string]string{"the legacy builder": "0", "BuildKit": "1"} {
		before := connections()
		dockerfile := "FROM " + image + "\nARG BUST\nRUN [\"python3\", \"-c\", " + string(script) + ", \"" + gateway + "\", \"" + itoa(port) + "\"]\n"
		args := []string{"-H", orchestrator.ColimaEndpoint(), "build", "--no-cache", "--build-arg", "BUST=" + itoa(int(time.Now().UnixNano()%1e9))}
		if buildKit == "1" {
			args = append(args, "--progress=plain") // or it does not print what a step printed
		}
		cmd := exec.Command("docker", append(args, "-")...)
		cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT="+buildKit)
		cmd.Stdin = strings.NewReader(dockerfile)
		var log bytes.Buffer
		cmd.Stdout, cmd.Stderr = &log, &log
		if err := cmd.Run(); err != nil {
			t.Errorf("a build with %s failed: %v\n%s", builder, err, log.String())
			continue
		}
		outcome := ""
		for _, word := range []string{answered, closed, refused, timedOut} {
			if strings.Contains(log.String(), word) {
				outcome = word
			}
		}
		if outcome == "" {
			t.Errorf("a build step with %s did not say how its attempt ended:\n%s", builder, log.String())
		}
		if outcome == answered || connections() != before {
			t.Errorf("a build step with %s reached the Mac's listener (%s, %d connections)", builder, outcome, connections()-before)
		}
	}
}
