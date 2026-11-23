package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"zeolite/internal/sandbox"
)

// A Terminal Session is a login shell the app starts with a TTY
// `docker exec`. Docker leaves it, and what it started, running when the
// app's connection closes, so the session is recorded here and ended on
// request.

// sessionDir holds one file per Terminal Session, named by its ID and
// containing the shell's process ID.
var sessionDir = filepath.Join(sandbox.SupervisorDir, "sessions")

var sessionIDPattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

func sessionFile(id string) (string, error) {
	if !sessionIDPattern.MatchString(id) {
		return "", fmt.Errorf("%q is not a session ID", id)
	}
	return filepath.Join(sessionDir, id), nil
}

// shellArgv is the command line of a session's shell: a login shell,
// which a leading hyphen in its name tells it to be. It is given no
// text to read and no program to start: what runs in a session, its
// user types there.
var shellArgv = []string{"-bash"}

// shell records this process as the session's and becomes its login
// shell, keeping its process ID. With a TTY exec that process leads a
// session of its own, which is how hangup finds everything started from
// it.
func shell(id string) error {
	file, err := sessionFile(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(file, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return err
	}
	return syscall.Exec("/bin/bash", shellArgv, os.Environ())
}

// hangup ends a Terminal Session: every process in the shell's session
// gets SIGHUP, as when a terminal closes, and SIGKILL if it stays.
func hangup(id string) error {
	file, err := sessionFile(id)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil // never started, or already ended
	}
	if err != nil {
		return err
	}
	defer os.Remove(file)
	leader, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || leader <= 1 {
		return fmt.Errorf("session %s has no process recorded", id)
	}

	signalSession(leader, syscall.SIGHUP)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sessionMembers(leader)) == 0 {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	signalSession(leader, syscall.SIGKILL)
	return nil
}

func signalSession(leader int, sig syscall.Signal) {
	for _, pid := range sessionMembers(leader) {
		syscall.Kill(pid, sig)
	}
}

// sessionMembers lists the processes whose session is the leader's.
func sessionMembers(leader int) []int {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(procDir, e.Name(), "stat"))
		if err != nil {
			continue
		}
		if session, ok := statSession(string(stat)); ok && session == leader {
			pids = append(pids, pid)
		}
	}
	return pids
}

// statSession reads the session ID from a line of /proc/<pid>/stat.
func statSession(stat string) (int, bool) {
	return statField(stat, 3)
}

// statField reads a number from a line of /proc/<pid>/stat: "pid (comm)
// state ppid pgrp session ...", counted from the state, which is 0. The
// command may hold spaces and parentheses, so fields are counted from
// its last ")".
func statField(stat string, i int) (int, bool) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, false
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) <= i {
		return 0, false
	}
	n, err := strconv.Atoi(fields[i])
	return n, err == nil
}
