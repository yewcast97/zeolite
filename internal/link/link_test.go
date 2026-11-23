package link

import (
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zeolite/internal/sandbox"
)

// ends is the two ends of a sandbox's Link, as a test holds them.
type ends struct {
	t      *testing.T
	toApp  *io.PipeWriter
	ofApp  *json.Decoder
	served chan error
}

type bothEnds struct {
	io.Reader
	io.Writer
	close func()
}

func (b bothEnds) Close() error { b.close(); return nil }

func serve(t *testing.T, h Handlers) *ends {
	fromTest, toApp := io.Pipe()
	ofApp, toTest := io.Pipe()
	e := &ends{t: t, toApp: toApp, ofApp: json.NewDecoder(ofApp), served: make(chan error, 1)}
	go func() {
		e.served <- Serve(bothEnds{fromTest, toTest, func() { fromTest.Close(); toTest.Close() }}, h)
	}()
	t.Cleanup(func() { toApp.Close() })
	return e
}

func (e *ends) send(line string) {
	e.t.Helper()
	if _, err := io.WriteString(e.toApp, line+"\n"); err != nil {
		e.t.Fatal(err)
	}
}

// answers reads n answers, by ask.
func (e *ends) answers(n int) map[uint64]sandbox.LinkAnswer {
	e.t.Helper()
	got := map[uint64]sandbox.LinkAnswer{}
	for len(got) < n {
		var a sandbox.LinkAnswer
		if err := e.ofApp.Decode(&a); err != nil {
			e.t.Fatalf("after %d answers: %v", len(got), err)
		}
		got[a.ID] = a
	}
	return got
}

func (e *ends) ended() error {
	e.t.Helper()
	select {
	case err := <-e.served:
		return err
	case <-time.After(10 * time.Second):
		e.t.Fatal("the Link went on")
		return nil
	}
}

// runs is a stand-in for a sandbox's Host Tools: a run prints its
// program's name, and one named wait goes on until it is withdrawn.
type runs struct {
	mu        sync.Mutex
	started   []string
	withdrawn atomic.Int32
	going     atomic.Int32
}

func (r *runs) run(ctx context.Context, request sandbox.HostToolRequest) sandbox.HostToolResult {
	r.mu.Lock()
	r.started = append(r.started, strings.Join(request.Argv, " "))
	r.mu.Unlock()
	r.going.Add(1)
	defer r.going.Add(-1)
	if len(request.Argv) > 0 && request.Argv[0] == "wait" {
		<-ctx.Done()
		r.withdrawn.Add(1)
		return sandbox.HostToolResult{ExitCode: -1}
	}
	return sandbox.HostToolResult{Stdout: strings.Join(request.Argv, " ")}
}

// A sandbox's Link is answered ask by ask, side by side; an ask that is
// withdrawn is ended and not answered; and when the Link ends, so does
// everything an ask started.
func TestALinkIsServed(t *testing.T) {
	tools := &runs{}
	e := serve(t, Handlers{HostTool: tools.run})
	e.send(`{"id":1,"hostTool":{"argv":["one"]}}`)
	e.send(`{"id":2,"hostTool":{"argv":["two","words"],"dir":"sub"}}`)
	got := e.answers(2)
	if got[1].HostTool == nil || got[1].HostTool.Stdout != "one" || got[2].HostTool == nil || got[2].HostTool.Stdout != "two words" || got[1].Refused != "" {
		t.Errorf("two asks were answered %+v", got)
	}

	// An ask of no kind, and one of a kind the app does not know, are
	// refused, each under its number, and the Link goes on.
	for id, line := range map[uint64]string{3: `{"id":3}`, 4: `{"id":4,"somethingElse":{"x":1}}`} {
		e.send(line)
		if got := e.answers(1); !strings.Contains(got[id].Refused, "no kind") || got[id].HostTool != nil {
			t.Errorf("an ask of no kind was answered %+v", got)
		}
	}

	// Asks that wait are worked on side by side, as many as LinkMaxAsks,
	// and one more is refused.
	for id := 100; id < 100+sandbox.LinkMaxAsks; id++ {
		e.send(fmt.Sprintf(`{"id":%d,"hostTool":{"argv":["wait"]}}`, id))
	}
	waitFor(t, "the asks to be worked on", func() bool { return int(tools.going.Load()) == sandbox.LinkMaxAsks })
	e.send(`{"id":999,"hostTool":{"argv":["too-many"]}}`)
	if got := e.answers(1); !strings.Contains(got[999].Refused, "at once") {
		t.Errorf("one ask too many was answered %+v", got)
	}

	// One is withdrawn: it is ended, nothing is answered for it, and its
	// place is free. A withdrawal of what was never asked changes nothing.
	e.send(`{"id":100,"cancel":true}`)
	e.send(`{"id":5555,"cancel":true}`)
	waitFor(t, "the withdrawn ask to end", func() bool { return tools.withdrawn.Load() == 1 })
	e.send(`{"id":7,"hostTool":{"argv":["after"]}}`)
	if got := e.answers(1); got[7].HostTool == nil || got[7].HostTool.Stdout != "after" {
		t.Errorf("after a withdrawal the next answer was %+v", got)
	}

	// The Link ends: every ask does.
	e.toApp.Close()
	if err := e.ended(); err != nil {
		t.Errorf("a Link that ended was served with %v", err)
	}
	if n := int(tools.withdrawn.Load()); n != sandbox.LinkMaxAsks || tools.going.Load() != 0 {
		t.Errorf("after the Link ended %d asks were ended, and %d go on", n, tools.going.Load())
	}
}

// A raised hand is handed to what takes hands, with the session the
// workstation named and nothing else, and answered: taken, or refused
// with why.
func TestAHandIsHandedOn(t *testing.T) {
	var mu sync.Mutex
	var raised []string
	e := serve(t, Handlers{Hand: func(r sandbox.HandRequest) string {
		mu.Lock()
		defer mu.Unlock()
		raised = append(raised, r.Session)
		if r.Session != "aaa" {
			return "the hand is raised from no Terminal Session the app opened in this sandbox"
		}
		return ""
	}})
	e.send(`{"id":1,"hand":{"session":"aaa"}}`)
	if got := e.answers(1); got[1].Refused != "" || got[1].HostTool != nil {
		t.Errorf("a hand was answered %+v", got)
	}
	e.send(`{"id":2,"hand":{"session":"zzz","words":"look at me"}}`)
	if got := e.answers(1); !strings.Contains(got[2].Refused, "no Terminal Session") {
		t.Errorf("a hand of a session that is not the sandbox's was answered %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(raised, " ") != "aaa zzz" {
		t.Errorf("the hands handed on are %q", raised)
	}
}

// A sandbox that is given no Host Tools is refused them, in words an
// agent can act on, and nothing is run.
func TestWhatASandboxIsNotGivenIsRefused(t *testing.T) {
	e := serve(t, Handlers{})
	e.send(`{"id":1,"hostTool":{"argv":["ls"]}}`)
	if got := e.answers(1); !strings.Contains(got[1].Refused, "given no Host Tools") || got[1].HostTool != nil {
		t.Errorf("a Host Tool's request of a sandbox that has none was answered %+v", got)
	}
}

// What is no ask ends the Link: the workstation's end of it is the
// app's own program, which sends none. A line is bounded before it is
// read.
func TestWhatIsNoAskEndsTheLink(t *testing.T) {
	for _, bad := range []string{"not json", `{"id":"one"}`, `{"id":1,"hostTool":{"argv":["` + strings.Repeat("x", sandbox.LinkMaxLine) + `"]}}`} {
		tools := &runs{}
		e := serve(t, Handlers{HostTool: tools.run})
		e.send(bad)
		if err := e.ended(); err == nil {
			t.Errorf("the Link went on after %.20q", bad)
		}
		if len(tools.started) != 0 {
			t.Errorf("%.20q started %v", bad, tools.started)
		}
	}
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !done(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s", what)
		}
	}
}

// The package imports the core alone: it starts no program, opens no
// file and makes no socket. What an ask asks for is done by what the
// app handed it.
func TestThePackageImportsTheCoreAlone(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		files++
		f, err := parser.ParseFile(token.NewFileSet(), e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			switch {
			case path == "zeolite/internal/sandbox":
			case strings.HasPrefix(path, "zeolite/"):
				t.Errorf("%s imports %s", e.Name(), path)
			case strings.Contains(strings.SplitN(path, "/", 2)[0], "."):
				t.Errorf("%s imports %s, which is not the standard library's", e.Name(), path)
			case path == "net" || strings.HasPrefix(path, "net/") || path == "os" || path == "os/exec":
				t.Errorf("%s imports %s: the Link is a stream it is given", e.Name(), path)
			}
		}
	}
	if files != 1 {
		t.Errorf("read %d files", files)
	}
}
