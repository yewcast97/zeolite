package sharedfolder

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// folders are the folders under a root of the test's own.
func folders(t *testing.T) *Folders {
	t.Helper()
	// The root's parent is made too, as /Users/Shared/zeolite is.
	f := New(filepath.Join(t.TempDir(), "zeolite", "shared.noindex"))
	if _, err := f.Prepare(); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestPrepareMakesTheRootAndItsMarker(t *testing.T) {
	f := New(filepath.Join(t.TempDir(), "zeolite", "shared.noindex"))
	token, err := f.Prepare()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(f.Root())
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("the root is %v (%v), want a directory of the user's alone", info, err)
	}
	marker, err := os.ReadFile(filepath.Join(f.Root(), MarkerName))
	if err != nil || strings.TrimSpace(string(marker)) != token || len(token) < 26 {
		t.Errorf("the marker holds %q (%v), want the token %q", marker, err, token)
	}

	// A Shared Folder is kept: preparing again deletes nothing, and
	// writes another marker.
	if err := f.Create("research"); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(f.Path("research"), "work.txt")
	if err := os.WriteFile(kept, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := f.Prepare()
	if err != nil {
		t.Fatal(err)
	}
	if again == token {
		t.Error("preparing again wrote the same marker")
	}
	if data, err := os.ReadFile(kept); err != nil || string(data) != "kept" {
		t.Errorf("preparing again lost the Shared Folder's file: %q (%v)", data, err)
	}
}

func TestPrepareWritesThroughNoLink(t *testing.T) {
	f := folders(t)
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(f.Root(), MarkerName)
	for _, name := range []string{marker, marker + ".tmp"} {
		os.Remove(name)
		if err := os.Symlink(victim, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.Prepare(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(victim); string(data) != "mine" {
		t.Errorf("the marker was written through a link: the file it led to holds %q", data)
	}
	if info, err := os.Lstat(marker); err != nil || !info.Mode().IsRegular() {
		t.Errorf("the marker is %v (%v), want a file", info, err)
	}
}

func TestPrepareRefusesARootThatIsNotTheUsersDirectory(t *testing.T) {
	dir := t.TempDir()
	elsewhere := filepath.Join(dir, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "zeolite"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "zeolite", "shared.noindex")
	if err := os.Symlink(elsewhere, root); err != nil {
		t.Fatal(err)
	}
	if _, err := New(root).Prepare(); err == nil {
		t.Error("a link where the root should be was taken for the root")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("a marker was written where the link led: %v", entries)
	}
}

// Shared Folders are known by name, and several are there at once.
func TestSharedFoldersAreKnownByName(t *testing.T) {
	f := folders(t)
	if names, err := f.Names(); err != nil || len(names) != 0 {
		t.Fatalf("an empty root lists %q (%v)", names, err)
	}
	if names, err := New(filepath.Join(t.TempDir(), "no-root")).Names(); err != nil || len(names) != 0 {
		t.Fatalf("where there is no root, the folders are %q (%v)", names, err)
	}
	for _, name := range []string{"research", "builds"} {
		if err := f.Create(name); err != nil {
			t.Fatal(err)
		}
	}
	if info, err := os.Lstat(f.Path("research")); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("the Shared Folder is %v (%v)", info, err)
	}
	// What macOS leaves in the root is nobody's Shared Folder, and
	// neither is a file by a folder's name.
	for _, name := range []string{".DS_Store", "notes"} {
		if err := os.WriteFile(filepath.Join(f.Root(), name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(f.Root(), "Not A Name"), 0o700); err != nil {
		t.Fatal(err)
	}
	if names, err := f.Names(); err != nil || strings.Join(names, " ") != "builds research" {
		t.Fatalf("the root lists %q (%v), want builds and research", names, err)
	}
	// A folder that is there is found again, with what it holds.
	kept := filepath.Join(f.Path("research"), "work.txt")
	if err := os.WriteFile(kept, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.Create("research"); err != nil {
		t.Errorf("a Shared Folder that is there could not be found again: %v", err)
	}
	if info, err := os.Lstat(kept); err != nil || info.Size() != 4 {
		t.Errorf("finding a Shared Folder again changed what it holds: %v (%v)", info, err)
	}
	// One goes, the other stays.
	if err := f.Remove("research"); err != nil {
		t.Fatal(err)
	}
	if names, err := f.Names(); err != nil || strings.Join(names, " ") != "builds" {
		t.Errorf("after one was removed the root lists %q (%v)", names, err)
	}

	for _, name := range []string{"", "..", "a/b", "Research", ".zeolite-shared-folder-root"} {
		if err := f.Create(name); err == nil {
			t.Errorf("Create took %q for a Shared Folder's name", name)
		}
		if err := f.Remove(name); err == nil {
			t.Errorf("Remove took %q for a Shared Folder's name", name)
		}
	}
}

func TestCreateRefusesALinkWhereTheFolderShouldBe(t *testing.T) {
	f := folders(t)
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, f.Path("research")); err != nil {
		t.Fatal(err)
	}
	if err := f.Create("research"); err == nil {
		t.Error("a link was taken for a Shared Folder")
	}
}

// What is in a Shared Folder is the agents'. Deleting it follows no link
// out of it, and gets through what a build left read-only and what a
// Host Tool left under the flag that forbids deleting it.
func TestRemoveDeletesTheTreeAndFollowsNoLink(t *testing.T) {
	f := folders(t)
	outside := t.TempDir()
	precious := filepath.Join(outside, "precious.txt")
	if err := os.WriteFile(precious, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(outside, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "dir", "inner.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := f.Create("research"); err != nil {
		t.Fatal(err)
	}
	folder := f.Path("research")
	readOnly := filepath.Join(folder, "pkg", "mod")
	if err := os.MkdirAll(readOnly, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(readOnly, "file.go"), []byte("package mod"), 0o400); err != nil {
		t.Fatal(err)
	}
	for _, link := range []struct{ target, name string }{
		{precious, "to-a-file"},
		{filepath.Join(outside, "dir"), "to-a-folder"},
		{"../../..", "upwards"},
		{f.Root(), "to-the-root"},
	} {
		if err := os.Symlink(link.target, filepath.Join(folder, link.name)); err != nil {
			t.Fatal(err)
		}
	}
	// As a Go module cache is left: nothing in it can be deleted as it is.
	for _, dir := range []string{readOnly, filepath.Dir(readOnly)} {
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	// As chflags uchg leaves a file, which a Host Tool may run there.
	kept := filepath.Join(folder, "kept")
	if err := os.WriteFile(kept, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	const immutable = 0x2 // UF_IMMUTABLE
	if err := syscall.Chflags(kept, immutable); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Chflags(kept, 0)
		os.Chmod(filepath.Dir(readOnly), 0o700)
		os.Chmod(readOnly, 0o700)
	})

	if err := f.Remove("research"); err != nil {
		t.Fatalf("removing the Shared Folder: %v", err)
	}
	if _, err := os.Lstat(folder); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the Shared Folder is still there (%v)", err)
	}
	for _, kept := range []string{precious, filepath.Join(outside, "dir", "inner.txt"), filepath.Join(f.Root(), MarkerName)} {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("removing the Shared Folder took %s with it: %v", kept, err)
		}
	}
	if info, err := os.Lstat(filepath.Join(outside, "dir")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("a folder outside the tree was touched: %v (%v)", info, err)
	}
	// And there being none is no error.
	if err := f.Remove("research"); err != nil {
		t.Errorf("removing a Shared Folder that is not there: %v", err)
	}
	if err := New(filepath.Join(t.TempDir(), "no-root")).Remove("research"); err != nil {
		t.Errorf("removing where there is no root: %v", err)
	}
}

// readers are the calls by which a program reads a file's content or a
// link's target, or follows a link to what it names. The package makes,
// lists and deletes: it calls none of them.
var readers = map[string]bool{
	"ReadFile": true, "Open": true, "Readlink": true, "EvalSymlinks": true,
	"Stat": true, "Walk": true, "Glob": true, "Copy": true, "ReadAll": true, "Read": true,
}

// readingCalls are the readers the source of one file calls.
func readingCalls(t *testing.T, name, source string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && readers[sel.Sel.Name] {
				found = append(found, sel.Sel.Name)
			}
		}
		return true
	})
	return found
}

func TestThePackageReadsNothingASandboxWrote(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	files, calls := 0, 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		files++
		calls += strings.Count(string(source), "(")
		for _, call := range readingCalls(t, name, string(source)) {
			t.Errorf("%s calls %s: nothing on the Mac reads what is in a Shared Folder", name, call)
		}
	}
	if files < 1 || calls < 40 {
		t.Fatalf("read %d files with %d calls: the test would prove little", files, calls)
	}
	// The search finds a reader where there is one.
	sample := "package p\nimport \"os\"\nfunc f(root *os.Root) { os.ReadFile(\"a\"); root.Open(\"b\"); os.Stat(\"c\") }\n"
	if got := readingCalls(t, "sample.go", sample); len(got) != 3 {
		t.Errorf("the search found %v in a file that reads three ways", got)
	}
}
