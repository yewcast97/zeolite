package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"zeolite/internal/configstore"
	"zeolite/internal/sandbox"
)

// Started as a sandbox's ACP Bridge, the program is that before it is
// anything of the app: the first thing main does is to see whether it
// was started so, and then it does nothing else.
func TestTheACPBridgeIsTheFirstThingMainDoes(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var first ast.Stmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "main" && fn.Recv == nil {
			first = fn.Body.List[0]
		}
	}
	var printed bytes.Buffer
	printer.Fprint(&printed, fset, first)
	got := strings.Join(strings.Fields(printed.String()), " ")
	if want := `if len(os.Args) > 1 && os.Args[1] == acpBridgeCommand { os.Exit(acpBridge(os.Args[2:], os.Stdin, os.Stdout, os.Stderr)) }`; got != want {
		t.Errorf("main begins with\n%s\nwant\n%s", got, want)
	}
	if acpBridgeCommand != "acp-bridge" {
		t.Errorf("the ACP Bridge's command is %q", acpBridgeCommand)
	}

	// What the bridge is made of has nothing of the window: its file
	// imports no UI toolkit, and nothing that keeps a Shared Folder,
	// starts a Carrier or runs a Host Tool.
	bridge, err := parser.ParseFile(token.NewFileSet(), "acpbridge.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	var imports []string
	for _, imp := range bridge.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		imports = append(imports, p)
		if strings.Contains(p, "mygo") || strings.HasPrefix(p, "zeolite/") && !slices.Contains([]string{"zeolite/internal/acpbridge", "zeolite/internal/configstore", "zeolite/internal/orchestrator", "zeolite/internal/sandbox"}, p) {
			t.Errorf("acpbridge.go imports %s", p)
		}
	}
	if !slices.Contains(imports, "zeolite/internal/acpbridge") || !slices.Contains(imports, "zeolite/internal/orchestrator") {
		t.Fatalf("acpbridge.go imports %v: the test is not reading what it should", imports)
	}
}

// shellWords splits a line as a shell does, for what the app writes: a
// word as it is, or between single quotes.
func shellWords(t *testing.T, line string) []string {
	t.Helper()
	var words []string
	var word strings.Builder
	inWord, quoted := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quoted && c == '\'':
			quoted = false
		case quoted:
			word.WriteByte(c)
		case c == '\'':
			quoted, inWord = true, true
		case c == '\\' && i+1 < len(line):
			i++
			word.WriteByte(line[i])
			inWord = true
		case c == ' ':
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	if quoted {
		t.Fatalf("%q ends inside a quote", line)
	}
	if inWord {
		words = append(words, word.String())
	}
	return words
}

// The command line a client is given is read back as what it was made
// of, whatever the program's path holds.
func TestTheACPBridgesCommandLineIsReadBack(t *testing.T) {
	for _, executable := range []string{
		"/Applications/zeolite.app/Contents/MacOS/zeolite",
		"/Users/someone/My Apps/zeolite.app/Contents/MacOS/zeolite",
		"/tmp/it's here/zeolite",
		`/tmp/a "b" $c/zeolite`,
	} {
		for _, agent := range sandbox.KnownAgents {
			line := acpBridgeCommandLine(executable, "s-3f9a", agent.Executable)
			if strings.ContainsAny(line, "\n\t") {
				t.Errorf("the command line is not one line: %q", line)
			}
			words := shellWords(t, line)
			if len(words) != 4 || words[0] != executable || words[1] != acpBridgeCommand {
				t.Fatalf("%q is read as %q", line, words)
			}
			id, name, added, ok := acpBridgeArguments(words[2:])
			if !ok || id != "s-3f9a" || name != agent.Executable || len(added) != 0 {
				t.Errorf("%q is read as the sandbox %q and the agent %q with %q (%v)", line, id, name, added, ok)
			}
		}
	}
	if got := acpBridgeCommandLine("/Applications/zeolite.app/Contents/MacOS/zeolite", "s1", "claude"); got != "/Applications/zeolite.app/Contents/MacOS/zeolite acp-bridge s1 claude" {
		t.Errorf("a plain command line is written %q", got)
	}
	// What follows the agent is the agent's, as it was given.
	id, name, added, ok := acpBridgeArguments([]string{"s1", "opencode", "--model", "a b", ""})
	if !ok || id != "s1" || name != "opencode" || !slices.Equal(added, []string{"--model", "a b", ""}) {
		t.Errorf("read the sandbox %q, the agent %q and %q (%v)", id, name, added, ok)
	}
	for _, words := range [][]string{nil, {}, {"s1"}, {"", "claude"}, {"s1", ""}} {
		if _, _, _, ok := acpBridgeArguments(words); ok {
			t.Errorf("%q is read as a sandbox and an agent", words)
		}
	}
}

// The bridge says why it cannot start an agent, in the app's words, on
// its standard error, and writes nothing where the app's data is.
func TestTheACPBridgeSaysWhyItCannot(t *testing.T) {
	// No runtime answers here: the Colima profile is looked for under a
	// home that has none.
	t.Setenv("HOME", t.TempDir())
	run := func(words ...string) (status int, said string) {
		var out, errOut bytes.Buffer
		status = acpBridge(words, strings.NewReader(""), &out, &errOut)
		if out.Len() != 0 {
			t.Errorf("%q: the bridge wrote %q where the protocol is", words, out.String())
		}
		return status, errOut.String()
	}

	// A data directory that is not there stays so.
	missing := filepath.Join(t.TempDir(), "nothing")
	t.Setenv("ZEOLITE_DATA_DIR", missing)
	if status, said := run("s1", "claude"); status != 1 || !strings.Contains(said, `no sandbox has the ID "s1"`) {
		t.Errorf("with no saved sandbox: status %d, %q", status, said)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("the bridge made the data directory: %v", err)
	}

	dir := t.TempDir()
	t.Setenv("ZEOLITE_DATA_DIR", dir)
	cfg := sandbox.NewConfig("s1", "Research")
	cfg.SharedFolder, cfg.ACPBridge = "research", true
	if err := configstore.Open(dir).Save([]sandbox.Config{cfg}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadDir(dir)

	for _, words := range [][]string{nil, {"s1"}, {"", "claude"}} {
		if status, said := run(words...); status != 2 || !strings.HasPrefix(said, "usage: zeolite acp-bridge <sandbox ID> <agent executable>") {
			t.Errorf("%q: status %d, %q", words, status, said)
		}
	}
	if status, said := run("s2", "claude"); status != 1 || !strings.Contains(said, `zeolite acp-bridge: no sandbox has the ID "s2"`) {
		t.Errorf("another sandbox's ID: status %d, %q", status, said)
	}
	// An agent that is none is said before anything is asked of the
	// runtime, with the sandbox's name.
	if status, said := run("s1", "bash"); status != 1 || !strings.Contains(said, `zeolite acp-bridge: Research: "bash" is no agent that can be started here`) || !strings.Contains(said, "opencode") {
		t.Errorf("an agent that is none: status %d, %q", status, said)
	}
	if status, said := run("s1", "opencode", "--flag"); status != 1 || !strings.HasPrefix(said, "zeolite acp-bridge: Research: ") {
		t.Errorf("with no runtime: status %d, %q", status, said)
	}
	after, _ := os.ReadDir(dir)
	if len(after) != len(before) {
		t.Errorf("the bridge wrote in the data directory: it holds %v", after)
	}
}
