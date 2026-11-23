//go:build darwin

// Package agentfiles deletes what agents wrote on this Mac's disk: a
// Shared Folder, a sandbox's Host Tool Scratch.
//
// Such a tree is the agents', links and all, and so is how it was left.
// Nothing here reads a file of it or follows a link in it: the package
// lists folders, changes what forbids deleting them, and deletes by
// names. A test holds it.
package agentfiles

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Remove deletes the tree of a name in a folder of the app's, and
// everything in the tree. It is no error for there to be no such tree,
// or no such folder. Whether the tree may be deleted is for the caller
// to know: this deletes.
//
// The tree is deleted through an os.Root of the folder: a name in it is
// only ever taken beneath the folder, so a link that leads out of the
// tree is deleted, and never followed.
//
// An agent may have left the tree so that it cannot be deleted as it
// is, by a build's habit or to stay: a folder that is read-only, and a
// file, a folder or a link under a flag of this Mac's file system that
// forbids changing it. Those are loosened, and the tree deleted then.
// Only what is on the folder's own volume is loosened: what is mounted
// in the tree is another disk, and nothing of it is the agents' to have
// changed.
func Remove(folder, name string) error {
	if !entryName(name) {
		return fmt.Errorf("%q is not the name of a tree in %s", name, folder)
	}
	root, err := os.OpenRoot(folder)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.RemoveAll(name); err == nil {
		return nil
	}
	if dir, err := root.OpenFile(".", os.O_RDONLY|unix.O_DIRECTORY, 0); err == nil {
		var folder unix.Stat_t
		if unix.Fstat(int(dir.Fd()), &folder) == nil {
			loosen(int(dir.Fd()), name, folder.Dev)
		}
		dir.Close()
	}
	return root.RemoveAll(name)
}

// entryName is whether a name is one entry's in a folder: not empty,
// not the folder itself or the one above it, and with no separator.
func entryName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsRune(name, '/')
}

// forbidsDeleting are the flags a file's owner sets that keep it, and
// what a folder holds, from being changed or deleted: chflags uchg and
// uappnd.
const forbidsDeleting = unix.UF_IMMUTABLE | unix.UF_APPEND

// loosen takes from the entry of a name in a folder, and from all under
// it, what keeps a tree from being deleted: the flags that forbid it,
// and a folder's mode. What cannot be loosened is left: deleting the
// tree then says why it could not.
//
// Every step names an entry by its folder's descriptor and its own name,
// and takes the entry itself where it is a link: nothing outside the
// tree is listed or changed, whatever a link in it leads to. And the
// walk stays on one volume, the one its first folder is on: an entry on
// another is where a disk is mounted, and is left as it is with all
// under it.
func loosen(folder int, name string, volume int32) {
	// A name with a separator in it is a path, and one that begins with
	// a separator is taken from the root of this Mac whatever folder it
	// is given with: neither is ever an entry's name.
	if !entryName(name) {
		return
	}
	var entry unix.Stat_t
	if unix.Fstatat(folder, name, &entry, unix.AT_SYMLINK_NOFOLLOW) != nil || entry.Dev != volume {
		return
	}
	if entry.Flags&forbidsDeleting != 0 {
		setFlags(folder, name, entry.Flags&^forbidsDeleting)
	}
	if entry.Mode&unix.S_IFMT != unix.S_IFDIR {
		return
	}
	unix.Fchmodat(folder, name, 0o700, unix.AT_SYMLINK_NOFOLLOW)
	fd, err := unix.Openat(folder, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	dir := os.NewFile(uintptr(fd), name)
	defer dir.Close()
	names, _ := dir.Readdirnames(-1)
	for _, name := range names {
		loosen(fd, name, volume)
	}
}

// setFlags sets the flags of the entry of a name in a folder: of the
// entry itself where it is a link. A file's flags are otherwise set by
// its path, which follows links on the way, or by its descriptor, which
// a file that may not be read and may not be changed does not give.
func setFlags(folder int, name string, flags uint32) error {
	path, err := unix.BytePtrFromString(name)
	if err != nil {
		return err
	}
	attributes := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_FLAGS}
	_, _, errno := unix.Syscall6(unix.SYS_SETATTRLISTAT, uintptr(folder), uintptr(unsafe.Pointer(path)), uintptr(unsafe.Pointer(&attributes)),
		uintptr(unsafe.Pointer(&flags)), unsafe.Sizeof(flags), unix.FSOPT_NOFOLLOW)
	if errno != 0 {
		return errno
	}
	return nil
}
