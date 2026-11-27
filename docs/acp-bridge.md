# The ACP Bridge

An agent in a sandbox is reached by typing in a Terminal Session. A sandbox that is given an **ACP Bridge**, in its Setup under its Shared Folder, is reached from an editor on this Mac too: an ACP client, such as Zed, works with an agent that runs in the sandbox's workstation, over the [Agent Client Protocol](https://agentclientprotocol.com).

The protocol has a client start its agent as a program and speak with it on the program's standard streams. So the app gives the client a command to start in an agent's place:

```
<the app's executable> acp-bridge <sandbox ID> <agent executable> [arguments...]
```

The Setup copies it: switched on, the ACP Bridge shows a glyph for each agent that speaks the protocol, and a click on one copies the command for that agent. In Zed it goes under `agent_servers` in the settings, as a custom agent:

```json
{
  "agent_servers": {
    "OpenCode in Research": {
      "type": "custom",
      "command": "/Applications/zeolite.app/Contents/MacOS/zeolite",
      "args": ["acp-bridge", "s3f9a1c20d7", "opencode"],
      "env": {}
    }
  }
}
```

Then open a folder of the sandbox's Shared Folder in the editor, by its path as the folder's page shows it, and start a thread with that agent.

- **The command is the app's own executable.** Started so, it is the bridge and nothing of the window: it reads the saved sandboxes to find the one named, writes nothing there, and needs the app running only because the sandbox does. The sandbox is named by its ID, which a new name does not change, and the agent by its Agent Executable. What follows the agent is added to the agent's own command, each word as it was given: no shell reads it.
- **The app starts the agent, in this one case.** It asks Docker about the workstation as it runs: the sandbox was started with an ACP Bridge where its workstation has the label that says so, whatever a saved config says, and its Shared Folder is where the workstation mounts it from. A sandbox that does not run, or was started without an ACP Bridge, starts no agent. Switching the ACP Bridge in the Setup starts a running sandbox again.
- **The agent is started in the workstation**, by the supervisor, in the Shared Folder, as the agent's user: through a login shell, so that it is found where a Terminal Session finds what you installed, and has what your `~/.bashrc` sets. The shell is given one text, which is fixed; the agent's program and its arguments are words after it. It has the workstation's environment and none of the editor's. Its streams cross on the Docker API, as a sandbox's Link does: nothing listens on this Mac and no port of the sandbox is published.
- **Only the seven known agents are started**, each by the command its maker documents, written once beside its name (`internal/sandbox/agent.go`):

  | Agent | Named as | Started by |
  |---|---|---|
  | Claude Code | `claude` | `claude-agent-acp` |
  | Codex | `codex` | `codex-acp` |
  | Grok | `grok` | `grok agent stdio` |
  | OpenCode | `opencode` | `opencode acp` |
  | Kimi Code | `kimi` | `kimi acp` |
  | Pi | `omp` | `omp acp` |
  | Antigravity | `agy` | `agy acp` |

  Claude Code and Codex speak the protocol by a program of another name, which you install in the workstation besides the agent: `npm install --global @agentclientprotocol/claude-agent-acp`, and `@agentclientprotocol/codex-acp`. A workstation that has no such program is said so, on the command's standard error.
- **Nothing crosses as it was written.** Every message, either way, is read with each member under its exact name, rebuilt from the members the protocol's first version gives it, and written anew (`internal/acpbridge`): what the bridge does not know by name does not cross. An object that has a name twice is refused, since the next reader might take the other one. `_meta`, the member by which either side may attach anything, crosses nowhere. The shapes are those of the protocol's schema 1.35.0, of 23 November 2026.
- **The agent is told that its client offers nothing**, whatever the editor says of itself: no reading or writing of files, no terminal, no login in a terminal, no forms, no subagents, and none of the kinds of update a client has to ask for. What an agent asks of its client all the same, `fs/read_text_file`, `terminal/create`, `elicitation/create` or anything else, the bridge answers itself, with an error said as an agent reads one; the editor never sees it. The one thing an agent asks that crosses is a permission, and the editor's answer to it.
- **The agent works in the Shared Folder.** A session whose folder, or a folder beside it, is outside the Shared Folder is refused, to the editor, with the folder's path; the agent never hears of it. A path in the Shared Folder is written as each side has it: `/Users/Shared/zeolite/shared.noindex/<name>/…` to the editor, `/workspace/shared/…` to the agent. So a file the agent edits opens in the editor, and a file you mention is the agent's to read.
- **No other path crosses.** A file you mention that is outside the Shared Folder reaches the agent by its name alone, with what the editor sent of its text, and a URI of the editor's own kind by its kind alone, so that no path with your name reaches the workstation. Of what an agent reports, a place or a change of a file outside the Shared Folder is left out, and a resource is passed only where it is a file in the Shared Folder or a web address. Paths are mapped by their text: cleaned, so that no `..` leads out, and compared by whole names. The bridge reads no file and follows no link.
- **The editor's MCP servers are not passed.** They are programs and addresses of this Mac's. The agent has the sandbox's own, which the supervisor wrote into its MCP Registry: OpenCode found them there when it was started so, and so did Claude Code, whose own program for the protocol reads Claude Code's registry.
- **A login that runs a command on this Mac is not offered.** An agent may offer a way to log in that has its client run a program in a terminal of its own; the bridge leaves it out of what the editor is told. Log in to an agent in a Terminal Session.
- **When the editor leaves, the agent ends.** The agent leads a session of its own in the workstation. When its client closes, when the bridge is killed, or when the agent's own program exits, every process of that session is hung up, and killed if it stays two seconds; when the sandbox stops, the bridge ends by itself. Zed starts the command once for each window that opens a folder, and closes it with that window.
- **Bounds.** A message is 16 MiB at most: a longer one from the editor is dropped, and one from the workstation ends the bridge. An agent may keep 64 requests waiting for the editor's answer; more ends the bridge.

What you accept by giving a sandbox one:

- **The editor acts on what an agent says.** The words of an agent's message, the title of a tool call and what a tool was given and gave cross as the agent wrote them: they are what you asked it for. If the editor loads a picture or opens a link that those words name, that connection is made from this Mac, by none of the sandbox's Allowed Interfaces, and says to whoever is asked that somebody read the message. The bridge does not prevent it; it is the editor's to ask first. Zed 1.30.2 does not ask: it fetches a picture that an agent's reply names as it shows the reply.
- **The editor opens a folder agents wrote.** A Shared Folder holds what its sandboxes' agents put there, and an editor reads more of a folder than you look at: its own settings and tasks for the project, a checkout's `.git/config`, what a language server is told to run. Treat what the editor offers to trust there as you would in a download. Zed 1.30.2 marks a Shared Folder as not trusted and still offers the tasks its `.zed/tasks.json` names, and runs one when you pick it, without asking: on this Mac, outside every sandbox, as you. A link an agent made in the folder leads anywhere on this Mac, and the editor follows it; the bridge maps paths by their text and cannot know.
- **The agent learns the name of a file you mention**, though it is outside the Shared Folder, and whatever of its text the editor sent along. It learns the editor's name and version, which the editor says of itself.
- **An agent's own account of a tool call names paths of the workstation.** What a tool was given and gave, and the words of a title, are not paths to the protocol, and cross as written: `/workspace/shared/…` there is the Shared Folder.
- **Claude Code and Codex need their program installed besides**, and an agent that is not logged in answers that it is not: both are done in a Terminal Session.
- **No Pane shows an agent started so.** It is in no Terminal Session: it has no glyph in the Header, the Card does not say that it runs, and its `raise_hand` is answered that there is no session to call you to. What it does shows in the editor.
- **A process that makes a session of its own outlives the agent**, as one does that was started in a Terminal Session and left it. It stays in the workstation, behind the gate, until the sandbox stops.
- **Only the protocol's first version**, and of it what a client that asks for nothing is sent: an agent's plan, its messages, its tool calls, its modes and the options chosen among several values. A second version is in preview; the bridge answers 1. What a later schema adds does not cross until the bridge is taught it.
- **The agents' commands are as their makers documented them on 27 November 2026**, and a maker may change one: they are in one place, beside the agents' names.
