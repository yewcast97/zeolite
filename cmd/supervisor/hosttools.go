package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"zeolite/internal/sandbox"
)

// A sandbox's Host Tools are reached from its workstation in two steps.
// The agent's MCP client starts `supervisor host-tools`, the MCP server
// macos, whose one tool, run, hands each call to the sandbox's Link,
// which carries it to the app, and waits for its answer.
//
// Nothing here runs a program: a request names one, and the app runs it
// on the Mac, under the Host Tool Wall.

// askHostTool hands one request to the Link and waits for its answer.
// Closing stop withdraws it.
func askHostTool(socket string, r sandbox.HostToolRequest, stop <-chan struct{}) (sandbox.HostToolResult, error) {
	answer, err := ask(socket, sandbox.LinkRequest{HostTool: &r}, stop)
	switch {
	case err != nil:
		return sandbox.HostToolResult{}, err
	case answer.Refused != "":
		return sandbox.HostToolResult{Refused: answer.Refused}, nil
	case answer.HostTool == nil:
		return sandbox.HostToolResult{}, errors.New("the call was answered with no run's result: try again")
	}
	return *answer.HostTool, nil
}

// What an agent is told of the GPU and of the Mac's name, by whether the
// sandbox's Host Tool Wall was given each.
const (
	hostToolsWithGPU = "The Mac's GPU is given: Metal finds its device, and the hardware's video encoders are there (ffmpeg's `h264_videotoolbox` and `hevc_videotoolbox`). " +
		"A program has all of it when `run` starts it, when bash or zsh starts it, however deep in a run, and when it is Homebrew's and started by its name. " +
		"A program named by its path that another of the Mac's own programs starts finds no hardware encoder, and ends where it keeps what Metal compiled: " +
		"`env`, `xargs`, `find`, `sh` inside a script, a `#!/bin/sh` script, a Makefile's lines, Python's `shell=True`. " +
		"Start it through bash there: `bash -c './prog'`, `SHELL := /bin/bash` in a Makefile, `executable=\"/bin/bash\"` in Python. " +
		"`sh` as the program of `argv` is started as bash: its `echo -n` prints no `-n`."
	hostToolsWithoutGPU = "The Mac's GPU is not given: Metal finds no device, and the hardware's video encoders are not there. " +
		"Render and encode on the CPU, or ask your user for the GPU."
	hostToolsWithName    = "The Mac's name is given: `uname` answers."
	hostToolsWithoutName = "The Mac's name is not given: `uname` fails, and with it what asks the platform's name, as Python's `platform` module and numpy do, " +
		"and `swiftc` unless it is told (`-target arm64-apple-macos13.0`). Ask your user for it if you need it."
)

// hostToolWords are what an agent is told of Host Tools, as they are
// for this sandbox: once as the server's instructions, and once more,
// whole, as the description of its one tool. They are said as the agent
// uses the tool: a program runs on a Mac, under what bounds, and what
// it sees there. Whose Mac it is, and what else it does, they leave out.
func hostToolWords(notice sandbox.HostToolsNotice) string {
	gpu, name := hostToolsWithoutGPU, hostToolsWithoutName
	if notice.GPU {
		gpu = hostToolsWithGPU
	}
	if notice.MacName {
		name = hostToolsWithName
	}
	shared := sandbox.SharedFolderPath
	return strings.Join([]string{
		"Run a program on a Mac, in a restricted macOS environment, and return its exit status and the start of what it printed. " +
			"Use it for what needs macOS: its compilers, an application installed there, its GPU where it is given.",
		"`argv` is the program and its arguments, as a list. No shell reads it: to use one, run it (`[\"bash\", \"-c\", \"...\"]`). " +
			"`dir` is the folder the program starts in, named from " + shared + "; `env` adds variables; `seconds` is how long the run may take: " +
			fmt.Sprintf("%d where you do not say, %d at most.", sandbox.HostToolDefaultSeconds, sandbox.HostToolMaxSeconds),
		fmt.Sprintf("It answers with the start of what the program printed, %d KiB of each stream. Send long output to a file.", sandbox.HostToolMaxOutput>>10),
		"Of your files the program sees " + shared + " and nothing else. The Mac has that folder under another path, which is where the program starts: " +
			"name files from there, put there what the program needs, and find there what it made. It has a home and a temporary folder of its own, which you do not see.",
		"It reads what is installed on the Mac: Homebrew under /opt/homebrew, the system's tools and its compilers, " + hostToolApplications(notice.Applications) + " " +
			"It has no network, no other file of the Mac and no window: have a program write pictures, and look at those.",
		"What is not installed on the Mac, fetch yourself, built for macOS, into " + shared + ": a program runs from there. It cannot fetch anything itself.",
		gpu,
		name,
	}, "\n\n")
}

// hostToolApplications ends the sentence that says what a program reads
// of what is installed: of /Applications, the folders the sandbox is
// given, by their paths, and that there is nothing else there for it.
func hostToolApplications(names []string) string {
	if len(names) == 0 {
		return "and nothing of /Applications: ask your user for an application you need."
	}
	paths := make([]string, len(names))
	for i, name := range names {
		paths[i] = "/Applications/" + name
	}
	return "and of /Applications these and nothing else: " + strings.Join(paths, ", ") + ". Ask your user for another."
}

// hostToolRunSchema is the tool's arguments, as JSON Schema.
var hostToolRunSchema = json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "argv": {"type": "array", "items": {"type": "string"}, "minItems": 1, "description": "The program and its arguments. The program is a name the Mac's PATH finds, or a path. No shell reads this: to use one, run it, as in [\"bash\", \"-c\", \"...\"]."},
    "dir": {"type": "string", "description": "The folder the program starts in, named from /workspace/shared, such as build/out. Empty for /workspace/shared itself."},
    "env": {"type": "object", "additionalProperties": {"type": "string"}, "description": "Variables added to the program's environment."},
    "seconds": {"type": "integer", "minimum": 1, "maximum": %d, "description": "How long the run may take. %d where it is not given."}
  },
  "required": ["argv"]
}`, sandbox.HostToolMaxSeconds, sandbox.HostToolDefaultSeconds))

// hostToolServer is the MCP server macos: one tool, run, which hands a
// call to the Link at socket. notice is what the sandbox's Host Tool
// Wall was given, which an agent is told.
func hostToolServer(socket string, notice sandbox.HostToolsNotice) mcpServer {
	words := hostToolWords(notice)
	return mcpServer{
		name:         sandbox.HostToolServer,
		instructions: "The tool `" + sandbox.HostToolRun + "` of this server: " + words,
		tools: []mcpTool{{
			name:        sandbox.HostToolRun,
			description: words,
			schema:      hostToolRunSchema,
			call: func(arguments json.RawMessage, stop <-chan struct{}) (string, bool, *rpcError) {
				return callHostTool(socket, arguments, stop)
			},
		}},
	}
}

// callHostTool answers one call of run.
func callHostTool(socket string, arguments json.RawMessage, stop <-chan struct{}) (string, bool, *rpcError) {
	var p struct {
		Argv    []string          `json:"argv"`
		Dir     string            `json:"dir"`
		Env     map[string]string `json:"env"`
		Seconds int               `json:"seconds"`
	}
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &p); err != nil {
			return "", false, &rpcError{Code: -32602, Message: "the call's arguments: " + err.Error()}
		}
	}
	request := sandbox.HostToolRequest{Argv: p.Argv, Dir: p.Dir, Env: p.Env, Seconds: p.Seconds}
	if err := request.Check(); err != nil {
		return "Not run: " + err.Error(), true, nil
	}
	result, err := askHostTool(socket, request, stop)
	if err != nil {
		return "Not run: " + err.Error(), true, nil
	}
	return describeHostToolResult(request, result), result.Refused != "" || result.TimedOut, nil
}

// describeHostToolResult is a run's answer as an agent reads it.
func describeHostToolResult(request sandbox.HostToolRequest, result sandbox.HostToolResult) string {
	if result.Refused != "" {
		return "Not run: " + result.Refused
	}
	var b strings.Builder
	switch {
	case result.TimedOut:
		seconds := request.Seconds
		if seconds == 0 {
			seconds = sandbox.HostToolDefaultSeconds
		}
		fmt.Fprintf(&b, "Ended after %d seconds, which was its time: give `seconds`, %d at most, for a run that takes longer.\n", seconds, sandbox.HostToolMaxSeconds)
	case result.ExitCode < 0:
		b.WriteString("Ended by a signal.\n")
	default:
		fmt.Fprintf(&b, "Exit status %d.\n", result.ExitCode)
	}
	if result.Truncated {
		fmt.Fprintf(&b, "It printed more than is shown: the first %d KiB of each stream. Send long output to a file, and read it under %s.\n", sandbox.HostToolMaxOutput>>10, sandbox.SharedFolderPath)
	}
	for _, stream := range []struct{ name, text string }{{"stdout", result.Stdout}, {"stderr", result.Stderr}} {
		if stream.text == "" {
			continue
		}
		fmt.Fprintf(&b, "--- %s ---\n%s", stream.name, stream.text)
		if !strings.HasSuffix(stream.text, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}
