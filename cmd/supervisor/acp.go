package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// A sandbox's ACP Bridge has an agent started in the workstation: the
// app runs `supervisor acp <program> [args...]` there, without a
// terminal, and the standard streams of that command are the agent's,
// on which it speaks the Agent Client Protocol with a client on the
// Mac. It is the one case in which the app starts an agent, and the
// agent is the one its client named.
//
// The program is started through a login shell, so that it is found
// where a Terminal Session finds what its user installed. That shell is
// given one text, which is fixed: the program and its arguments are the
// shell's positional words, and are never read as text.

// acpShellText is the one text a shell is given by the supervisor: it
// becomes the program its positional words name, with the agent's
// streams, which wait on two descriptors of their own, as its standard
// input and output. Until then the shell's input is empty and what it
// prints goes nowhere, so that a profile that reads or prints takes
// nothing from the protocol and adds nothing to it.
const acpShellText = `exec "$@" <&4 4<&- >&3 3>&-`

// acpShellArgv is the command line of the shell that becomes an agent:
// a login shell, the fixed text, the name the shell goes by, and then
// the agent's program and its arguments, each a word of its own.
func acpShellArgv(program string, args []string) []string {
	return append([]string{"bash", "-l", "-c", acpShellText, "acp", program}, args...)
}

// acp starts an agent's program and passes its standard streams on:
// what arrives on this command's input is the agent's input, and what
// the agent prints is this command's output. The agent leads a session
// of its own. When this command's input ends, its client being gone,
// when its output can no longer be written, or when the agent exits,
// every process of that session is ended. It returns the status to
// exit with: the agent's, or the shell's where it found no such
// program.
//
// The streams this command was started with are held by it alone. The
// agent has pipes of this command's, and no standard error: a process
// that outlives the session keeps nothing open that the app waits on.
func acp(program string, args []string, in io.Reader, out io.Writer) int {
	// Writing to a client that is gone is an error here, and no signal:
	// the agent's session is still to be ended.
	signal.Ignore(syscall.SIGPIPE)
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return acpFailed(err)
	}
	fromAgent, agentOut, err := os.Pipe()
	if err != nil {
		return acpFailed(err)
	}
	agentIn, toAgent, err := os.Pipe()
	if err != nil {
		return acpFailed(err)
	}
	cmd := &exec.Cmd{
		Path:   "/bin/bash",
		Args:   acpShellArgv(program, args),
		Env:    os.Environ(),
		Stdin:  null,
		Stdout: null,
		Stderr: null,
		// Descriptors 3 and 4, which the fixed text makes the agent's
		// standard output and input.
		ExtraFiles:  []*os.File{agentOut, agentIn},
		SysProcAttr: &syscall.SysProcAttr{Setsid: true},
	}
	if err := cmd.Start(); err != nil {
		return acpFailed(err)
	}
	null.Close()
	agentOut.Close()
	agentIn.Close()
	leader := cmd.Process.Pid

	ended := make(chan struct{}, 3)
	printed := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		io.Copy(toAgent, in)
		toAgent.Close()
		ended <- struct{}{}
	}()
	go func() {
		io.Copy(out, fromAgent)
		close(printed)
		ended <- struct{}{}
	}()
	go func() {
		cmd.Wait()
		close(exited)
		ended <- struct{}{}
	}()
	<-ended
	endSession(leader)
	<-exited
	// What the agent printed last is passed on, unless something that
	// left its session still holds the pipe.
	select {
	case <-printed:
	case <-time.After(time.Second):
	}
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return cmd.ProcessState.ExitCode()
}

func acpFailed(err error) int {
	fmt.Fprintln(os.Stderr, "supervisor:", err)
	return 1
}
