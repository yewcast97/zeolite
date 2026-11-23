package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// macMountTypes are the filesystem types by which a VM mounts folders of
// the Mac: virtiofs under Virtualization.framework, 9p and sshfs under
// QEMU.
var macMountTypes = map[string]bool{"virtiofs": true, "9p": true, "fuse.sshfs": true}

// sharedFolderMountType is the one type the Shared Folders' root may be
// mounted by: the one the Shared Folder was measured on, and the one for
// which nothing on the Mac serves the files but the hypervisor.
const sharedFolderMountType = "virtiofs"

// macMount is a folder of the Mac that a VM mounts.
type macMount struct {
	// point is where the VM mounts it, and fstype by what.
	point, fstype string
}

// macMounts reads the mounts of a mount table, in the format of
// /proc/mounts, that are folders of the Mac.
func macMounts(table string) []macMount {
	var mounts []macMount
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && macMountTypes[fields[2]] {
			// The kernel writes a space in a path as \040.
			mounts = append(mounts, macMount{point: strings.ReplaceAll(fields[1], `\040`, " "), fstype: fields[2]})
		}
	}
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].point < mounts[j].point })
	return mounts
}

// SharedFolderRoot is the one folder of the Mac the VM may mount: the
// root that holds the Shared Folder. The app writes a marker file in it,
// by which the VM's mount is known to be that folder.
type SharedFolderRoot struct {
	// Path is the folder on the Mac, which is also where the VM mounts it.
	Path string
	// MarkerName is the marker's file name and MarkerToken its content.
	MarkerName, MarkerToken string
}

// sortMounts tells the mounts that expose the Mac from the one that may
// be the Shared Folders' root: a mount at the root's path, by virtiofs.
// Whether it is that folder is for its marker to say.
func sortMounts(mounts []macMount, rootPath string) (exposed []string, candidate bool) {
	for _, m := range mounts {
		if m.point == rootPath && m.fstype == sharedFolderMountType {
			candidate = true
			continue
		}
		exposed = append(exposed, m.point)
	}
	return exposed, candidate
}

// ExposedMacFolders reports what the VM the Docker daemon runs in sees
// of the Mac: the folders it mounts beyond the Shared Folders' root,
// which anything that got out of a sandbox into the VM could read, and
// whether it mounts that root at all. The VM is meant to mount the root
// or nothing. A mount at the root's path that does not hold the marker
// is some other folder, and is exposed.
func (o *Orchestrator) ExposedMacFolders(ctx context.Context, root SharedFolderRoot) (exposed []string, rootMounted bool, err error) {
	// The VM's mount table is that of its first process.
	table, err := o.runOnce(ctx, []string{"cat", "/vm-mounts"}, mount.Mount{Type: mount.TypeBind, Source: "/proc/1/mounts", Target: "/vm-mounts", ReadOnly: true})
	if err != nil {
		return nil, false, fmt.Errorf("reading the VM's mount table: %w", err)
	}
	if !strings.Contains(table, " / ") {
		return nil, false, fmt.Errorf("reading the VM's mount table: unexpected content %q", firstLine(table))
	}
	exposed, candidate := sortMounts(macMounts(table), root.Path)
	if !candidate {
		return exposed, false, nil
	}
	marker, err := o.runOnce(ctx, []string{"cat", "/shared-folder-root/" + root.MarkerName}, mount.Mount{Type: mount.TypeBind, Source: root.Path, Target: "/shared-folder-root", ReadOnly: true})
	if err == nil && root.MarkerToken != "" && strings.TrimSpace(marker) == root.MarkerToken {
		return exposed, true, nil
	}
	return append(exposed, root.Path), false, nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// runOnce runs a command in a throwaway container that has no network
// and no capabilities, with one mount, and returns what it printed.
func (o *Orchestrator) runOnce(ctx context.Context, command []string, m mount.Mount) (string, error) {
	image, err := o.ensureGatekeeperImage(ctx)
	if err != nil {
		return "", err
	}
	created, err := o.docker.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      image.Tag,
			Entrypoint: command,
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    "none",
			Mounts:         []mount.Mount{m},
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			ReadonlyRootfs: true,
		},
	})
	if err != nil {
		return "", err
	}
	defer o.docker.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})
	if _, err := o.docker.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return "", err
	}
	wait := o.docker.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	var status int64
	select {
	case result := <-wait.Result:
		status = result.StatusCode
	case err := <-wait.Error:
		return "", err
	case <-ctx.Done():
		return "", ctx.Err()
	}
	logs, err := o.docker.ContainerLogs(ctx, created.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return "", err
	}
	defer logs.Close()
	var out, errOut bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &errOut, logs); err != nil {
		return "", err
	}
	if status != 0 {
		return "", fmt.Errorf("%s: %s", strings.Join(command, " "), strings.TrimSpace(errOut.String()))
	}
	return out.String(), nil
}
