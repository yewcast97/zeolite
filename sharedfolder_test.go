package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"zeolite/internal/pack"
	"zeolite/internal/sandbox"
	"zeolite/internal/sharedfolder"
)

func TestSharedFolderRootFollowsTheDataDirectory(t *testing.T) {
	t.Setenv("ZEOLITE_DATA_DIR", "")
	if got := sharedFolderRoot(); got != sharedfolder.DefaultRoot {
		t.Errorf("the Shared Folders' root is %s, want %s", got, sharedfolder.DefaultRoot)
	}
	// A test run's root is its own, never the user's.
	dir := t.TempDir()
	t.Setenv("ZEOLITE_DATA_DIR", dir)
	if got := sharedFolderRoot(); got != filepath.Join(dir, "shared.noindex") {
		t.Errorf("with ZEOLITE_DATA_DIR set the Shared Folders' root is %s", got)
	}
}

// sharedFolderImport is the package that keeps the Shared Folder on the
// Mac's disk, and sharedFolderRootName what its root is called.
const sharedFolderImport = "zeolite/internal/sharedfolder"

var sharedFolderRootName = filepath.Base(sharedfolder.DefaultRoot)

// namesTheSharedFolder says how a Go file gets at the Shared Folder on
// the Mac's disk: by importing the package that keeps it, or by naming
// its root.
func namesTheSharedFolder(t *testing.T, name string, source []byte) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), name, source, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	var how []string
	for _, imp := range file.Imports {
		if path, _ := strconv.Unquote(imp.Path.Value); path == sharedFolderImport {
			how = append(how, "imports "+sharedFolderImport)
		}
	}
	if strings.Contains(string(source), sharedFolderRootName) {
		how = append(how, "names "+sharedFolderRootName)
	}
	return how
}

// The app reads nothing in a Shared Folder. One package names the root,
// and makes, lists by name and deletes there without opening a file,
// which its own test holds; only this package asks it to, and only to
// pass a folder's path on to Docker and to the Host Tool Wall. No other
// code of the app can find a Shared Folder at all.
func TestOnlyTheAppReachesTheSharedFolder(t *testing.T) {
	files := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// What is built, and the tests against the VM, which use the
			// real folder as the app does.
			if name := d.Name(); path != "." && (strings.HasPrefix(name, ".") || name == "build" || name == "integration") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		dir := filepath.ToSlash(filepath.Dir(path))
		if dir == "." || dir == "internal/sharedfolder" {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		for _, how := range namesTheSharedFolder(t, path, source) {
			t.Errorf("%s %s: only the main package and internal/sharedfolder reach the Shared Folder on this Mac", path, how)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 30 {
		t.Fatalf("read %d files: the test would prove little", files)
	}
	// The search finds both ways in.
	sample := []byte("package p\nimport \"" + sharedFolderImport + "\"\nvar root = \"/Users/Shared/zeolite/" + sharedFolderRootName + "\"\nvar _ = sharedfolder.New\n")
	if got := namesTheSharedFolder(t, "sample.go", sample); len(got) != 2 {
		t.Errorf("the search found %v in a file that reaches the Shared Folder two ways", got)
	}
}

// startsPrograms are the packages that start a program, and what for.
var startsPrograms = map[string]string{
	"cmd/gatekeeper":     "nft, in the gatekeeper's container",
	"cmd/supervisor":     "the packs' daemons, the MCP servers an agent's client asks for and the agent an ACP Bridge's client named, in the workstation",
	"internal/carrier":   "a sandbox's Carrier, under the Carrier Wall",
	"internal/hosttool":  "a sandbox's Host Tools, under the Host Tool Wall",
	"internal/hostwatch": "ipconfig, to read the Wi-Fi network for a Host Condition, and networksetup, to read what the system calls this Mac's interfaces",
}

// networkStackImport is the network stack in which a sandbox's
// connections are ended on this Mac, gVisor's: one package may import
// it. probeImport is the package that makes a probe's request, which
// opens connections: the two that ask have it, the gatekeeper in its
// container and the Host Watchdog on this Mac.
//
// agentFilesImport is the package that deletes what agents wrote on this
// Mac's disk, by a walk that changes modes and flags on its way: the two
// that keep such a tree have it, for a Shared Folder and for a Host Tool
// Scratch.
const (
	networkStackImport = "gvisor.dev/gvisor/"
	probeImport        = "zeolite/internal/probe"
	agentFilesImport   = "zeolite/internal/agentfiles"
)

// sendsProbes are the packages that may import probeImport.
var sendsProbes = map[string]bool{"cmd/gatekeeper": true, "internal/hostwatch": true}

// deletesAgentFiles are the packages that may import agentFilesImport.
var deletesAgentFiles = map[string]bool{"internal/sharedfolder": true, "internal/hosttool": true}

// Only the packages listed start a program. The app starts two kinds
// for a sandbox: its Carrier, under the Carrier Wall, which runs nothing
// of the agent's, and its Host Tools, under the Host Tool Wall; the
// other programs it starts are the system's own, ipconfig and
// networksetup, which the Host Watchdog asks about this Mac. Only the
// Carrier's program holds the network stack, only the gatekeeper and
// the Host Watchdog send probes, and only the Shared Folders' package
// and the Host Tools' delete what agents wrote.
func TestOnlyTheListedPackagesStartProgramsCarryProbeOrDeleteAgentFiles(t *testing.T) {
	found := map[string]bool{}
	files := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != "." && (strings.HasPrefix(name, ".") || name == "build" || name == "integration") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		dir := filepath.ToSlash(filepath.Dir(path))
		file, err := parser.ParseFile(token.NewFileSet(), path, source, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == "os/exec" {
				found[dir] = true
				if _, ok := startsPrograms[dir]; !ok {
					t.Errorf("%s starts a program: nothing but %v does", path, startsPrograms)
				}
			}
			if strings.HasPrefix(p, networkStackImport) {
				found[networkStackImport] = true
				if dir != "cmd/carrier" {
					t.Errorf("%s imports %s: only the Carrier ends a sandbox's connections", path, p)
				}
			}
			if p == probeImport {
				found[probeImport] = true
				if !sendsProbes[dir] {
					t.Errorf("%s imports %s: only the gatekeeper and the Host Watchdog send probes", path, probeImport)
				}
			}
			if p == agentFilesImport {
				found[agentFilesImport+" in "+dir] = true
				if !deletesAgentFiles[dir] {
					t.Errorf("%s imports %s: only a Shared Folder and a Host Tool Scratch are deleted by it", path, agentFilesImport)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 40 {
		t.Fatalf("read %d files: the test would prove little", files)
	}
	for dir := range startsPrograms {
		if !found[dir] {
			t.Errorf("%s starts no program: take it off the list", dir)
		}
	}
	for _, known := range []string{networkStackImport, probeImport} {
		if !found[known] {
			t.Errorf("nothing imports %s: the test is not reading what it should", known)
		}
	}
	for dir := range deletesAgentFiles {
		if !found[agentFilesImport+" in "+dir] {
			t.Errorf("%s does not import %s: take it off the list", dir, agentFilesImport)
		}
	}
}

// An app run with a data directory of its own has a Shared Folders' root
// of its own, which no VM mounts. The VM's mount of the user's root is
// not that app's to call exposed; any other folder still is.
func TestATestRunDoesNotJudgeTheUsersSharedFolderRoot(t *testing.T) {
	users := sharedfolder.DefaultRoot
	exposed := []string{"/Users/someone", users}
	if got := withoutUsersRoot(exposed, "/tmp/test-home/shared.noindex"); len(got) != 1 || got[0] != "/Users/someone" {
		t.Errorf("a test run's app is told that the VM exposes %v", got)
	}
	// The user's own app judges it: a mount at its root that it did not
	// take for the root, by the marker, is exposed.
	if got := withoutUsersRoot(exposed, users); len(got) != 2 {
		t.Errorf("the user's app is told that the VM exposes %v", got)
	}
}

// A Shared Folder is deleted by RemoveSharedFolder, by its name, and
// never while a start that mounts it is under way.
func TestASharedFolderIsNotDeletedWhileItIsMounted(t *testing.T) {
	catalog, err := pack.NewCatalog()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "zeolite", "shared.noindex")
	b := newBackend(t.TempDir(), root, t.TempDir(), catalog, func(sandbox.Update) {})
	if _, err := b.folders.Prepare(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"research", "notes"} {
		if err := b.folders.Create(name); err != nil {
			t.Fatal(err)
		}
	}
	listed := func() string {
		names, err := b.SharedFolders()
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(names, " ")
	}
	if got := listed(); got != "notes research" {
		t.Fatalf("the Shared Folders are %q", got)
	}

	// A sandbox given research is starting.
	b.mounting["research"]++
	if err := b.RemoveSharedFolder("research"); err == nil || !strings.Contains(err.Error(), "a running sandbox mounts it") {
		t.Errorf("deleting a Shared Folder that a start is about to mount: %v", err)
	}
	if err := b.RemoveSharedFolder("notes"); err != nil {
		t.Errorf("deleting another Shared Folder meanwhile: %v", err)
	}
	if got := listed(); got != "research" {
		t.Fatalf("the Shared Folders are %q, want research alone", got)
	}
	b.mounting["research"]--
	if err := b.RemoveSharedFolder("research"); err != nil {
		t.Errorf("deleting a Shared Folder nothing mounts: %v", err)
	}
	if got := listed(); got != "" {
		t.Errorf("after both were deleted the Shared Folders are %q", got)
	}
	// And what is not a name is not a folder to delete.
	if err := b.RemoveSharedFolder("../zeolite"); err == nil {
		t.Error("a path was taken for a Shared Folder's name")
	}
}

// linked returns the folders of the app's own packages that its process
// links: the main package's, and those of every package of the app that
// one of them imports.
func linked(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	var visit func(dir string)
	visit = func(dir string) {
		if seen[dir] {
			return
		}
		seen[dir] = true
		names, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range file.Imports {
				if p, _ := strconv.Unquote(imp.Path.Value); strings.HasPrefix(p, "zeolite/") {
					visit(strings.TrimPrefix(p, "zeolite/"))
				}
			}
		}
	}
	visit(".")
	dirs := make([]string, 0, len(seen))
	for dir := range seen {
		dirs = append(dirs, dir)
	}
	slices.Sort(dirs)
	return dirs
}

// makesSockets reports the names by which a Go file makes a socket of
// its own, takes a connection or looks up a name.
func makesSockets(t *testing.T, name string, source any) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, _ := sel.X.(*ast.Ident)
		if pkg == nil {
			return true
		}
		switch {
		case pkg.Name == "net" && (strings.HasPrefix(sel.Sel.Name, "Dial") || strings.HasPrefix(sel.Sel.Name, "Listen") || strings.HasPrefix(sel.Sel.Name, "Lookup") ||
			strings.HasPrefix(sel.Sel.Name, "File") || sel.Sel.Name == "Resolver" || sel.Sel.Name == "DefaultResolver"):
			found = append(found, "net."+sel.Sel.Name)
		case pkg.Name == "http" && (sel.Sel.Name == "Client" || sel.Sel.Name == "Transport" || sel.Sel.Name == "Server" || strings.HasPrefix(sel.Sel.Name, "Default") ||
			strings.HasPrefix(sel.Sel.Name, "Listen") || strings.HasPrefix(sel.Sel.Name, "Serve") ||
			sel.Sel.Name == "Get" || sel.Sel.Name == "Head" || sel.Sel.Name == "Post" || sel.Sel.Name == "PostForm"):
			found = append(found, "http."+sel.Sel.Name)
		}
		return true
	})
	return found
}

// Of the app's own code that its process links, one package makes a
// socket: internal/probe, whose own test holds that each carries the
// asker's socket option, and which in the app only the Host Watchdog
// uses, with sockets bound to one of this Mac's physical Interfaces. No
// other package dials, listens, looks up a name or holds an HTTP client
// or server. What reaches Docker is Docker's client, on the Colima
// profile's socket; a sandbox's traffic is its Carrier's, in a process
// of its own.
func TestTheAppsProcessMakesSocketsOnlyForProbes(t *testing.T) {
	dirs := linked(t)
	for _, want := range []string{".", "internal/dashboard", "internal/hostwatch", "internal/orchestrator", "internal/probe", "internal/sandbox", "internal/viewport"} {
		if !slices.Contains(dirs, want) {
			t.Fatalf("the app's process is found to link %v, without %s: the test is not reading what it should", dirs, want)
		}
	}
	for _, not := range []string{"cmd/carrier", "cmd/gatekeeper", "integration"} {
		if slices.Contains(dirs, not) {
			t.Fatalf("the app's process is found to link %s", not)
		}
	}
	files, inProbe := 0, 0
	for _, dir := range dirs {
		names, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			files++
			found := makesSockets(t, name, nil)
			if dir == "internal/probe" {
				inProbe += len(found)
				continue
			}
			for _, how := range found {
				t.Errorf("%s uses %s: in the app's process only internal/probe makes a socket", name, how)
			}
		}
	}
	if files < 40 || inProbe < 3 {
		t.Fatalf("read %d files, and found %d such names in internal/probe: the test would prove little", files, inProbe)
	}
	// The search finds each way.
	sample := `package p
import ("net"; "net/http")
func f() {
	net.Dial("tcp", "example.com:443")
	_ = net.Dialer{}
	net.Listen("tcp", ":0")
	net.LookupHost("example.com")
	_ = net.DefaultResolver
	http.Get("https://example.com")
	_ = http.Client{}
	_ = http.DefaultTransport
	http.ListenAndServe(":0", nil)
}`
	got := strings.Join(makesSockets(t, "sample.go", sample), " ")
	for _, want := range []string{"net.Dial", "net.Dialer", "net.Listen", "net.LookupHost", "net.DefaultResolver", "http.Get", "http.Client", "http.DefaultTransport", "http.ListenAndServe"} {
		if !strings.Contains(got, want) {
			t.Errorf("the search missed %s: it found %s", want, got)
		}
	}
}
