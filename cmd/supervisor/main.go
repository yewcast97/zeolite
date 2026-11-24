// Command supervisor is PID 1 of a sandbox's workstation.
//
//	supervisor run                       starts the packs' daemons and reports on them
//	supervisor mcp-bridge <name>         runs the sandbox's MCP server of that name, logging what crosses it
//	supervisor shell <session-id>        becomes a login shell, recorded as a Terminal Session
//	supervisor hangup <session-id>       ends a Terminal Session
//	supervisor link                      carries what the workstation asks of the app, which runs it: the sandbox's Link
//	supervisor host-tools                is the MCP server by which an agent runs Host Tools
//	supervisor state                     is the MCP server by which an agent tells its user of its state
package main

import (
	"fmt"
	"os"

	"zeolite/internal/sandbox"
)

func main() {
	args := os.Args[1:]
	var err error
	switch {
	case len(args) == 1 && args[0] == "run":
		err = run()
	case len(args) == 2 && args[0] == bridgeCommand:
		err = mcpBridge(args[1])
	case len(args) == 2 && args[0] == "shell":
		err = shell(args[1])
	case len(args) == 2 && args[0] == "hangup":
		err = hangup(args[1])
	case len(args) == 1 && args[0] == "link":
		err = link(linkSocket, os.Stdin, os.Stdout)
	case len(args) == 1 && args[0] == "host-tools":
		// What the Wall was given, an agent is told: where the supervisor
		// was told of no Host Tools, the app refuses every call.
		cfg, _ := readConfig(configPath)
		var notice sandbox.HostToolsNotice
		if cfg.HostTools != nil {
			notice = *cfg.HostTools
		}
		err = hostToolServer(linkSocket, notice).serve(os.Stdin, os.Stdout)
	case len(args) == 1 && args[0] == "state":
		err = stateServer(linkSocket, ownSession).serve(os.Stdin, os.Stdout)
	default:
		fmt.Fprintln(os.Stderr, "usage: supervisor run | mcp-bridge <name> | shell <session-id> | hangup <session-id> | link | host-tools | state")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "supervisor:", err)
		os.Exit(1)
	}
}
