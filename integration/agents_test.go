//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"zeolite/internal/orchestrator"
	"zeolite/internal/sandbox"
)

// The app installs no agent: its user does, in a Terminal Session, with
// the installer the agent's maker publishes. These tests do the same,
// in a login shell of the workstation, with real installers fetched
// from the internet, and ask no model anything.

// agentInstallers are the command lines the makers' own documentation
// gives to install each agent the app knows, as read on 19 November
// 2026, by Agent Executable. They are here and nowhere in the app: each
// needs only what a workstation has (bash, curl, tar, a checksum tool)
// and writes only under the home.
var agentInstallers = map[string]string{
	"claude": "curl -fsSL https://claude.ai/install.sh | bash",
	// Codex's installer would ask, on a terminal, whether to start it.
	"codex":    "curl -fsSL https://chatgpt.com/codex/install.sh | CODEX_NON_INTERACTIVE=1 sh",
	"grok":     "curl -fsSL https://x.ai/cli/install.sh | bash",
	"opencode": "curl -fsSL https://opencode.ai/v2/install | bash",
	"kimi":     "curl -fsSL https://code.kimi.com/kimi-code/install.sh | bash",
	"omp":      "curl -fsSL https://omp.sh/install | sh", // oh-my-pi
	"agy":      "curl -fsSL https://antigravity.google/cli/install.sh | bash",
}

// agentLists are the command lines by which an agent lists the MCP
// servers of its registry, for each agent that has one, as each one's
// own help gave it on 20 November 2026. Kimi Code and Pi have none.
var agentLists = map[string]string{
	"claude":   "claude mcp list",
	"codex":    "codex mcp list",
	"grok":     "grok mcp list",
	"opencode": "opencode mcp list",
	"agy":      "agy mcp list",
}

// agentsThatStartTheirServers are the agents that start the MCP servers
// of their registry when they are started on a terminal, before anyone
// logged in: each with what its first screen shows where it asks a
// question first, which Enter answers. OpenCode starts them too, but in
// a service of its own that its listing has started by then.
var agentsThatStartTheirServers = map[string]string{
	"kimi": "Trust and continue",
	"omp":  "",
	"agy":  "",
}

// listsTheServer has an agent list the MCP servers of its registry, and
// returns the line that names the server sandbox, empty where none
// does. An agent that keeps a service of its own is asked again while
// that starts.
func listsTheServer(t *testing.T, sandboxID, executable string) string {
	t.Helper()
	var line, out string
	deadline := time.Now().Add(90 * time.Second)
	for line == "" && time.Now().Before(deadline) {
		out, _ = inLoginShell(sandboxID, "cd /workspace && timeout 60 "+agentLists[executable]+" </dev/null 2>&1")
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, sandbox.StateServer) && !strings.Contains(l, "No MCP") {
				line = trimmed(l)
			}
		}
		if line == "" {
			time.Sleep(2 * time.Second)
		}
	}
	if line == "" {
		t.Logf("%s listed:\n%s", agentLists[executable], out)
	}
	return line
}

// initialized counts the times an agent's client met the server sandbox
// of a sandbox, as the bridge reported them.
func initialized(sandboxID string) int {
	n := 0
	for _, call := range updates.mcpCallsOf(sandboxID) {
		if call.Server == sandbox.StateServer && call.FromAgent && call.Method == "initialize" {
			n++
		}
	}
	return n
}

// startedByItsName starts an agent in a Terminal Session, typed by its
// name as its user types it, in a terminal as a Pane shows one. It says
// whether the workstation then names that agent as the one that runs in
// the session, and whether the agent's client met the server sandbox.
// Nobody logs in, and no model is asked anything. The session is ended.
func startedByItsName(t *testing.T, sandboxID, executable string) (named, met bool) {
	t.Helper()
	shown, session := showSession(t, sandboxID, nil)
	before := initialized(sandboxID)
	shown.run(executable)
	named = shown.until(60*time.Second, func() bool {
		status, _ := updates.workstationOf(sandboxID)
		for _, s := range status.Sessions {
			if s.ID == session.ID() && s.Agent == executable {
				return true
			}
		}
		return false
	})
	if question, starts := agentsThatStartTheirServers[executable]; starts {
		if question != "" && shown.until(30*time.Second, func() bool { return strings.Contains(shown.term.Text(), question) }) {
			shown.tt.Key(0, ui.KeyEnter)
		}
		met = shown.until(60*time.Second, func() bool { return initialized(sandboxID) > before })
	}
	if !named || (!met && agentsThatStartTheirServers[executable] != "") {
		t.Logf("%s, started by its name, shows:\n%s", executable, shown.term.Text())
	}
	// Its session ends, and everything started in it.
	shown.term.Close()
	session.Close()
	waitFor(t, 30*time.Second, "the session of "+executable+" to end", func() bool {
		status, _ := updates.workstationOf(sandboxID)
		for _, s := range status.Sessions {
			if s.ID == session.ID() {
				return false
			}
		}
		return true
	})
	return named, met
}

// installByHand runs an agent's maker's installer in a sandbox, in a
// login shell, as its user would type it in a Pane, and returns the
// last of what it printed, for the test's log.
func installByHand(t *testing.T, sandboxID, executable string, limit time.Duration) string {
	t.Helper()
	line, ok := agentInstallers[executable]
	if !ok {
		t.Fatalf("the tests have no installer for %q", executable)
	}
	out, _ := inLoginShell(sandboxID, "timeout "+itoa(int(limit.Seconds()))+" bash -c "+shellQuote(line)+" 2>&1")
	if len(out) > 1200 {
		out = out[len(out)-1200:]
	}
	return out
}

// installedByHand installs an agent with its maker's installer, and
// says where a login shell then finds it. A gate locks now and then for
// a probe that went unanswered, and ends every connection with it: as a
// user would, the test tries again.
func installedByHand(t *testing.T, sandboxID string, agent sandbox.Agent) string {
	t.Helper()
	const tries = 3
	for try := 1; ; try++ {
		tail := installByHand(t, sandboxID, agent.Executable, 15*time.Minute)
		where := whereInstalled(sandboxID, agent.Executable)
		if strings.HasPrefix(where, sandbox.HomePath+"/") {
			return where
		}
		if try == tries {
			t.Errorf("%s: after its installer, %d times, a login shell finds it at %q; the installer ended with:\n%s", agent.Title, tries, where, tail)
			return ""
		}
		t.Logf("%s: its installer left nothing a login shell finds, and is run again; it ended with:\n%s", agent.Title, tail)
		waitState(t, sandboxID, sandbox.StateGateOpen, 60*time.Second)
	}
}

// whereInstalled is where a login shell of a workstation finds an
// executable by its name, empty where it finds none.
func whereInstalled(sandboxID, executable string) string {
	out, _ := inLoginShell(sandboxID, "type -P "+shellQuote(executable))
	return trimmed(out)
}

// Every agent the app knows is installed by its maker's installer, in
// one sandbox behind an open gate: each ends, leaves its executable
// where a login shell finds it by its name, under the home, and nothing
// but what the gate lets pass left the sandbox. Each then finds the
// server sandbox where the supervisor wrote it, in its own registry: it
// lists it, where it has a command that lists, or starts it when it is
// started; and started by its name in a Terminal Session, each is the
// agent the workstation says runs there. What was installed is in the
// sandbox's Home: there after a stop and a start. The installers and
// the agents are their makers', as published that day: this test is the
// one that says when one of them changed.
func TestEveryKnownAgentIsInstalledByHand(t *testing.T) {
	cfg := openConfig(t, "agents-by-hand")
	cfg.Resources.MemoryMB = 6144
	startWith(t, cfg, nil, orchestrator.StartOptions{Link: serveLink(cfg.ID, nil, nil, nil)})
	waitState(t, cfg.ID, sandbox.StateGateOpen, 60*time.Second)
	capture := captureTUN(t, cfg.ID)
	if len(agentInstallers) != len(sandbox.KnownAgents) {
		t.Errorf("the tests have %d installers for the %d agents the app knows", len(agentInstallers), len(sandbox.KnownAgents))
	}
	installed := map[string]string{}
	for _, agent := range sandbox.KnownAgents {
		if where := whereInstalled(cfg.ID, agent.Executable); where != "" {
			t.Fatalf("before anything was installed the workstation holds %s at %s", agent.Title, where)
		}
	}
	for _, agent := range sandbox.KnownAgents {
		began := time.Now()
		where := installedByHand(t, cfg.ID, agent)
		if where == "" {
			continue
		}
		installed[agent.Executable] = where
		t.Logf("%s was installed in %v and is at %s", agent.Title, time.Since(began).Round(time.Second), where)
		// The sandbox is still what it was: its gate open, or locked for
		// a probe that went unanswered and open again soon.
		waitState(t, cfg.ID, sandbox.StateGateOpen, 60*time.Second)
	}
	// Each finds the server sandbox in its own registry, as the
	// supervisor wrote it there: no agent was told of it otherwise.
	for _, agent := range sandbox.KnownAgents {
		if installed[agent.Executable] == "" {
			continue
		}
		_, lists := agentLists[agent.Executable]
		_, starts := agentsThatStartTheirServers[agent.Executable]
		// An agent started behind a gate that locked a moment ago says
		// that it reaches nobody, and ends.
		waitState(t, cfg.ID, sandbox.StateGateOpen, 60*time.Second)
		if !lists && !starts {
			t.Errorf("%s: the tests have no way to ask it for its MCP servers", agent.Title)
		}
		if lists {
			if line := listsTheServer(t, cfg.ID, agent.Executable); line == "" {
				t.Errorf("%s does not list the server %s of its registry", agent.Title, sandbox.StateServer)
			} else {
				t.Logf("%s lists: %s", agent.Title, line)
			}
		}
		// Started by its name, it is the agent the workstation names,
		// and meets the server where it starts its servers unasked.
		named, met := startedByItsName(t, cfg.ID, agent.Executable)
		if !named {
			t.Errorf("%s, started by its name in a Terminal Session, is not the agent the workstation says runs there", agent.Title)
		}
		if starts && !met {
			t.Errorf("%s, started by its name, did not start the server %s of its registry", agent.Title, sandbox.StateServer)
		}
		t.Logf("%s, started by its name: named by the workstation %v, met the server %v", agent.Title, named, met)
	}

	// Nothing was written outside the home, which is all a workstation's
	// user can write but /tmp.
	if out, _ := inSandbox(cfg.ID, "ls -A /workspace | sort | tr '\\n' ' '"); trimmed(out) == "" {
		t.Error("the Home is empty after seven installers")
	} else {
		t.Logf("the Home holds: %s", trimmed(out))
	}
	// Everything they fetched left by the gate: TCP to public addresses
	// and name queries to the sandbox's resolver, and nothing else.
	capture.expectGated(t)

	// After a stop and a start each is where it was, with nothing
	// fetched again.
	orch.Stop(context.Background(), cfg.ID)
	startAgain(t, cfg, nil, orchestrator.StartOptions{})
	waitState(t, cfg.ID, sandbox.StateGateOpen, 60*time.Second)
	for executable, where := range installed {
		if again := whereInstalled(cfg.ID, executable); again != where {
			t.Errorf("after a stop and a start %s is found at %q, and was at %q", executable, again, where)
		}
	}
}

// Behind a locked gate an installer fetches nothing: it ends, the agent
// is not there, and nothing of the sandbox left this Mac but its probes.
func TestAnInstallerDoesNotPassALockedGate(t *testing.T) {
	cfg := lockedConfig("agents-locked", "Agents, locked")
	start(t, cfg, nil, nil)
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)
	capture := captureTUN(t, cfg.ID)

	tail := installByHand(t, cfg.ID, "opencode", 5*time.Minute)
	t.Logf("behind a locked gate the installer ended with:\n%s", tail)
	if where := whereInstalled(cfg.ID, "opencode"); where != "" {
		t.Errorf("behind a locked gate the workstation holds opencode at %s", where)
	}
	if out, _ := inSandbox(cfg.ID, "ls -A ~ | sort | tr '\\n' ' '"); strings.Contains(out, ".opencode") {
		t.Errorf("behind a locked gate the Home holds %q", trimmed(out))
	}
	capture.expectProbesOnly(t)
}
