//go:build integration

package integration

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"zeolite/internal/orchestrator"
	"zeolite/internal/sandbox"
)

// Every agent the app knows is given work, as its user gives it: each
// installed by hand, told of a model in its own config and environment,
// and started by its name in a Terminal Session. The models are real,
// all asked through OpenRouter with the experiments' key, and cheap:
// each agent is driven by its maker's own cheap model, where its maker
// makes models, and by the experiments' model otherwise. What each
// agent needs to be sent to OpenRouter is its maker's documentation's,
// as read on 22 November 2026, and is here and nowhere in the app.

// agentJob is how one agent is given a job with nobody at its screen.
type agentJob struct {
	// model is the model that drives the agent, as OpenRouter knows it.
	model string
	// environment is what the agent's user exports for it in ~/.bashrc,
	// as lines of shell. The key is named there by its variable and
	// written once, in the line that sets that variable.
	environment []string
	// configFile is a file of the agent's own, from the home, and config
	// what its user adds to it: where the agent is told of a model in a
	// file.
	configFile, config string
	// command is what its user types to give the agent a prompt and have
	// it end by itself, asking nothing: the prompt is quoted for a shell.
	command func(prompt string) string
	// translated says that the agent speaks the Gemini API alone, and is
	// sent to the Translator, which asks OpenRouter in its place.
	translated bool
}

// translatorPort is the port of the workstation's loopback on which a
// test's Translator answers.
const translatorPort = 8765

// agentJobs are the jobs' ways, by Agent Executable.
var agentJobs = map[string]agentJob{
	// Claude Code is sent to OpenRouter's Anthropic API, and asks nothing
	// of its maker meanwhile.
	"claude": {
		model: "anthropic/claude-haiku-5.5",
		environment: []string{
			"export ANTHROPIC_BASE_URL=https://openrouter.ai/api",
			`export ANTHROPIC_AUTH_TOKEN="$` + keyVariable + `"`,
			"export ANTHROPIC_API_KEY=",
			"export ANTHROPIC_MODEL=anthropic/claude-haiku-5.5",
			"export ANTHROPIC_DEFAULT_HAIKU_MODEL=anthropic/claude-haiku-5.5",
			"export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		},
		command: func(prompt string) string { return "claude -p " + prompt + " --dangerously-skip-permissions" },
	},
	// Codex takes a provider on its command line: OpenRouter's Responses
	// API, with the key under the variable named there.
	"codex": {
		model: "openai/gpt-6-luna",
		command: func(prompt string) string {
			return "codex exec --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check" +
				" -c model_provider=openrouter -c model_providers.openrouter.name=OpenRouter" +
				" -c model_providers.openrouter.base_url=https://openrouter.ai/api/v1" +
				" -c model_providers.openrouter.env_key=" + keyVariable +
				" -c model_providers.openrouter.wire_api=responses" +
				" -m openai/gpt-6-luna " + prompt
		},
	},
	// Grok takes a model of its user's in its config, beside the servers
	// the supervisor wrote there.
	"grok": {
		model:      "x-ai/grok-4.3",
		configFile: ".grok/config.toml",
		config: "\n[model.openrouter]\nmodel = \"x-ai/grok-4.3\"\nbase_url = \"https://openrouter.ai/api/v1\"\n" +
			"env_key = \"" + keyVariable + "\"\n",
		command: func(prompt string) string { return "grok -p " + prompt + " -m openrouter --always-approve" },
	},
	// OpenCode's maker makes no model: the experiments' drives it, as it
	// does there.
	"opencode": {
		model:      agentModelID,
		configFile: ".config/opencode/opencode.json",
		config:     modelConfig,
		command:    func(prompt string) string { return "opencode run --auto -m " + agentModel + " -- " + prompt },
	},
	// Kimi Code takes a model from its environment, and starts the
	// servers of a folder its user trusts.
	"kimi": {
		model: "moonshotai/kimi-k2.5",
		environment: []string{
			"export KIMI_MODEL_NAME=moonshotai/kimi-k2.5",
			"export KIMI_MODEL_PROVIDER_TYPE=openai",
			"export KIMI_MODEL_BASE_URL=https://openrouter.ai/api/v1",
			`export KIMI_MODEL_API_KEY="$` + keyVariable + `"`,
			"export KIMI_MODEL_MAX_CONTEXT_SIZE=262144",
			"export KIMI_CODE_TRUST_WORKSPACE=1",
		},
		command: func(prompt string) string { return "kimi -p " + prompt },
	},
	// Pi knows OpenRouter by the key's variable, and its maker makes no
	// model either.
	"omp": {
		model: agentModelID,
		command: func(prompt string) string {
			return "omp -p --model " + agentModel + " --auto-approve --allow-home --no-title " + prompt
		},
	},
	// Antigravity speaks the Gemini API, to its maker or to an endpoint
	// its user names: the Translator. The key it is given is no key: the
	// Translator has the real one.
	"agy": {
		model: "google/gemini-4-hydrogen",
		environment: []string{
			"export GEMINI_API_KEY=the-translator-has-the-key",
			fmt.Sprintf("export GOOGLE_GEMINI_BASE_URL=http://127.0.0.1:%d", translatorPort),
		},
		configFile: ".gemini/antigravity-cli/settings.json",
		config:     `{"modelProvider": "gemini"}` + "\n",
		command:    func(prompt string) string { return "agy -p " + prompt + " --dangerously-skip-permissions" },
		translated: true,
	},
}

// translatorPath is where a test puts the Translator in a workstation.
const translatorPath = sandbox.TmpPath + "/gemini_translator.py"

// startTranslator starts the Translator in a workstation, with the key
// in its environment, and returns once it answers. It ends with the
// workstation.
func startTranslator(t *testing.T, sandboxID, key, model string) {
	t.Helper()
	program, err := os.ReadFile("testdata/gemini_translator.py")
	if err != nil {
		t.Fatal(err)
	}
	workstation := orchestrator.WorkstationContainer(sandboxID)
	if out, code := dockerWith(nil, string(program), "exec", "-i", workstation, "sh", "-c", "cat > "+translatorPath); code != 0 {
		t.Fatalf("putting the Translator in the workstation: %s", out)
	}
	// The key is in docker's own environment, which hands it to the
	// program it starts: it is on no command line.
	if out, code := dockerWith([]string{keyVariable + "=" + key}, "", "exec", "-d", "-e", keyVariable, workstation,
		"python3", translatorPath, itoa(translatorPort), model); code != 0 {
		t.Fatalf("starting the Translator: %s", out)
	}
	waitFor(t, 20*time.Second, "the Translator to answer", func() bool {
		out, code := inSandbox(sandboxID, fmt.Sprintf("curl -s --max-time 3 http://127.0.0.1:%d/v1beta/models", translatorPort))
		return code == 0 && strings.Contains(out, model)
	})
}

// tellOfTheModels does what the agents' user does before the first job:
// the key and each agent's variables in ~/.bashrc, which every Terminal
// Session's shell reads, and each agent's own file. The key crosses on a
// command's standard input, and is in that one file of the Home, which
// is deleted with the test's sandbox.
func tellOfTheModels(t *testing.T, sandboxID, key string) {
	t.Helper()
	lines := []string{"export " + keyVariable + "=" + shellQuote(key)}
	for _, agent := range sandbox.KnownAgents {
		lines = append(lines, agentJobs[agent.Executable].environment...)
	}
	workstation := orchestrator.WorkstationContainer(sandboxID)
	if out, code := dockerWith(nil, strings.Join(lines, "\n")+"\n", "exec", "-i", workstation, "sh", "-c", "umask 077 && cat >> /workspace/.bashrc"); code != 0 {
		t.Fatalf("giving the agents their key: exit %d (%d bytes said)", code, len(out))
	}
	for _, agent := range sandbox.KnownAgents {
		job := agentJobs[agent.Executable]
		if job.configFile == "" {
			continue
		}
		file := sandbox.HomePath + "/" + job.configFile
		if out, code := dockerWith(nil, job.config, "exec", "-i", workstation, "sh", "-c",
			"mkdir -p \"$(dirname "+shellQuote(file)+")\" && cat >> "+shellQuote(file)); code != 0 {
			t.Fatalf("telling %s of its model in %s: %s", agent.Title, job.configFile, out)
		}
	}
}

// handsOfSession counts the hands the app was told of for one Terminal
// Session of a sandbox.
func handsOfSession(sandboxID, session string) int {
	n := 0
	for _, hand := range updates.handsOf(sandboxID) {
		if hand == session {
			n++
		}
	}
	return n
}

// givenWork starts an agent by its name in a Terminal Session of its
// own, with a job typed after it as its user types one, and waits for
// the agent to end by itself. It says what the agent left in the file
// it was asked for, and whether the app was told of a hand raised for
// that session. Nothing is taken from what the agent printed, which is
// logged, with the secrets taken out, where the job was not done.
func givenWork(t *testing.T, sandboxID string, agent sandbox.Agent, redact func(string) string) (wrote string, raised bool) {
	t.Helper()
	job := agentJobs[agent.Executable]
	file := "/workspace/from-" + agent.Executable + ".txt"
	ended := sandbox.TmpPath + "/ended-" + agent.Executable
	inSandbox(sandboxID, "rm -f "+file+" "+ended)

	shown, session := showSession(t, sandboxID, nil)
	prompt := "Create the file " + file + " containing exactly the word READY. " +
		"Then call the tool raise_hand of the MCP server sandbox once, and then stop."
	began := time.Now()
	shown.run(job.command(shellQuote(prompt)) + "; echo $? > " + ended)
	done := shown.until(5*time.Minute, func() bool {
		_, code := inSandbox(sandboxID, "test -s "+ended)
		return code == 0
	})
	// The hand crosses the Link after the tool answered.
	raised = shown.until(5*time.Second, func() bool { return handsOfSession(sandboxID, session.ID()) > 0 })
	out, _ := inSandbox(sandboxID, "cat "+file+" 2>/dev/null")
	wrote = trimmed(out)
	exit, _ := inSandbox(sandboxID, "cat "+ended+" 2>/dev/null")
	t.Logf("%s, driven by %s: ended %v (exit %q) after %s, wrote %q, raised its hand %v",
		agent.Title, job.model, done, trimmed(exit), time.Since(began).Round(time.Second), wrote, raised)
	if !done || wrote != "READY" || !raised {
		t.Logf("%s shows:\n%s", agent.Title, redact(shown.term.Text()))
	}
	shown.term.Close()
	session.Close()
	return wrote, raised
}

// Each agent the app knows does a job in a sandbox: installed by its
// maker's installer, sent to OpenRouter as its own documentation says,
// and started by its name in a Terminal Session with a prompt, it writes
// the file it was asked for and raises its hand, which the app takes for
// the session it was typed in. So each one's model is reached through
// the gate, each one runs what its model asks, and each one found the
// tool raise_hand in its own MCP Registry, where the supervisor wrote
// the server: nothing else told it. The verdicts are the file, the
// hand as the app was told of it, and the TUN capture; none is what a
// model says of itself.
func TestEveryKnownAgentDoesAJob(t *testing.T) {
	key := openRouterKey(t)
	cfg := openConfig(t, "agents-at-work")
	cfg.Resources.MemoryMB = 6144
	startWith(t, cfg, nil, orchestrator.StartOptions{Link: serveLink(cfg.ID, nil, nil, nil)})
	noteLocks(t, cfg.ID)
	waitState(t, cfg.ID, sandbox.StateGateOpen, 60*time.Second)
	capture := captureTUN(t, cfg.ID)
	redact := func(text string) string {
		text = strings.ReplaceAll(text, key, "[the API key]")
		if physical, err := seenAt(); err == nil {
			text = strings.ReplaceAll(text, physical, "[this Mac's physical exit address]")
		}
		return text
	}

	if len(agentJobs) != len(sandbox.KnownAgents) {
		t.Errorf("the tests have %d ways to give a job for the %d agents the app knows", len(agentJobs), len(sandbox.KnownAgents))
	}
	installed := map[string]bool{}
	for _, agent := range sandbox.KnownAgents {
		if _, known := agentJobs[agent.Executable]; !known {
			t.Errorf("%s: the tests have no way to give it a job", agent.Title)
			continue
		}
		if where := installedByHand(t, cfg.ID, agent); where != "" {
			installed[agent.Executable] = true
			out, _ := inLoginShell(cfg.ID, "timeout 20 "+agent.Executable+" --version 2>&1 | head -1")
			t.Logf("%s is at %s and says of its version: %s", agent.Title, where, trimmed(out))
		}
	}
	tellOfTheModels(t, cfg.ID, key)
	for _, agent := range sandbox.KnownAgents {
		if job := agentJobs[agent.Executable]; job.translated && installed[agent.Executable] {
			startTranslator(t, cfg.ID, key, job.model)
		}
	}

	for _, agent := range sandbox.KnownAgents {
		if !installed[agent.Executable] {
			continue
		}
		// A model that is cheap does not always do as it is told, and a
		// gate that locked meanwhile ended its connection: a job that
		// was not done is given once more, as its user would give it.
		const tries = 2
		var wrote string
		var raised bool
		for try := 1; try <= tries && (wrote != "READY" || !raised); try++ {
			waitState(t, cfg.ID, sandbox.StateGateOpen, 60*time.Second)
			wrote, raised = givenWork(t, cfg.ID, agent, redact)
		}
		if wrote != "READY" {
			t.Errorf("%s, driven by %s, left %q in the file it was asked for", agent.Title, agentJobs[agent.Executable].model, wrote)
		}
		if !raised {
			t.Errorf("%s, driven by %s, raised no hand the app was told of for its session", agent.Title, agentJobs[agent.Executable].model)
		}
	}

	// From the first installer to the last job, the sandbox's packets
	// were TCP to public addresses and name queries to its resolver.
	capture.expectGated(t)
}
