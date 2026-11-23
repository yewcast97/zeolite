package dashboard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"zeolite/internal/sandbox"
)

// Every glyph is a rectangle of marks and dots with something marked in
// it: the controls' and the states' eight cells a side, the agents'
// twelve. Every state has one and every known agent has one, and no two
// agents have the same.
func TestGlyphsAreWellFormed(t *testing.T) {
	controls := map[string]glyph{
		"glyphStart": glyphStart, "glyphStop": glyphStop, "glyphConfigure": glyphConfigure, "glyphClone": glyphClone,
		"glyphDestroy": glyphDestroy, "glyphAdd": glyphAdd, "glyphRemove": glyphRemove, "glyphClose": glyphClose,
		"glyphUp": glyphUp, "glyphAddPane": glyphAddPane, "glyphHide": glyphHide, "glyphCalls": glyphCalls, "glyphFolder": glyphFolder,
		"glyphInfo": glyphInfo, "glyphHand": glyphHand, "glyphCheck": glyphCheck,
	}
	check := func(name string, g glyph, side int) {
		t.Helper()
		if len(g) != side || g.columns() != side {
			t.Errorf("%s is %d cells by %d, want %d a side", name, g.columns(), len(g), side)
		}
		marks := 0
		for i, row := range g {
			if len(row) != g.columns() {
				t.Errorf("%s: row %d is %d cells wide, the widest %d", name, i+1, len(row), g.columns())
			}
			if rest := strings.Trim(row, string(mark)+"."); rest != "" {
				t.Errorf("%s: row %d holds %q, which is neither a mark nor nothing", name, i+1, rest)
			}
			marks += strings.Count(row, string(mark))
		}
		if marks == 0 {
			t.Errorf("%s has no mark", name)
		}
	}
	for name, g := range controls {
		check(name, g, 8)
	}

	// The glyphs checked by name are all there are: no glyph of the file
	// is left out.
	file, err := parser.ParseFile(token.NewFileSet(), "glyphs.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, value := range spec.Values {
			lit, ok := value.(*ast.CompositeLit)
			if !ok {
				continue
			}
			if kind, ok := lit.Type.(*ast.Ident); ok && kind.Name == "glyph" {
				if name := spec.Names[i].Name; controls[name] == nil {
					t.Errorf("%s is a glyph this test does not check", name)
				}
			}
		}
		return true
	})

	states := []sandbox.State{sandbox.StateStopped, sandbox.StateStarting, sandbox.StateGateOpen, sandbox.StateGateLocked, sandbox.StateError}
	for _, state := range states {
		g, ok := stateGlyphs[state]
		if !ok {
			t.Errorf("the state %s has no glyph", state)
			continue
		}
		check("the glyph of "+string(state), g, 8)
	}
	if len(stateGlyphs) != len(states) {
		t.Errorf("there are %d glyphs of states for %d states", len(stateGlyphs), len(states))
	}
	// Two states never look the same.
	seen := map[string]string{}
	same := func(what string, g glyph) {
		t.Helper()
		art := strings.Join(g, "\n")
		if other, twice := seen[art]; twice {
			t.Errorf("%s and %s have the same glyph", what, other)
		}
		seen[art] = what
	}
	for _, state := range states {
		same("the state "+string(state), stateGlyphs[state])
	}

	seen = map[string]string{}
	for _, a := range sandbox.KnownAgents {
		g, ok := agentGlyphs[a.Executable]
		if !ok {
			t.Errorf("%s has no glyph", a.Title)
			continue
		}
		check("the glyph of "+a.Title, g, 12)
		same(a.Title, g)
	}
	if len(agentGlyphs) != len(sandbox.KnownAgents) {
		t.Errorf("there are %d glyphs of agents for %d known agents", len(agentGlyphs), len(sandbox.KnownAgents))
	}
	// No two controls look the same either.
	seen = map[string]string{}
	for name, g := range controls {
		same(name, g)
	}
}
