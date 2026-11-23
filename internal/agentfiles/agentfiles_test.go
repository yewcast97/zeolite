//go:build darwin

package agentfiles

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// The tests change modes and flags by a walk, which is what the package
// is for. A walk that went astray would change them wherever it went:
// one that was once given the root of this Mac for a name did. So the
// tests run in a process that may write nowhere but a temporary folder
// of its own: the test starts itself again under a Seatbelt profile
// that says so, and a test below fails where it is not in force.
const confinedVariable = "ZEOLITE_AGENTFILES_CONFINED"

func TestMain(m *testing.M) {
	if os.Getenv(confinedVariable) != "" {
		os.Exit(m.Run())
	}
	os.Exit(confined())
}

// confined runs this test binary again, as it was run, under the
// profile, and returns how it ended.
func confined() int {
	dir, err := os.MkdirTemp("", "agentfiles")
	if err == nil {
		dir, err = filepath.EvalSymlinks(dir)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "a temporary folder for the tests:", err)
		return 1
	}
	defer os.RemoveAll(dir)
	// What go test itself has the binary write: its coverage, where it
	// was asked for.
	writes := fmt.Sprintf("(literal \"/dev/null\") (subpath %q)", dir)
	for _, arg := range os.Args[1:] {
		for flag, filter := range map[string]string{"-test.gocoverdir=": "subpath", "-test.outputdir=": "subpath", "-test.coverprofile=": "literal"} {
			if path, ok := strings.CutPrefix(arg, flag); ok {
				// Seatbelt names a path as it resolves.
				if abs, err := filepath.Abs(path); err == nil {
					if dir, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
						writes += fmt.Sprintf(" (%s %q)", filter, filepath.Join(dir, filepath.Base(abs)))
					}
				}
			}
		}
	}
	profile := "(version 1)(allow default)(deny file-write*)(allow file-write* " + writes + ")"
	cmd := exec.Command("/usr/bin/sandbox-exec", append([]string{"-p", profile, os.Args[0]}, os.Args[1:]...)...)
	cmd.Env = append(os.Environ(), confinedVariable+"=1", "TMPDIR="+dir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			fmt.Fprintln(os.Stderr, "running the tests where they may write only their own folder:", err)
		}
		return 1
	}
	return 0
}

// The tests may change nothing outside their temporary folder: not the
// mode of this package's own folder, set here to what it is, nor a
// file's flags there, nor a new file.
func TestTheTestsWriteOnlyTheirOwnFolder(t *testing.T) {
	info, err := os.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(".", info.Mode().Perm()); err == nil {
		t.Error("the tests may change a mode outside their temporary folder")
	}
	if err := unix.Chflags("agentfiles.go", int(flagsOf(t, "agentfiles.go"))); err == nil {
		t.Error("the tests may change flags outside their temporary folder")
	}
	if file, err := os.CreateTemp(".", "written-by-a-test"); err == nil {
		file.Close()
		os.Remove(file.Name())
		t.Error("the tests may write a file outside their temporary folder")
	}
	inside := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(inside, nil, 0o600); err != nil {
		t.Errorf("the tests may not write their own temporary folder: %v", err)
	} else if err := os.Chmod(inside, 0o400); err != nil {
		t.Errorf("the tests may not change a mode in their own temporary folder: %v", err)
	}
}

// An entry's name is one name: nothing with a separator in it, which
// would be a path, and not the folder itself or the one above it. A path
// that begins with a separator is taken from the root of this Mac
// whatever folder it is given with, so the root's own name is the first
// that is none. The test touches no disk.
func TestAnEntrysNameIsOneName(t *testing.T) {
	for name, want := range map[string]bool{
		"tree": true, "s1": true, "a.b": true, "..x": true, "x..": true, ".hidden": true, "...": true,
		"with space": true, "研究": true, `back\slash`: true, "-rf": true, "~": true,

		"": false, ".": false, "..": false,
		"/": false, "//": false, "/.": false, "/..": false, "/etc": false, "/Users": false,
		"a/b": false, "a/": false, "/a": false, "./a": false, "../a": false, "a/..": false, "a/.": false,
		"tree/": false, "tree//": false, "../..": false, "a/b/c": false,
	} {
		if got := entryName(name); got != want {
			t.Errorf("%q is one entry's name: %v, want %v", name, got, want)
		}
	}
}

// left is a folder of the app's with one tree in it, as an agent left
// it, and beside the folder what is the user's.
type left struct {
	t *testing.T
	// folder holds the tree, by the name "tree"; outside is not the
	// agents'.
	folder, tree, outside string
}

func newLeft(t *testing.T) *left {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := &left{t: t, folder: filepath.Join(base, "folder"), outside: filepath.Join(base, "outside")}
	l.tree = filepath.Join(l.folder, "tree")
	for _, dir := range []string{l.tree, l.outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Whatever the test leaves, the test's own folder can be deleted.
	t.Cleanup(func() { unlock(base) })
	return l
}

// unlock undoes, for a tree of the test's own, everything the test did
// to keep it: by paths, which is good enough for what no agent wrote.
func unlock(path string) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	unix.Chflags(path, 0)
	if !info.IsDir() {
		return
	}
	os.Chmod(path, 0o700)
	entries, _ := os.ReadDir(path)
	for _, e := range entries {
		unlock(filepath.Join(path, e.Name()))
	}
}

func (l *left) dir(path string) string {
	l.t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		l.t.Fatal(err)
	}
	return path
}

func (l *left) file(path string) string {
	l.t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		l.t.Fatal(err)
	}
	return path
}

// flag sets flags on an entry itself, a link's on the link, as a
// program does with chflags -h.
func (l *left) flag(path string, flags uint32) {
	l.t.Helper()
	dir, err := unix.Open(filepath.Dir(path), unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		l.t.Fatal(err)
	}
	defer unix.Close(dir)
	if err := setFlags(dir, filepath.Base(path), flags); err != nil {
		l.t.Fatalf("setting the flags of %s: %v", path, err)
	}
	if got := flagsOf(l.t, path); got != flags {
		l.t.Fatalf("the flags of %s are %#x, want %#x", path, got, flags)
	}
}

func (l *left) mode(path string, mode os.FileMode) {
	l.t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		l.t.Fatal(err)
	}
}

func flagsOf(t *testing.T, path string) uint32 {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Flags
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// Each way an agent leaves a tree so that it cannot be deleted as it
// is: the test first sees that it cannot, and then that Remove deletes
// it.
func TestRemoveDeletesWhatWasLeftToStay(t *testing.T) {
	for what, leave := range map[string]func(l *left){
		// As a Go module cache is left.
		"folders that are read-only": func(l *left) {
			mod := l.dir(filepath.Join(l.tree, "pkg", "mod"))
			l.file(filepath.Join(mod, "file.go"))
			l.mode(mod, 0o500)
			l.mode(filepath.Dir(mod), 0o500)
		},
		"a folder that may not even be listed": func(l *left) {
			sealed := l.dir(filepath.Join(l.tree, "sealed"))
			l.file(filepath.Join(l.dir(filepath.Join(sealed, "in")), "file"))
			l.mode(sealed, 0)
		},
		"a file that may not be changed": func(l *left) {
			l.flag(l.file(filepath.Join(l.tree, "kept")), unix.UF_IMMUTABLE)
		},
		"a file that may only be added to": func(l *left) {
			l.flag(l.file(filepath.Join(l.tree, "log")), unix.UF_APPEND)
		},
		"a file that may be neither read nor changed": func(l *left) {
			kept := l.file(filepath.Join(l.tree, "kept"))
			l.mode(kept, 0)
			l.flag(kept, unix.UF_IMMUTABLE)
		},
		"a folder that may not be changed": func(l *left) {
			kept := l.dir(filepath.Join(l.tree, "kept"))
			l.file(filepath.Join(kept, "file"))
			l.flag(kept, unix.UF_IMMUTABLE)
		},
		"a folder that may be neither listed nor changed, deep in others like it": func(l *left) {
			outer := l.dir(filepath.Join(l.tree, "outer"))
			inner := l.dir(filepath.Join(outer, "inner"))
			l.file(filepath.Join(inner, "file"))
			l.flag(l.file(filepath.Join(inner, "kept")), unix.UF_IMMUTABLE|unix.UF_HIDDEN)
			for _, dir := range []string{inner, outer} {
				l.mode(dir, 0)
				l.flag(dir, unix.UF_IMMUTABLE)
			}
		},
		"a link that may not be changed": func(l *left) {
			link := filepath.Join(l.tree, "link")
			if err := os.Symlink(l.outside, link); err != nil {
				t.Fatal(err)
			}
			l.flag(link, unix.UF_IMMUTABLE)
		},
		"the tree itself, which may not be changed": func(l *left) {
			l.file(filepath.Join(l.tree, "file"))
			l.mode(l.tree, 0o500)
			l.flag(l.tree, unix.UF_IMMUTABLE)
		},
	} {
		l := newLeft(t)
		leave(l)
		root, err := os.OpenRoot(l.folder)
		if err != nil {
			t.Fatal(err)
		}
		err = root.RemoveAll("tree")
		root.Close()
		if err == nil {
			t.Errorf("%s: the tree was deleted as it was: the test leaves nothing to stay", what)
			continue
		}
		if err := Remove(l.folder, "tree"); err != nil {
			t.Errorf("%s: %v", what, err)
		}
		if _, err := os.Lstat(l.tree); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the tree is still there (%v)", what, err)
		}
	}
}

// What a link in the tree leads to is the user's, and stays as it is:
// not deleted, not listed, and neither its mode nor its flags changed,
// though the tree is full of what Remove changes.
func TestRemoveFollowsNoLink(t *testing.T) {
	l := newLeft(t)
	precious := l.file(filepath.Join(l.outside, "precious.txt"))
	l.mode(precious, 0o640)
	l.flag(precious, unix.UF_IMMUTABLE)
	mine := l.dir(filepath.Join(l.outside, "mine"))
	inner := l.file(filepath.Join(mine, "inner.txt"))
	l.flag(inner, unix.UF_APPEND)
	l.mode(mine, 0o550)
	l.flag(mine, unix.UF_IMMUTABLE)
	sibling := l.dir(filepath.Join(l.folder, "sibling"))
	l.file(filepath.Join(sibling, "file"))
	l.mode(sibling, 0o500)

	nested := l.dir(filepath.Join(l.tree, "a", "b"))
	for _, link := range []struct{ target, at string }{
		{precious, filepath.Join(l.tree, "to-a-file")},
		{mine, filepath.Join(l.tree, "to-a-folder")},
		{mine, filepath.Join(nested, "to-a-folder")},
		{"../../../outside/mine", filepath.Join(nested, "upwards")},
		{"../sibling", filepath.Join(l.tree, "to-a-sibling")},
		{l.folder, filepath.Join(l.tree, "to-the-folder")},
	} {
		if err := os.Symlink(link.target, link.at); err != nil {
			t.Fatal(err)
		}
	}
	// So that the tree is not deleted as it is, and every folder of it
	// is gone through: each holds something that was left to stay.
	l.flag(filepath.Join(l.tree, "to-a-folder"), unix.UF_IMMUTABLE)
	l.flag(l.file(filepath.Join(nested, "kept")), unix.UF_IMMUTABLE)
	l.mode(nested, 0o500)
	l.mode(filepath.Dir(nested), 0)

	if err := Remove(l.folder, "tree"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(l.tree); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the tree is still there (%v)", err)
	}
	for path, want := range map[string]struct {
		mode  os.FileMode
		flags uint32
	}{
		precious: {0o640, unix.UF_IMMUTABLE},
		mine:     {0o550, unix.UF_IMMUTABLE},
		inner:    {0o600, unix.UF_APPEND},
		sibling:  {0o500, 0},
		l.folder: {0o700, 0},
	} {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("deleting the tree took %s with it: %v", path, err)
			continue
		}
		if mode, flags := modeOf(t, path), flagsOf(t, path); mode != want.mode || flags != want.flags {
			t.Errorf("%s, outside the tree, now has the mode %o and the flags %#x: it had %o and %#x", path, mode, flags, want.mode, want.flags)
		}
	}
	if _, err := os.Lstat(filepath.Join(sibling, "file")); err != nil {
		t.Errorf("deleting the tree took what a sibling holds: %v", err)
	}
}

// The walk stays on the volume it began on. A disk mounted in the tree
// shows as an entry on another volume: told that the tree is on another
// than the one it began on, the walk changes nothing of it, not a mode
// and not a flag; told the volume it is on, it loosens all of it.
func TestTheWalkStaysOnItsVolume(t *testing.T) {
	l := newLeft(t)
	inner := l.dir(filepath.Join(l.tree, "inner"))
	kept := l.file(filepath.Join(inner, "kept"))
	l.flag(kept, unix.UF_IMMUTABLE)
	l.mode(inner, 0o500)
	l.flag(inner, unix.UF_APPEND)
	l.mode(l.tree, 0o550)

	folder, err := unix.Open(l.folder, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(folder)
	var st unix.Stat_t
	if err := unix.Fstat(folder, &st); err != nil {
		t.Fatal(err)
	}
	is := func(when string, treeMode, innerMode os.FileMode, innerFlags, keptFlags uint32) {
		t.Helper()
		if got := modeOf(t, l.tree); got != treeMode {
			t.Errorf("%s the tree's mode is %o, want %o", when, got, treeMode)
		}
		if mode, flags := modeOf(t, inner), flagsOf(t, inner); mode != innerMode || flags != innerFlags {
			t.Errorf("%s a folder in it has the mode %o and the flags %#x, want %o and %#x", when, mode, flags, innerMode, innerFlags)
		}
		if got := flagsOf(t, kept); got != keptFlags {
			t.Errorf("%s a file in it has the flags %#x, want %#x", when, got, keptFlags)
		}
	}
	loosen(folder, "tree", st.Dev+1)
	is("taken for another volume's,", 0o550, 0o500, unix.UF_APPEND, unix.UF_IMMUTABLE)
	loosen(folder, "tree", st.Dev)
	is("on its own volume,", 0o700, 0o700, 0, 0)
}

// A tree is named by one name in its folder, and by nothing that leads
// elsewhere; one that is not there is deleted already.
func TestRemoveTakesOneNameInTheFolder(t *testing.T) {
	l := newLeft(t)
	l.file(filepath.Join(l.tree, "file"))
	l.file(filepath.Join(l.dir(filepath.Join(l.tree, "sub")), "file"))
	l.file(filepath.Join(l.outside, "file"))
	for _, name := range []string{"", ".", "..", "../outside", "tree/sub", "tree/", "/", l.tree, "./tree"} {
		if err := Remove(l.folder, name); err == nil {
			t.Errorf("deleting %q was done", name)
		}
	}
	for _, kept := range []string{filepath.Join(l.tree, "sub", "file"), filepath.Join(l.outside, "file")} {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("a name that is no tree's deleted %s: %v", kept, err)
		}
	}
	if err := Remove(l.folder, "tree"); err != nil {
		t.Fatal(err)
	}
	if err := Remove(l.folder, "tree"); err != nil {
		t.Errorf("deleting a tree that is not there: %v", err)
	}
	if err := Remove(filepath.Join(l.folder, "no-folder"), "tree"); err != nil {
		t.Errorf("deleting where there is no folder: %v", err)
	}
}

// What could not be deleted is said, and the folder the tree is in is
// the app's and is left as it is: here it forbids the change itself, so
// the tree's name stays in it, and its mode and its flags stay too.
func TestRemoveSaysWhatItCouldNotDelete(t *testing.T) {
	l := newLeft(t)
	l.file(filepath.Join(l.tree, "file"))
	l.mode(l.folder, 0o750)
	l.flag(l.folder, unix.UF_IMMUTABLE)
	if err := Remove(l.folder, "tree"); err == nil {
		t.Error("a tree in a folder that may not be changed is said to be deleted")
	}
	if _, err := os.Lstat(l.tree); err != nil {
		t.Errorf("the tree's name is gone from a folder that may not be changed: %v", err)
	}
	if mode, flags := modeOf(t, l.folder), flagsOf(t, l.folder); mode != 0o750 || flags != unix.UF_IMMUTABLE {
		t.Errorf("the folder the tree is in now has the mode %o and the flags %#x", mode, flags)
	}
}

// reads are the calls by which a program reads a file's content or a
// link's target, or follows a link to what it names: the package calls
// none. noFollow are the calls that name an entry by its folder's
// descriptor, and what each must be told so that it takes a link
// itself; a folder alone is opened, to be listed.
var (
	reads = map[string]bool{
		"ReadFile": true, "Open": true, "Create": true, "Readlink": true, "EvalSymlinks": true,
		"Stat": true, "Lstat": true, "Walk": true, "WalkDir": true, "Glob": true, "Copy": true, "ReadAll": true, "Read": true,
		"ReadDir": true, "Chmod": true, "Chflags": true, "Chown": true, "FS": true,
	}
	noFollow = map[string][]string{
		"Fstatat":  {"AT_SYMLINK_NOFOLLOW"},
		"Fchmodat": {"AT_SYMLINK_NOFOLLOW"},
		"Openat":   {"O_DIRECTORY", "O_NOFOLLOW"},
		"OpenFile": {"O_DIRECTORY"},
		"Syscall6": {"SYS_SETATTRLISTAT", "FSOPT_NOFOLLOW"},
	}
)

// howItReaches says what is wrong with how a source file reaches the
// disk: a call that reads or follows, or one by a descriptor that is
// not told to take a link itself.
func howItReaches(t *testing.T, name string, source any) (problems []string, calls int) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		calls++
		if reads[sel.Sel.Name] {
			problems = append(problems, "calls "+sel.Sel.Name)
		}
		told := map[string]bool{}
		for _, arg := range call.Args {
			ast.Inspect(arg, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok {
					told[id.Name] = true
				}
				return true
			})
		}
		for _, must := range noFollow[sel.Sel.Name] {
			if !told[must] {
				problems = append(problems, "calls "+sel.Sel.Name+" without "+must)
			}
		}
		return true
	})
	return problems, calls
}

func TestThePackageReadsNothingAndFollowsNoLink(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	files, calls := 0, 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		problems, n := howItReaches(t, name, nil)
		calls += n
		for _, problem := range problems {
			t.Errorf("%s %s: what agents wrote is listed and deleted, and never read or followed", name, problem)
		}
	}
	if files < 1 || calls < 15 {
		t.Fatalf("read %d files with %d calls: the test would prove little", files, calls)
	}
	// The search finds each way.
	sample := `package p
func f(root *os.Root, dir int) {
	os.ReadFile("a"); root.Open("b"); os.Stat("c"); os.Chmod("d", 0)
	unix.Fstatat(dir, "e", &st, 0)
	unix.Openat(dir, "f", unix.O_RDONLY|unix.O_DIRECTORY, 0)
	unix.Fchmodat(dir, "g", 0o700, unix.AT_SYMLINK_NOFOLLOW)
}
`
	if got, _ := howItReaches(t, "sample.go", sample); len(got) != 6 {
		t.Errorf("the search found %v in a file that reaches the disk six wrong ways", got)
	}
}
