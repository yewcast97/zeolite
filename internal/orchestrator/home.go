package orchestrator

import (
	"context"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"zeolite/internal/sandbox"
)

// A sandbox's Home is a Docker volume in the VM, named for the sandbox's
// ID. Docker makes it when the sandbox's workstation is first created,
// which mounts it at sandbox.HomePath, and it stays when the workstation
// is removed: what the agent's user installed and logged in to is there
// at the next start. Nothing here lists volumes: a Home is deleted by
// its sandbox's ID, in RemoveHome, and nowhere else.

// HomeVolume names the volume that is a sandbox's Home.
func HomeVolume(sandboxID string) string { return "zeolite-" + sandboxID + "-home" }

// RemoveHome deletes a sandbox's Home, and everything in it. It refuses
// while the sandbox is starting or running, and it is no error for there
// to be none.
func (o *Orchestrator) RemoveHome(ctx context.Context, sandboxID string) error {
	if !sandbox.ValidID(sandboxID) {
		return fmt.Errorf("%q is not a sandbox's ID", sandboxID)
	}
	o.mu.Lock()
	_, running := o.running[sandboxID]
	o.mu.Unlock()
	if running {
		return fmt.Errorf("the sandbox is running: stop it before its Home is deleted")
	}
	if _, err := o.docker.VolumeRemove(ctx, HomeVolume(sandboxID), client.VolumeRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("deleting the sandbox's Home: %w", err)
	}
	return nil
}
