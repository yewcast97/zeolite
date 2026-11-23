package hosttool

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// shimVariable is the loader's variable that loads the GPU Shim into a
// program. This Mac's own programs, those of /bin and /usr/bin, are not
// shown it and drop it for whatever they start.
const shimVariable = "DYLD_INSERT_LIBRARIES"

// gpuFiles are what a run that is given the GPU has beside the Wall,
// in one folder of the Host Tools' root that such a run reads and runs
// from, and never writes.
type gpuFiles struct {
	dir string
	// shim is the GPU Shim.
	shim string
	// startup is the Shell Startup File: one line that sets the loader's
	// variable again. zsh reads it as the .zshenv of its ZDOTDIR, bash
	// as its BASH_ENV, at every start that is not told to read no file.
	startup string
	// wrapper is the script every Homebrew Wrapper is, and wrappers the
	// folder of them: one link to it for each program of Homebrew's.
	wrapper, wrappers string
}

func gpuFilesIn(root string) gpuFiles {
	dir := filepath.Join(root, "gpu")
	return gpuFiles{
		dir:      dir,
		shim:     filepath.Join(dir, "gpushim.dylib"),
		startup:  filepath.Join(dir, ".zshenv"),
		wrapper:  filepath.Join(dir, "wrapper"),
		wrappers: filepath.Join(dir, "bin"),
	}
}

// wrapperLink is what a Homebrew Wrapper's link holds: the script,
// named from the folder of the wrappers.
const wrapperLink = "../wrapper"

// installGPU writes the GPU Shim, the Shell Startup File and the
// Homebrew Wrappers, where they are not as this build of the app has
// them.
func (t *Tools) installGPU() error {
	for _, dir := range []string{t.gpu.dir, t.gpu.wrappers} {
		if err := ownDir(dir); err != nil {
			return err
		}
	}
	set := "export " + shimVariable + "=" + shellQuote(t.gpu.shim) + "\n"
	// The wrapper is started under the name of its link, which is the
	// program's: it becomes Homebrew's program of that name.
	wrapper := "#!/bin/sh\n" + set + "exec " + shellQuote(t.homebrewBin) + `/"${0##*/}" "$@"` + "\n"
	for _, file := range []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{t.gpu.shim, gpuShim, 0o755},
		{t.gpu.startup, []byte(set), 0o644},
		{t.gpu.wrapper, []byte(wrapper), 0o755},
	} {
		if err := writeFile(file.path, file.data, file.mode); err != nil {
			return err
		}
	}
	return t.syncWrappers()
}

// writeFile makes a file hold data, replacing it whole where it holds
// anything else.
func writeFile(path string, data []byte, mode os.FileMode) error {
	if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, data) {
		return os.Chmod(path, mode)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// shellQuote writes a path as one word of a shell's.
func shellQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// syncWrappers makes the folder of the Homebrew Wrappers hold one for
// each program of Homebrew's that the loader loads, and nothing else.
// A script of Homebrew's gets none: it is read by a shell, not loaded,
// and a script that another reads by its name (gettext.sh) must be
// found as itself.
func (t *Tools) syncWrappers() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	entries, err := os.ReadDir(t.homebrewBin)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	want := map[string]bool{}
	for _, e := range entries {
		if loaded(filepath.Join(t.homebrewBin, e.Name())) {
			want[e.Name()] = true
		}
	}
	have, err := os.ReadDir(t.gpu.wrappers)
	if err != nil {
		return err
	}
	for _, e := range have {
		link := filepath.Join(t.gpu.wrappers, e.Name())
		if target, err := os.Readlink(link); err == nil && target == wrapperLink && want[e.Name()] {
			delete(want, e.Name())
			continue
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	}
	for name := range want {
		if err := os.Symlink(wrapperLink, filepath.Join(t.gpu.wrappers, name)); err != nil {
			return err
		}
	}
	return nil
}

// machOMagic are the four bytes a program the loader loads begins with:
// for one kind of processor, or for several.
var machOMagic = [][]byte{
	{0xcf, 0xfa, 0xed, 0xfe}, {0xce, 0xfa, 0xed, 0xfe},
	{0xca, 0xfe, 0xba, 0xbe}, {0xca, 0xfe, 0xba, 0xbf},
}

// loaded reports whether a file is a program the loader loads, by its
// first four bytes: the app reads no more of it.
func loaded(path string) bool {
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil {
		return false
	}
	for _, m := range machOMagic {
		if bytes.Equal(magic, m) {
			return true
		}
	}
	return false
}

// shellThatReadsTheStartupFile is a request's command line as a run
// with the GPU starts it. sh reads no file when it starts, so nothing
// would set the loader's variable for what it starts: at the head of a
// request it is started as bash, which is the same program on this Mac
// under the name that reads the Shell Startup File. The difference
// shows in a script (echo -n), so agents are told.
func shellThatReadsTheStartupFile(argv []string) []string {
	if argv[0] != "sh" && argv[0] != "/bin/sh" {
		return argv
	}
	return append([]string{"/bin/bash"}, argv[1:]...)
}
