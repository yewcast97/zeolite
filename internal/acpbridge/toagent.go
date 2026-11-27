package acpbridge

import "fmt"

// What crosses from the client to the agent: the client's requests,
// each rebuilt by its method, and the client's answer to the one thing
// an agent asks it.

// clientCapabilities is what the bridge says of its client, whatever
// the client says of itself: it reads and writes no file for an agent
// and runs no command for one. What it leaves unsaid, the protocol
// reads as not offered: a login in a terminal, forms, subagents, and
// the kinds of update a client has to ask for.
func clientCapabilities() object {
	return object{
		"fs":       object{"readTextFile": false, "writeTextFile": false},
		"terminal": false,
	}
}

// protocolVersion is the version of the protocol the bridge speaks, and
// says the client speaks.
const protocolVersion = 1

// notAsTheProtocolHasIt refuses a request whose parameters are not the
// method's.
func notAsTheProtocolHasIt(method string) *refusal {
	return &refusal{code: codeInvalidParams, message: fmt.Sprintf("zeolite's ACP Bridge: the parameters of %s are not as version 1 of the Agent Client Protocol has them", method)}
}

// outsideTheFolder refuses a session whose folder is not in the
// sandbox's Shared Folder.
func (b *bridge) outsideTheFolder(p string) *refusal {
	return &refusal{code: codeInvalidParams, message: fmt.Sprintf("zeolite's ACP Bridge: an agent in this sandbox works in its Shared Folder, %s, and %s is not in it: open a folder there, by that path", b.folder.mac, p)}
}

// requestToAgent rebuilds the parameters of a request of the client's,
// or says why the bridge answers it itself: it is no method the bridge
// passes, its parameters are not the method's, or it names a folder
// outside the Shared Folder.
func (b *bridge) requestToAgent(method string, p object) (object, *refusal) {
	out := object{}
	ok := true
	switch method {
	case methodInitialize:
		out["protocolVersion"] = protocolVersion
		out["clientCapabilities"] = clientCapabilities()
		if info, ok := implementation(p, "clientInfo"); ok {
			out["clientInfo"] = info
		}
	case methodAuthenticate:
		ok = needText(out, p, "methodId")
	case methodLogout:
	case methodNewSession, methodLoadSession, methodResumeSession:
		if method != methodNewSession && !needText(out, p, "sessionId") {
			return nil, notAsTheProtocolHasIt(method)
		}
		if refused := b.sessionFolders(out, method, p); refused != nil {
			return nil, refused
		}
	case methodListSessions:
		keepText(out, p, "cursor")
		if cwd, named := text(p, "cwd"); named {
			mapped, inside := b.folder.toWorkstation(cwd)
			if !inside {
				return nil, b.outsideTheFolder(cwd)
			}
			out["cwd"] = mapped
		}
	case methodDeleteSession, methodCloseSession:
		ok = needText(out, p, "sessionId")
	case methodSetMode:
		ok = needText(out, p, "sessionId", "modeId")
	case methodSetConfigOption:
		// The value of an option that is chosen among several. One
		// that is switched on and off is a client's to ask for, and the
		// bridge asks for none.
		_, typed := p["type"]
		ok = needText(out, p, "sessionId", "configId", "value") && !typed
	case methodPrompt:
		prompt, listed := items(p, "prompt")
		blocks, taken := b.blocksToAgent(prompt)
		out["prompt"] = blocks
		ok = needText(out, p, "sessionId") && listed && taken
	default:
		return nil, &refusal{code: codeMethodNotFound, message: fmt.Sprintf("zeolite's ACP Bridge passes no %.64q: it is no method of version 1 of the Agent Client Protocol", method)}
	}
	if !ok {
		return nil, notAsTheProtocolHasIt(method)
	}
	return out, nil
}

// sessionFolders rebuilds what a request that makes or takes up a
// session says of where the agent works: its folder and the folders
// beside it, each of which is in the Shared Folder and is written as
// the workstation has it, or the request is refused. The client's MCP
// servers are replaced by none: they are programs and addresses of
// this Mac's. What the client adds to the agent's instructions is
// passed as a prompt is.
func (b *bridge) sessionFolders(out object, method string, p object) *refusal {
	cwd, ok := text(p, "cwd")
	if !ok {
		return notAsTheProtocolHasIt(method)
	}
	mapped, inside := b.folder.toWorkstation(cwd)
	if !inside {
		return b.outsideTheFolder(cwd)
	}
	out["cwd"] = mapped
	if _, named := p["additionalDirectories"]; named {
		list, ok := items(p, "additionalDirectories")
		if !ok {
			return notAsTheProtocolHasIt(method)
		}
		beside := []any{}
		for _, v := range list {
			dir, ok := v.(string)
			if !ok {
				return notAsTheProtocolHasIt(method)
			}
			mapped, inside := b.folder.toWorkstation(dir)
			if !inside {
				return b.outsideTheFolder(dir)
			}
			beside = append(beside, mapped)
		}
		out["additionalDirectories"] = beside
	}
	out["mcpServers"] = []any{}
	if prompt, named := member(p, "systemPrompt"); named {
		system := object{}
		content, listed := items(prompt, "content")
		blocks, taken := b.blocksToAgent(content)
		if !needText(system, prompt, "mode") || !listed || !taken {
			return notAsTheProtocolHasIt(method)
		}
		system["content"] = blocks
		out["systemPrompt"] = system
	}
	return nil
}

// blocksToAgent rebuilds what a client's prompt holds, and says whether
// all of it was taken: a prompt is passed whole or not at all.
func (b *bridge) blocksToAgent(list []any) ([]any, bool) {
	blocks := []any{}
	for _, v := range list {
		block, ok := contentBlock(v, b.folder.uriToAgent)
		if !ok {
			return nil, false
		}
		blocks = append(blocks, block)
	}
	return blocks, true
}

// permissionOutcome rebuilds the client's answer to an agent that asked
// for a permission: that its user chose one of the options, or that
// the question was withdrawn.
func permissionOutcome(v any) (object, bool) {
	answer, _ := v.(object)
	outcome, _ := member(answer, "outcome")
	switch kind, _ := text(outcome, "outcome"); kind {
	case "cancelled":
		return object{"outcome": object{"outcome": kind}}, true
	case "selected":
		out := object{"outcome": kind}
		return object{"outcome": out}, needText(out, outcome, "optionId")
	}
	return nil, false
}

// errorOf rebuilds an error the client answered the agent with: its
// code and its sentence. What else it holds is the client's own.
func errorOf(failed object) refusal {
	r := refusal{code: codeInternalError}
	if code, ok := errorCode(failed); ok {
		r.code = code
	}
	r.message, _ = text(failed, "message")
	return r
}

// notOffered is the bridge's answer to what an agent asks of its
// client besides a permission. It is said as an agent reads it: what
// the client does not do, and what the agent can do instead.
func notOffered(method string) refusal {
	r := refusal{code: codeMethodNotFound}
	switch method {
	case "fs/read_text_file", "fs/write_text_file":
		r.message = "this client reads and writes no files for an agent: read and write them with your own tools"
	case "terminal/create", "terminal/output", "terminal/release", "terminal/wait_for_exit", "terminal/kill":
		r.message = "this client runs no commands for an agent: run them with your own tools"
	case "elicitation/create":
		r.message = "this client shows no forms and opens no addresses for an agent: ask your user in a message"
	default:
		r.message = fmt.Sprintf("this client does not answer %.64q", method)
	}
	return r
}
