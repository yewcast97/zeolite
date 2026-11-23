# Capability Packs

A Capability Pack is a YAML manifest that adds a tool to a sandbox. It runs in the workstation, never on the Mac. It has four parts, all optional:

```yaml
name: web-test-browser
title: Web Test Browser
runtimeLayer:        # added to the workstation's image: packages, commands, files, env, read-only volumes
  packages: [chromium]
daemonProcess:       # the tool's background process and the port that says it is ready
  command: [chromium, --headless=new, --remote-debugging-port=9222, ...]
  readinessPort: 9222
mcpBridge:           # an MCP server, written into every known agent's MCP registry
  name: browser
  command: [playwright-mcp, --cdp-endpoint, http://127.0.0.1:9222]
viewportStream:      # how the tool's display reaches the dashboard: cdp_screencast, or none
  type: cdp_screencast
  port: 9222
```

Manifests are trusted: a `runtimeLayer` runs as root when the image is built. Put your own in `~/Library/Application Support/zeolite/packs/`.

Built in:

- **Web Test Browser**: headless Chromium in the sandbox, shown in the dashboard and driven by the Playwright MCP server. You use it as much as an agent does, so its User-Agent says Chrome with its own version, as it would with a window, and not `HeadlessChrome`. That is all that changes: its debugging port is open on the sandbox's loopback, which is how its frames reach the window and what you type reaches the page, and a site may tell from other things that it is no browser with a window.

An agent finds the sandbox's MCP servers in its own **MCP registry**: the file its maker has it read, in the shape its maker gave it. The workstation's supervisor writes them into every known agent's registry at each start, so that whichever agent you start finds them:

| Agent | Its registry | A server there |
|---|---|---|
| Claude Code | `~/.claude.json` | JSON, `mcpServers.<name>`, with `"type": "stdio"` |
| Codex | `~/.codex/config.toml` | TOML, `[mcp_servers.<name>]`, with `tool_timeout_sec` |
| Grok | `~/.grok/config.toml` | TOML, `[mcp_servers.<name>]` |
| OpenCode | `/tmp/zeolite/opencode.json`, which `OPENCODE_CONFIG` names in the workstation's environment | JSON, `mcp.<name>`, a `local` server with one command line |
| Kimi Code | `~/.kimi-code/mcp.json` | JSON, `mcpServers.<name>` |
| Pi | `~/.omp/agent/mcp.json` | JSON, `mcpServers.<name>` |
| Antigravity | `~/.gemini/config/mcp_config.json` | JSON, `mcpServers.<name>` |

- **Every entry is the same command**: the supervisor's bridge with the server's name, `supervisor mcp-bridge <name>`. The bridge looks the server up in what the supervisor was told, starts it, and logs the calls crossing it; the dashboard lists them behind MCP Calls, in the Header, whichever agent made them.
- **What else a registry holds is yours and the agent's.** An entry is the app's exactly when its command is the supervisor: those are replaced at each start, and no list is kept of what was written. In a JSON registry every other key and server keeps its place and its text. In a TOML registry every other line stays, its comments with it: the app's tables are cut out and put again at the end, and the file is written only if it then reads, as TOML, as it did but for them.
- **A registry that does not read is left as it is**, and the supervisor says so in the workstation's log: a JSON file with comments in it, a TOML file with a mistake. So is one where a server of your own has a name of the sandbox's. OpenCode's own config may hold comments, which is why the app writes a file of its own for it and leaves yours alone.
- **Every sandbox has one server**, `sandbox`, by which an agent raises its hand, so every Home has the six registries that are under it after its first start; a pack adds its own server, and Host Tools add `host`.
