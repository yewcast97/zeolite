package acpbridge

import "encoding/json"

// What crosses from the agent to the client: the agent's answers, each
// rebuilt by the method it answers; what a session did; and the one
// thing an agent asks of its client, a permission.

// resultToClient rebuilds the agent's answer to a request of the
// client's, by that request's method, and says whether it was such an
// answer at all. An answer that holds nothing may be written as null.
func (b *bridge) resultToClient(method string, v any) (object, bool) {
	r, isObject := v.(object)
	out := object{}
	switch method {
	case methodInitialize:
		// An agent that speaks no version the bridge does answers with
		// another: the client is not told that one, by which it would
		// go on to speak what the bridge does not pass.
		if version, ok := whole(r, "protocolVersion"); !ok || version != protocolVersion {
			return nil, false
		}
		out["protocolVersion"] = protocolVersion
		capabilities, _ := member(r, "agentCapabilities")
		out["agentCapabilities"] = agentCapabilities(capabilities)
		methods, _ := items(r, "authMethods")
		out["authMethods"] = authMethods(methods)
		if info, ok := implementation(r, "agentInfo"); ok {
			out["agentInfo"] = info
		}
		return out, true
	case methodNewSession:
		sessionState(out, r)
		return out, needText(out, r, "sessionId")
	case methodLoadSession, methodResumeSession:
		sessionState(out, r)
		return out, isObject || v == nil
	case methodListSessions:
		list, ok := items(r, "sessions")
		sessions := []any{}
		for _, v := range list {
			if session, ok := b.sessionInfo(v); ok {
				sessions = append(sessions, session)
			}
		}
		out["sessions"] = sessions
		keepText(out, r, "nextCursor")
		return out, ok
	case methodSetConfigOption:
		list, ok := items(r, "configOptions")
		out["configOptions"] = configOptions(list)
		return out, ok
	case methodPrompt:
		return out, needText(out, r, "stopReason")
	case methodAuthenticate, methodLogout, methodDeleteSession, methodCloseSession, methodSetMode:
		return out, isObject || v == nil
	}
	return nil, false
}

// agentCapabilities rebuilds what an agent says it can do. Whatever it
// says of MCP servers, the client is told that it takes none over the
// network: the bridge passes it none of the client's at all.
func agentCapabilities(c object) object {
	out := object{"mcpCapabilities": object{"http": false, "sse": false}}
	keepFlags(out, c, "loadSession")
	if prompt, ok := member(c, "promptCapabilities"); ok {
		rebuilt := object{}
		keepFlags(rebuilt, prompt, "image", "audio", "embeddedContext")
		out["promptCapabilities"] = rebuilt
	}
	if session, ok := member(c, "sessionCapabilities"); ok {
		rebuilt := object{}
		keepMarks(rebuilt, session, "list", "delete", "additionalDirectories", "resume", "close")
		if system, ok := member(session, "systemPrompt"); ok {
			mode := object{}
			if modes, ok := member(system, "mode"); ok {
				keepMarks(mode, modes, "override")
			}
			rebuilt["systemPrompt"] = object{"mode": mode}
		}
		out["sessionCapabilities"] = rebuilt
	}
	if auth, ok := member(c, "auth"); ok {
		rebuilt := object{}
		keepMarks(rebuilt, auth, "logout")
		out["auth"] = rebuilt
	}
	return out
}

// authMethods rebuilds the ways to log in that an agent offers: those
// the agent does itself. One of any other type is left out, and so the
// one that has the client run a command in a terminal of its own.
// Logging in is done where the agent is.
func authMethods(list []any) []any {
	methods := []any{}
	for _, v := range list {
		in, _ := v.(object)
		if kind, typed := in["type"]; typed && kind != "agent" {
			continue
		}
		out := object{}
		if needText(out, in, "id", "name") {
			keepText(out, in, "description")
			methods = append(methods, out)
		}
	}
	return methods
}

// sessionState rebuilds what an answer says of a session's modes and
// its options.
func sessionState(out, r object) {
	if modes, ok := member(r, "modes"); ok {
		rebuilt := object{}
		available, listed := items(modes, "availableModes")
		list := []any{}
		for _, v := range available {
			in, _ := v.(object)
			mode := object{}
			if needText(mode, in, "id", "name") {
				keepText(mode, in, "description")
				list = append(list, mode)
			}
		}
		rebuilt["availableModes"] = list
		if needText(rebuilt, modes, "currentModeId") && listed {
			out["modes"] = rebuilt
		}
	}
	if options, ok := items(r, "configOptions"); ok {
		out["configOptions"] = configOptions(options)
	}
}

// configOptions rebuilds a session's options: those that are chosen
// among several values. One of any other type is left out: a client
// has to ask for it, and the bridge asks for none.
func configOptions(list []any) []any {
	options := []any{}
	for _, v := range list {
		in, _ := v.(object)
		if kind, _ := text(in, "type"); kind != "select" {
			continue
		}
		out := object{"type": "select"}
		values, listed := items(in, "options")
		if !needText(out, in, "id", "name", "currentValue") || !listed {
			continue
		}
		keepText(out, in, "description", "category")
		choices := []any{}
		for _, v := range values {
			in, _ := v.(object)
			if grouped, isGroup := items(in, "options"); isGroup {
				group := object{"options": selectValues(grouped)}
				if needText(group, in, "group", "name") {
					choices = append(choices, group)
				}
				continue
			}
			choices = append(choices, selectValues([]any{in})...)
		}
		out["options"] = choices
		options = append(options, out)
	}
	return options
}

// selectValues rebuilds the values an option is chosen among.
func selectValues(list []any) []any {
	values := []any{}
	for _, v := range list {
		in, _ := v.(object)
		out := object{}
		if needText(out, in, "value", "name") {
			keepText(out, in, "description")
			values = append(values, out)
		}
	}
	return values
}

// sessionInfo rebuilds one session of an agent's list. Its folder is
// written as this Mac has it; a session whose folder is outside the
// Shared Folder is left out of the list, and so is a folder beside it
// that is.
func (b *bridge) sessionInfo(v any) (object, bool) {
	in, _ := v.(object)
	out := object{}
	cwd, _ := text(in, "cwd")
	mapped, inside := b.folder.toMac(cwd)
	if !inside || !needText(out, in, "sessionId") {
		return nil, false
	}
	out["cwd"] = mapped
	if list, ok := items(in, "additionalDirectories"); ok {
		beside := []any{}
		for _, v := range list {
			dir, _ := v.(string)
			if mapped, inside := b.folder.toMac(dir); inside {
				beside = append(beside, mapped)
			}
		}
		out["additionalDirectories"] = beside
	}
	keepText(out, in, "title", "updatedAt")
	return out, true
}

// sessionUpdate rebuilds what a session did, by its kind. A kind that
// a client has to ask for is dropped, since the bridge asked for none,
// and so is one the protocol's first version does not have.
func (b *bridge) sessionUpdate(p object) (object, bool) {
	in, _ := member(p, "update")
	kind, _ := text(in, "sessionUpdate")
	update := object{"sessionUpdate": kind}
	ok := true
	switch kind {
	case "user_message_chunk", "agent_message_chunk", "agent_thought_chunk":
		update["content"], ok = contentBlock(in["content"], b.folder.uriToClient)
		keepText(update, in, "messageId")
	case "tool_call":
		ok = b.toolCall(update, in) && needText(update, in, "title")
	case "tool_call_update":
		ok = b.toolCall(update, in)
	case "plan":
		list, listed := items(in, "entries")
		entries := []any{}
		for _, v := range list {
			in, _ := v.(object)
			entry := object{}
			if needText(entry, in, "content", "priority", "status") {
				entries = append(entries, entry)
			}
		}
		update["entries"], ok = entries, listed
	case "available_commands_update":
		list, listed := items(in, "availableCommands")
		commands := []any{}
		for _, v := range list {
			in, _ := v.(object)
			command := object{}
			if !needText(command, in, "name", "description") {
				continue
			}
			if input, ok := member(in, "input"); ok {
				hint := object{}
				if needText(hint, input, "hint") {
					command["input"] = hint
				}
			}
			commands = append(commands, command)
		}
		update["availableCommands"], ok = commands, listed
	case "current_mode_update":
		ok = needText(update, in, "currentModeId")
	case "config_option_update":
		list, listed := items(in, "configOptions")
		update["configOptions"], ok = configOptions(list), listed
	case "session_info_update":
		// A title that is null is one that is cleared.
		for _, name := range []string{"title", "updatedAt"} {
			if v, said := in[name]; said {
				if s, isText := v.(string); isText {
					update[name] = s
				} else if v == nil {
					update[name] = nil
				}
			}
		}
	case "usage_update":
		used, usedOK := whole(in, "used")
		size, sizeOK := whole(in, "size")
		update["used"], update["size"], ok = used, size, usedOK && sizeOK
		if cost, said := member(in, "cost"); said {
			amount, isNumber := cost["amount"].(json.Number)
			rebuilt := object{"amount": amount}
			if needText(rebuilt, cost, "currency") && isNumber {
				update["cost"] = rebuilt
			}
		}
	default:
		ok = false
	}
	out := object{"update": update}
	return out, needText(out, p, "sessionId") && ok
}

// toolCall rebuilds what an agent says of a call of one of its tools.
// A file the call is at, or changes, is named by this Mac's path where
// it is in the Shared Folder, and left out where it is not: the client
// is given no path that is not in that folder. What the tool was given
// and gave is the agent's own account, written anew.
func (b *bridge) toolCall(out, in object) bool {
	keepText(out, in, "title", "name", "kind", "status")
	if list, ok := items(in, "content"); ok {
		content := []any{}
		for _, v := range list {
			if part, ok := b.toolCallContent(v); ok {
				content = append(content, part)
			}
		}
		out["content"] = content
	}
	if list, ok := items(in, "locations"); ok {
		locations := []any{}
		for _, v := range list {
			in, _ := v.(object)
			at, _ := text(in, "path")
			if mapped, inside := b.folder.toMac(at); inside {
				location := object{"path": mapped}
				keepWhole(location, in, "line")
				locations = append(locations, location)
			}
		}
		out["locations"] = locations
	}
	for _, name := range []string{"rawInput", "rawOutput"} {
		if v, said := in[name]; said && v != nil {
			out[name] = plain(v)
		}
	}
	return needText(out, in, "toolCallId")
}

// toolCallContent rebuilds one part of what a tool call shows: content,
// or the change of a file in the Shared Folder. A terminal is the
// client's own, which no agent was given here, and a session beside
// this one is a subagent's, which the bridge did not ask for.
func (b *bridge) toolCallContent(v any) (object, bool) {
	in, _ := v.(object)
	switch kind, _ := text(in, "type"); kind {
	case "content":
		block, ok := contentBlock(in["content"], b.folder.uriToClient)
		return object{"type": kind, "content": block}, ok
	case "diff":
		at, _ := text(in, "path")
		mapped, inside := b.folder.toMac(at)
		out := object{"type": kind, "path": mapped}
		keepText(out, in, "oldText")
		return out, inside && needText(out, in, "newText")
	}
	return nil, false
}

// permissionRequest rebuilds the one thing an agent asks of its client:
// whether its user allows a call of a tool, by one of the options it
// names.
func (b *bridge) permissionRequest(p object) (object, bool) {
	out := object{}
	call, _ := member(p, "toolCall")
	rebuilt := object{}
	list, listed := items(p, "options")
	options := []any{}
	for _, v := range list {
		in, _ := v.(object)
		option := object{}
		if needText(option, in, "optionId", "name", "kind") {
			options = append(options, option)
		}
	}
	out["toolCall"], out["options"] = rebuilt, options
	return out, needText(out, p, "sessionId") && b.toolCall(rebuilt, call) && listed
}

// errorToClient rebuilds an error the agent answered the client with:
// its code, its sentence, and what it says besides, written anew.
func errorToClient(failed object) refusal {
	r := errorOf(failed)
	if data, said := failed["data"]; said && data != nil {
		r.data = plain(data)
	}
	return r
}

// errorCode reads an error's code: an integer of 32 bits.
func errorCode(failed object) (int, bool) {
	n, ok := failed["code"].(json.Number)
	if !ok {
		return 0, false
	}
	code, err := n.Int64()
	return int(code), err == nil && code == int64(int32(code))
}
