// Package sharedfolder keeps the Shared Folders on the Mac's disk: it
// makes the folders that sandboxes' workstations mount, lists them by
// name, and deletes them.
//
// What is in a Shared Folder is the agents'. Nothing here reads it: the
// package makes directories, lists the root to see which folders are
// there, and has a tree deleted as everything agents wrote on this Mac
// is, by names and never following a link out of it. It is the one
// package that names the root, and a test holds both.
package sharedfolder

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"zeolite/internal/agentfiles"
	"zeolite/internal/sandbox"
)

// DefaultRoot holds the Shared Folders. It is outside every home
// directory, so that no path a sandbox sees carries the user's name, and
// its name ends in .noindex, which keeps Spotlight out of what the
// agents wrote. It is the one folder of the Mac the VM mounts.
const DefaultRoot = "/Users/Shared/zeolite/shared.noindex"

// MarkerName is the file in the root by which the VM's mount of that
// path is known to be this folder.
const MarkerName = ".zeolite-shared-folder-root"

// Folders is the root and the Shared Folders in it, each known by its
// name:
//
//	<root>/<name>  a Shared Folder
type Folders struct {
	root string
}

// New returns the folders under root.
func New(root string) *Folders { return &Folders{root: filepath.Clean(root)} }

// Root is the folder the VM mounts.
func (f *Folders) Root() string { return f.root }

// Path is where the Shared Folder of a name is on the Mac.
func (f *Folders) Path(name string) string { return filepath.Join(f.root, name) }

// ownDir makes path a directory of the user's alone. The root lives
// where any user of the Mac may create entries, so one that is already
// there is trusted only if it is a real directory this user owns: never
// a symbolic link, never someone else's.
func ownDir(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("%s belongs to another user", path)
	}
	// Chmod follows a link, and the check above saw a directory of the
	// user's. Nobody else can have put a link in its place since: where
	// any user may create entries, only its owner may replace one.
	return os.Chmod(path, 0o700)
}

// makeRoot makes the root and the folder that holds it, or finds them
// there.
func (f *Folders) makeRoot() error {
	for _, dir := range []string{filepath.Dir(f.root), f.root} {
		if err := ownDir(dir); err != nil {
			return fmt.Errorf("preparing the Shared Folders' root: %w", err)
		}
	}
	return nil
}

// Prepare makes the root, and writes a fresh marker in it. It returns
// the marker's content. It deletes nothing: a Shared Folder is kept
// until the user deletes it.
func (f *Folders) Prepare() (markerToken string, err error) {
	if err := f.makeRoot(); err != nil {
		return "", err
	}
	markerToken = rand.Text()
	// Written beside and renamed over, into a file that must be new: a
	// link put where the marker was is replaced, never written through.
	marker := filepath.Join(f.root, MarkerName)
	if err := os.Remove(marker + ".tmp"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	file, err := os.OpenFile(marker+".tmp", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, err = file.WriteString(markerToken + "\n")
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err := os.Rename(marker+".tmp", marker); err != nil {
		return "", err
	}
	return markerToken, nil
}

// Names lists the Shared Folders on the disk, by name, in order. It is
// no error for there to be no root yet.
func (f *Folders) Names() ([]string, error) {
	entries, err := os.ReadDir(f.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		// Anything else is not the app's: the marker, and what macOS
		// leaves in a folder.
		if e.IsDir() && sandbox.ValidSharedFolderName(e.Name()) {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// Create makes the Shared Folder of a name, or finds it there, and the
// root with it where the root is not there yet.
func (f *Folders) Create(name string) error {
	if !sandbox.ValidSharedFolderName(name) {
		return fmt.Errorf("%q is not a Shared Folder's name", name)
	}
	if err := f.makeRoot(); err != nil {
		return err
	}
	return ownDir(f.Path(name))
}

// Remove deletes the Shared Folder of a name and everything in it. It
// is no error for there to be none. Whether a running sandbox mounts the
// folder is for the caller to know: this deletes.
//
// The tree is the agents', links and all, and so is how it was left: a
// link that leads out of it is deleted and never followed, and what a
// build left read-only, or a Host Tool left under a flag that forbids
// deleting it, is deleted all the same.
func (f *Folders) Remove(name string) error {
	if !sandbox.ValidSharedFolderName(name) {
		return fmt.Errorf("%q is not a Shared Folder's name", name)
	}
	return agentfiles.Remove(f.root, name)
}
