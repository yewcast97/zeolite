package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The memory a workstation has in use is what its control group is
// charged with less what the kernel holds of files; its limit is the
// group's. Where either is not known, as under a control group of the
// first version or with no limit, neither is said: both are zero.
func TestMemoryIn(t *testing.T) {
	const stat = "anon 700\nfile 5000\nkernel 40\nshmem 300\ninactive_file 3000\nactive_file 1500\nunevictable 0\n"
	for _, c := range []struct {
		name        string
		files       map[string]string
		used, limit uint64
	}{
		{"a group with a limit", map[string]string{"memory.current": "6000\n", "memory.max": "8000\n", "memory.stat": stat}, 1500, 8000},
		{"what is held of files is all that is charged", map[string]string{"memory.current": "4000\n", "memory.max": "8000\n", "memory.stat": stat}, 0, 8000},
		{"no files held", map[string]string{"memory.current": "6000\n", "memory.max": "8000\n", "memory.stat": "anon 6000\n"}, 6000, 8000},
		{"a group with no limit", map[string]string{"memory.current": "6000\n", "memory.max": "max\n", "memory.stat": stat}, 0, 0},
		{"a limit of nothing", map[string]string{"memory.current": "6000\n", "memory.max": "0\n", "memory.stat": stat}, 0, 0},
		{"no account of what is in use", map[string]string{"memory.max": "8000\n", "memory.stat": stat}, 0, 0},
		{"no limit to read", map[string]string{"memory.current": "6000\n", "memory.stat": stat}, 0, 0},
		{"no account of files", map[string]string{"memory.current": "6000\n", "memory.max": "8000\n"}, 0, 0},
		{"an account of files that is no number", map[string]string{"memory.current": "6000\n", "memory.max": "8000\n", "memory.stat": "inactive_file many\n"}, 0, 0},
		{"what is in use is no number", map[string]string{"memory.current": "plenty\n", "memory.max": "8000\n", "memory.stat": stat}, 0, 0},
		{"a control group of the first version", map[string]string{"memory/memory.usage_in_bytes": "6000\n", "memory/memory.limit_in_bytes": "8000\n"}, 0, 0},
		{"nothing there", nil, 0, 0},
	} {
		dir := t.TempDir()
		for name, content := range c.files {
			at := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(at, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if used, limit := memoryIn(dir); used != c.used || limit != c.limit {
			t.Errorf("%s: %d in use of %d, want %d of %d", c.name, used, limit, c.used, c.limit)
		}
	}
	if used, limit := memoryIn(filepath.Join(t.TempDir(), "gone")); used != 0 || limit != 0 {
		t.Errorf("a folder that is not there: %d in use of %d", used, limit)
	}
}
