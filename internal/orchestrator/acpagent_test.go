package orchestrator

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"zeolite/internal/sandbox"
	"zeolite/internal/sandboximage"
)

// workstationJSON is what Docker answers of a sandbox's workstation: it
// runs or not, with the labels and the mounts given.
func workstationJSON(sandboxID string, running bool, labels map[string]string, mounts ...map[string]any) string {
	all := map[string]string{labelSandbox: sandboxID, labelRole: roleWorkstation}
	for k, v := range labels {
		all[k] = v
	}
	inspected, _ := json.Marshal(map[string]any{
		"Id":     "c0ffee",
		"State":  map[string]any{"Running": running},
		"Config": map[string]any{"Labels": all},
		"Mounts": mounts,
	})
	return string(inspected)
}

var (
	homeMount   = map[string]any{"Type": "volume", "Name": "zeolite-s1-home", "Source": "/var/lib/docker/volumes/zeolite-s1-home/_data", "Destination": "/workspace", "RW": true}
	sharedMount = map[string]any{"Type": "bind", "Source": "/Users/Shared/zeolite/shared.noindex/research", "Destination": "/workspace/shared", "RW": true}
)

// A workstation says by a label that its sandbox was started with an
// ACP Bridge, and has none where it was not.
func TestAWorkstationSaysThatItWasStartedWithAnACPBridge(t *testing.T) {
	cfg := sandbox.NewConfig("s1", "Research")
	if got := workstationLabels(cfg); len(got) != 2 || got[labelSandbox] != "s1" || got[labelRole] != roleWorkstation {
		t.Errorf("a workstation without an ACP Bridge is labelled %v", got)
	}
	cfg.ACPBridge = true
	if got := workstationLabels(cfg); len(got) != 3 || got["zeolite.acp-bridge"] != "true" {
		t.Errorf("a workstation with an ACP Bridge is labelled %v", got)
	}
	// The gatekeeper never has it.
	if got := labels("s1", roleGatekeeper); len(got) != 2 {
		t.Errorf("a gatekeeper is labelled %v", got)
	}
}

// An agent is started for an ACP Bridge only in a workstation that
// runs, was started with one and mounts a Shared Folder; only an agent
// the app knows to speak the protocol; and by the supervisor's command
// alone, with the agent's own command and what its client added as
// words.
func TestAnAgentIsStartedOnlyForAnACPBridge(t *testing.T) {
	f, o := newFakeDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	opencode, _ := sandbox.KnownAgent("opencode")
	bridged := map[string]string{labelACPBridge: "true"}

	for _, tt := range []struct {
		name, workstation, says string
	}{
		{"no workstation", "", "is not running"},
		{"a workstation that stopped", workstationJSON("s1", false, bridged, homeMount, sharedMount), "is not running"},
		{"another sandbox's workstation", workstationJSON("s2", true, bridged, homeMount, sharedMount), "is not running"},
		{"a gatekeeper under the workstation's name", workstationJSON("s1", true, map[string]string{labelACPBridge: "true", labelRole: roleGatekeeper}, sharedMount), "is not running"},
		{"a sandbox started without an ACP Bridge", workstationJSON("s1", true, nil, homeMount, sharedMount), "was not started with an ACP Bridge"},
		{"a label that says something else", workstationJSON("s1", true, map[string]string{labelACPBridge: "false"}, homeMount, sharedMount), "was not started with an ACP Bridge"},
		{"no Shared Folder", workstationJSON("s1", true, bridged, homeMount), "mounts no Shared Folder"},
		{"a volume where the Shared Folder is", workstationJSON("s1", true, bridged, homeMount, map[string]any{"Type": "volume", "Source": "/var/lib/docker/volumes/x/_data", "Destination": "/workspace/shared"}), "mounts no Shared Folder"},
		{"a bind elsewhere", workstationJSON("s1", true, bridged, homeMount, map[string]any{"Type": "bind", "Source": "/Users/Shared/zeolite/shared.noindex/research", "Destination": "/workspace/shared/inner"}), "mounts no Shared Folder"},
	} {
		f.mu.Lock()
		f.containers = map[string]string{}
		if tt.workstation != "" {
			f.containers[WorkstationContainer("s1")] = tt.workstation
		}
		f.mu.Unlock()
		a, err := startACPAgent(ctx, o.docker, "s1", opencode, nil)
		if err == nil || !strings.Contains(err.Error(), tt.says) {
			t.Errorf("%s: started %v, %v; want an error that says %q", tt.name, a, err, tt.says)
		}
	}
	f.mu.Lock()
	asked := len(f.execs)
	f.mu.Unlock()
	if asked != 0 {
		t.Fatalf("%d commands were run in a workstation that was refused", asked)
	}

	// An agent the app does not know, or does not know to speak the
	// protocol, is refused before Docker is asked anything.
	endpoint := "tcp://" + strings.TrimPrefix(f.server.URL, "http://")
	for _, agent := range []string{"", "bash", "sh", "node", "/bin/sh", "OpenCode", "opencode ", "opencode acp", "supervisor", "claude-agent-acp"} {
		if a, err := StartACPAgent(ctx, endpoint, "s1", agent, nil); err == nil || !strings.Contains(err.Error(), "is no agent that can be started here") {
			t.Errorf("the agent %q: started %v, %v", agent, a, err)
		}
	}
	for _, id := range []string{"", "S1", "../s1", "s1/json", "s1?x"} {
		if a, err := StartACPAgent(ctx, endpoint, id, "opencode", nil); err == nil || !strings.Contains(err.Error(), "is no sandbox's ID") {
			t.Errorf("the sandbox %q: started %v, %v", id, a, err)
		}
	}
	if a, err := startACPAgent(ctx, o.docker, "s1", sandbox.Agent{Title: "Mute", Executable: "mute"}, nil); err == nil {
		t.Errorf("an agent with no command to speak the protocol by was started: %v", a)
	}

	// In a workstation that was started with an ACP Bridge, the agent
	// is started by the supervisor, as the agent's user, in the Shared
	// Folder, with no terminal and nothing of this Mac's environment.
	f.mu.Lock()
	f.containers[WorkstationContainer("s1")] = workstationJSON("s1", true, bridged, homeMount, sharedMount)
	f.mu.Unlock()
	hostile := []string{"--model", "x; touch pwned", "$(id)", ""}
	a, err := StartACPAgent(ctx, endpoint, "s1", "opencode", hostile)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.SharedFolder(); got != "/Users/Shared/zeolite/shared.noindex/research" {
		t.Errorf("the Shared Folder is %q, want the folder the workstation mounts", got)
	}
	f.mu.Lock()
	execs := slices.Clone(f.execs)
	f.mu.Unlock()
	want := append([]string{"/usr/local/bin/supervisor", "acp", "opencode", "acp"}, hostile...)
	if len(execs) != 1 || !slices.Equal(execs[0].Cmd, want) {
		t.Fatalf("the workstation was asked to run %+v, want %q", execs, want)
	}
	if e := execs[0]; e.Container != "c0ffee" || e.User != sandboximage.WorkstationUser || e.WorkingDir != "/workspace/shared" || e.Tty || e.Privileged || !e.AttachStdin || !e.AttachStdout || len(e.Env) != 0 {
		t.Errorf("the agent is started as %+v", e)
	}
	// Its streams are the agent's.
	fmt.Fprintln(a, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if line, err := bufio.NewReader(a).ReadString('\n'); err != nil || line != `{"jsonrpc":"2.0","id":1,"method":"initialize"}`+"\n" {
		t.Errorf("the agent's stream gave %q (%v)", line, err)
	}
	// Closing it ends the agent's input, and its status is the
	// command's.
	f.mu.Lock()
	f.execExit = 127
	f.mu.Unlock()
	a.Close()
	if status, err := a.Wait(ctx); err != nil || status != 127 {
		t.Errorf("the agent ended with %d (%v), want 127", status, err)
	}

	// Each known agent's command is the supervisor's, then the agent's
	// own.
	for _, agent := range sandbox.KnownAgents {
		got := acpAgentCommand(agent, []string{"x"})
		if want := append(append([]string{"/usr/local/bin/supervisor", "acp"}, agent.ACP...), "x"); !slices.Equal(got, want) {
			t.Errorf("%s is started by %q, want %q", agent.Title, got, want)
		}
	}
}
