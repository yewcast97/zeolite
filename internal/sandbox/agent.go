package sandbox

import "time"

// Agent is an agent the app knows: what the dashboard calls it, its
// Agent Executable, the command by which it speaks ACP, and where it
// keeps its MCP registry.
type Agent struct {
	Title string
	// Executable is the Agent Executable: the name of the program the
	// agent's own documentation starts it by. Its user installs it in a
	// workstation and starts it there, by typing that name in a Terminal
	// Session; the app installs none, and starts one only for an ACP
	// Bridge.
	Executable string
	// ACP is the command by which the agent speaks the Agent Client
	// Protocol on its standard streams, as its maker documents it: the
	// Agent Executable with its arguments, or a program of another name
	// that its user installs besides. It is what a sandbox's ACP Bridge
	// has started in the workstation, and is empty for an agent that is
	// not known to speak the protocol: an ACP Bridge starts no such
	// agent.
	ACP []string
	// Registry is the file in which the agent looks for MCP servers, and
	// the shape of a server there, as its maker documents them. A
	// workstation's supervisor writes the sandbox's servers into it at
	// each start.
	Registry Registry
}

// RegistryFormat is the language of an agent's MCP registry.
type RegistryFormat string

const (
	RegistryJSON RegistryFormat = "json"
	RegistryTOML RegistryFormat = "toml"
)

// Registry is where one agent keeps the MCP servers it starts, and how
// it writes one down.
type Registry struct {
	// File is the registry: a path from the agent's user's home, or an
	// absolute path for a file that is the app's own, which the agent is
	// sent to by Variable.
	File string
	// Variable, where it is set, names the environment variable by which
	// the agent is told of File: the file is then written whole by the
	// app, beside the agent's own, which is not touched.
	Variable string
	Format   RegistryFormat
	// Servers is the key under which the file holds its servers, each
	// under its name: an object in JSON, a table in TOML.
	Servers string
	// Type is what a server that is a program of the workstation's is
	// said to be, under the key "type", where the agent asks for it.
	Type string
	// CommandList says that a server's command line is one list under
	// "command"; otherwise the program is under "command" and the rest
	// under "args".
	CommandList bool
	// Enabled says that a server is switched on by "enabled".
	Enabled bool
	// TimeoutKey is the key under which a server says how long the agent
	// waits for one call, and TimeoutUnit what its number counts; empty
	// where the agent has no such key.
	TimeoutKey  string
	TimeoutUnit time.Duration
}

// SupervisorDir is the folder of a workstation in which its supervisor
// keeps what its commands share, and the registries that are the app's
// own to write whole: on /tmp, made anew with each workstation.
const SupervisorDir = TmpPath + "/zeolite"

// KnownAgents are the agents the app knows, each under the name its
// own documentation starts it by, with its registry as its maker's
// documentation had it on 20 November 2026, and the command by which
// it speaks ACP as it had that on 27 November 2026.
var KnownAgents = []Agent{
	// Claude Code and Codex speak ACP by a program of another name,
	// which their user installs besides the agent.
	{Title: "Claude Code", Executable: "claude", ACP: []string{"claude-agent-acp"}, Registry: Registry{
		File: ".claude.json", Format: RegistryJSON, Servers: "mcpServers", Type: "stdio",
	}},
	{Title: "Codex", Executable: "codex", ACP: []string{"codex-acp"}, Registry: Registry{
		File: ".codex/config.toml", Format: RegistryTOML, Servers: "mcp_servers", TimeoutKey: "tool_timeout_sec", TimeoutUnit: time.Second,
	}},
	{Title: "Grok", Executable: "grok", ACP: []string{"grok", "agent", "stdio"}, Registry: Registry{
		File: ".grok/config.toml", Format: RegistryTOML, Servers: "mcp_servers",
	}},
	// OpenCode's own config may hold comments, which no strict reader
	// takes: the app writes a file of its own, and names it to OpenCode.
	{Title: "OpenCode", Executable: "opencode", ACP: []string{"opencode", "acp"}, Registry: Registry{
		File: SupervisorDir + "/opencode.json", Variable: "OPENCODE_CONFIG", Format: RegistryJSON, Servers: "mcp",
		Type: "local", CommandList: true, Enabled: true, TimeoutKey: "timeout", TimeoutUnit: time.Millisecond,
	}},
	{Title: "Kimi Code", Executable: "kimi", ACP: []string{"kimi", "acp"}, Registry: Registry{
		File: ".kimi-code/mcp.json", Format: RegistryJSON, Servers: "mcpServers",
	}},
	{Title: "Pi", Executable: "omp", ACP: []string{"omp", "acp"}, Registry: Registry{ // oh-my-pi
		File: ".omp/agent/mcp.json", Format: RegistryJSON, Servers: "mcpServers",
	}},
	{Title: "Antigravity", Executable: "agy", ACP: []string{"agy", "acp"}, Registry: Registry{
		File: ".gemini/config/mcp_config.json", Format: RegistryJSON, Servers: "mcpServers",
	}},
}

// KnownAgent finds the agent the app knows under an Agent Executable.
func KnownAgent(executable string) (Agent, bool) {
	for _, a := range KnownAgents {
		if a.Executable == executable {
			return a, true
		}
	}
	return Agent{}, false
}

// RegistryEnvironment is what a workstation's environment says of the
// registries that are the app's own files: each agent's variable, and
// the file it names.
func RegistryEnvironment() map[string]string {
	env := map[string]string{}
	for _, a := range KnownAgents {
		if a.Registry.Variable != "" {
			env[a.Registry.Variable] = a.Registry.File
		}
	}
	return env
}
