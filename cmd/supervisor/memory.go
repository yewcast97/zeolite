package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The supervisor says how much memory the workstation has in use, and
// how much it may have: the kernel's own account of the container, as
// its control group shows it. The sandbox's memory is the whole
// workstation's, whatever in it holds it.

// controlGroup is where a workstation finds its own control group: the
// container's, since a container has a namespace of control groups of
// its own.
const controlGroup = "/sys/fs/cgroup"

// memoryIn reads the memory in use and the limit, in bytes, of the
// control group shown under a folder. In use is what the group is
// charged with less what the kernel holds of files and gives back when
// asked: what programs hold, and /dev/shm, which is charged too. Both
// are zero where either is not known: the folder shows no control group
// of the second version, or the group has no limit.
func memoryIn(dir string) (used, limit uint64) {
	number := func(name string) (uint64, bool) {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return 0, false
		}
		n, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
		return n, err == nil
	}
	current, ok := number("memory.current")
	if !ok {
		return 0, 0
	}
	// A group with no limit says "max", which is no number.
	limit, ok = number("memory.max")
	if !ok || limit == 0 {
		return 0, 0
	}
	stat, err := os.ReadFile(filepath.Join(dir, "memory.stat"))
	if err != nil {
		return 0, 0
	}
	var files uint64
	for _, line := range strings.Split(string(stat), "\n") {
		name, value, found := strings.Cut(line, " ")
		if !found || name != "inactive_file" && name != "active_file" {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return 0, 0
		}
		files += n
	}
	if files > current {
		files = current
	}
	return current - files, limit
}
