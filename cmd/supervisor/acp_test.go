package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// hostileWords are arguments that a shell which read them as text
// would run, expand or split.
var hostileWords = []string{"$(touch pwned)", "; touch pwned", "a b", "`touch pwned`", "$HOME", "-c", "'", `"`, "*", "&& exit 9", "--", "x\ny", ""}

// An agent is started through a shell that is given one text, which is
// fixed: the program and its arguments follow it as words of their
// own, whatever they hold, and nothing of them is in the text.
func TestAnAgentsShellIsGivenOneFixedText(t *testing.T) {
	if acpShellText != `exec "$@" <&4 4<&- >&3 3>&-` {
		t.Errorf("the text a shell is given is %q", acpShellText)
	}
	got := acpShellArgv("claude-agent-acp", hostileWords)
	want := append([]string{"bash", "-l", "-c", acpShellText, "acp", "claude-agent-acp"}, hostileWords...)
	if !slices.Equal(got, want) {
		t.Errorf("the shell's command line is %q, want %q", got, want)
	}
	// A program that is hostile itself is one word too.
	if got := acpShellArgv("x; touch pwned", nil); len(got) != 6 || got[3] != acpShellText || got[5] != "x; touch pwned" {
		t.Errorf("a program with a shell's words in it gives the command line %q", got)
	}

	// The supervisor's source: one place gives a shell a text, and the
	// text is the constant, which nothing is put into.
	source, err := os.ReadFile("acp.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(source), `"-c"`); n != 1 {
		t.Errorf("acp.go gives a shell a text in %d places, want one", n)
	}
	if !strings.Contains(string(source), `[]string{"bash", "-l", "-c", acpShellText, "acp", program}`) {
		t.Error("the shell's text is not the fixed one, followed by the program as a word")
	}
	for _, never := range []string{"Sprintf", "strings.Join", "acpShellText +", "+ acpShellText", "os.Getenv"} {
		if strings.Contains(string(source), never) {
			t.Errorf("acp.go holds %s: the text a shell is given is fixed", never)
		}
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "acp.go" {
			continue
		}
		other, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(other), `"-c"`) || strings.Contains(string(other), "acpShellText") {
			t.Errorf("%s gives a shell a text: only acp.go does, and a fixed one", name)
		}
	}
	main, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(main), `len(args) >= 2 && args[0] == "acp"`) || !strings.Contains(string(main), `acp(args[1], args[2:], os.Stdin, os.Stdout)`) {
		t.Error("the supervisor's command for an agent is not a program and its arguments, as words")
	}
}

// loginHome makes a home whose profile a login shell reads, and has
// the test's shells read that one and not their user's.
func loginHome(t *testing.T, profile string) string {
	t.Helper()
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no bash to start an agent through")
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	// No process of this Mac is taken for one of an agent's session.
	old := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = old })
	return home
}

// The agent has this command's streams, and its arguments as they
// were given: a shell reads none of them. What a profile prints is not
// in the agent's output, and what it reads is not taken from the
// agent's input.
func TestAnAgentHasTheStreamsAndItsArgumentsAsGiven(t *testing.T) {
	home := loginHome(t, "echo 'a profile that prints'\nread taken\necho \"it read: $taken\"\necho 'and on its error' >&2\nexport FROM_PROFILE=found\n")
	t.Chdir(home)

	var out bytes.Buffer
	status := acp("printf", append([]string{`%s\n`}, hostileWords...), strings.NewReader(""), &out)
	if want := strings.Join(hostileWords, "\n") + "\n"; status != 0 || out.String() != want {
		t.Errorf("the agent printed %q (status %d), want its arguments as given: %q", out.String(), status, want)
	}
	if entries, _ := os.ReadDir(home); len(entries) != 1 {
		t.Errorf("an argument was run: the folder holds %v", entries)
	}

	// The agent's input is this command's, whole, and its output this
	// command's; it has what the profile set.
	out.Reset()
	status = acp("sh", []string{"-c", `cat; echo "$FROM_PROFILE"; echo 'on its error' >&2`}, strings.NewReader("{\"id\":1}\n{\"id\":2}\n"), &out)
	if status != 0 || out.String() != "{\"id\":1}\n{\"id\":2}\nfound\n" {
		t.Errorf("the agent printed %q (status %d)", out.String(), status)
	}

	// Its status is the agent's, and the shell's where there is no such
	// program.
	for program, want := range map[string]int{"true": 0, "false": 1, "no-such-agent-here": 127} {
		if got := acp(program, nil, strings.NewReader(""), &out); got != want {
			t.Errorf("%s: status %d, want %d", program, got, want)
		}
	}
	if got := acp("sh", []string{"-c", "exit 7"}, strings.NewReader(""), &out); got != 7 {
		t.Errorf("an agent that exits with 7: status %d", got)
	}
	if got := acp("sh", []string{"-c", "kill -TERM $$"}, strings.NewReader(""), &out); got != 128+int(syscall.SIGTERM) {
		t.Errorf("an agent that a signal ended: status %d", got)
	}
}

// fakeProcess writes what the kernel would show of a process: its
// parent and its session.
func fakeProcess(t *testing.T, pid, parent, session int) {
	t.Helper()
	at := filepath.Join(procDir, strconv.Itoa(pid))
	if err := os.MkdirAll(at, 0o755); err != nil {
		t.Fatal(err)
	}
	stat := fmt.Sprintf("%d (agent) S %d %d %d 0 -1 4194560", pid, parent, pid, session)
	if err := os.WriteFile(filepath.Join(at, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Ending a session ends every process of it: each is hung up, and what
// stays is killed. A process of another session is left.
func TestEndingASessionEndsItsProcesses(t *testing.T) {
	oldProc, oldGrace := procDir, sessionGrace
	procDir, sessionGrace = t.TempDir(), 300*time.Millisecond
	defer func() { procDir, sessionGrace = oldProc, oldGrace }()

	start := func(script string) *exec.Cmd {
		t.Helper()
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill() })
		return cmd
	}
	leader := start("sleep 60")
	stays := start(`trap "" HUP; sleep 60 & wait`)
	other := start("sleep 60")
	// Until the one that ignores a hang-up has said so, it would not.
	time.Sleep(200 * time.Millisecond)
	fakeProcess(t, leader.Process.Pid, 1, leader.Process.Pid)
	fakeProcess(t, stays.Process.Pid, leader.Process.Pid, leader.Process.Pid)
	fakeProcess(t, other.Process.Pid, 1, other.Process.Pid)

	began := time.Now()
	endSession(leader.Process.Pid)
	if took := time.Since(began); took < sessionGrace {
		t.Errorf("a session whose processes stay was ended in %v, before they had %v to end", took, sessionGrace)
	}
	ended := func(cmd *exec.Cmd) syscall.Signal {
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
			return cmd.ProcessState.Sys().(syscall.WaitStatus).Signal()
		case <-time.After(2 * time.Second):
			return 0
		}
	}
	if sig := ended(leader); sig != syscall.SIGHUP {
		t.Errorf("the session's leader was ended by %v, want a hang-up", sig)
	}
	if sig := ended(stays); sig != syscall.SIGKILL {
		t.Errorf("a process that ignores a hang-up was ended by %v, want a kill", sig)
	}
	if err := other.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("a process of another session was ended: %v", err)
	}

	// A session with nothing left in it is ended at once.
	os.RemoveAll(filepath.Join(procDir, strconv.Itoa(leader.Process.Pid)))
	os.RemoveAll(filepath.Join(procDir, strconv.Itoa(stays.Process.Pid)))
	began = time.Now()
	endSession(leader.Process.Pid)
	if took := time.Since(began); took > sessionGrace/2 {
		t.Errorf("an empty session took %v to end", took)
	}
}
