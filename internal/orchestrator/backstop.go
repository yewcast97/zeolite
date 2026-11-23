package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"zeolite/internal/sandbox"
)

// The backstop is a second ruleset, in the VM, that limits what every
// container bridge can reach. Nothing of the app's is on a bridge: a
// sandbox's packets go into its TUN device and from there to its
// Carrier, the steps of a workstation's image are built in such a
// namespace, and those of the gatekeeper's on the VM's own network.
// What is on a bridge has no gatekeeper at all: whatever else the VM's
// Docker is asked to run, and a build step, should Docker ever put one
// there whatever it was asked.
//
// It is loaded by a helper container on the VM's network with NET_ADMIN
// and nothing else, which exists only to run nft there. The table stays
// in the VM's kernel when the helper and the app are gone.

const (
	backstopContainer = "zeolite-backstop"
	backstopTable     = "zeolite_backstop"
)

// backstopRuleset is the table. What comes in from a container bridge is
// judged; everything else passes untouched.
//
//   - No bridge reaches an address that is not public: not the Mac, which
//     the VM reaches at a private address, nor the VM, the LAN or another
//     container. IPv6 is dropped too.
//   - A bridge reaches public IPv4 addresses and the VM's resolver: what
//     an image build needs.
func backstopRuleset() string {
	var nonPublic []string
	for _, p := range sandbox.NonPublicPrefixes {
		if p.Addr().Is4() {
			nonPublic = append(nonPublic, p.String())
		}
	}
	return fmt.Sprintf(`table inet %[1]s { }
delete table inet %[1]s
table inet %[1]s {
  set nonpublic4 { type ipv4_addr; flags interval; elements = { %[2]s } }

  chain bridged_forward {
    meta nfproto ipv6 drop
    ip daddr @nonpublic4 drop
  }
  chain bridged_input {
    meta nfproto ipv6 drop
    ct state established,related accept
    meta l4proto { tcp, udp } th dport 53 accept
    drop
  }
  chain forward {
    type filter hook forward priority -10; policy accept;
    meta iifkind "bridge" jump bridged_forward
  }
  chain input {
    type filter hook input priority -10; policy accept;
    meta iifkind "bridge" jump bridged_input
  }
}
`, backstopTable, strings.Join(nonPublic, ", "))
}

// backstopIdentity is a listing of the table, hashed.
func backstopIdentity(listing string) string {
	sum := sha256.Sum256([]byte(listing))
	return hex.EncodeToString(sum[:])
}

// LoadBackstop rebuilds the table from nothing. No sandbox starts before
// this run of the app has loaded it.
func (o *Orchestrator) LoadBackstop(ctx context.Context) error {
	image, err := o.ensureGatekeeperImage(ctx) // it has nft
	if err != nil {
		return err
	}
	o.docker.ContainerRemove(ctx, backstopContainer, client.ContainerRemoveOptions{Force: true})
	pids := int64(32)
	created, err := o.docker.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: backstopContainer,
		Config: &container.Config{
			Image:      image.Tag,
			Entrypoint: []string{"sleep", "infinity"},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    vmNetwork,
			CapDrop:        []string{"ALL"},
			CapAdd:         []string{"NET_ADMIN"},
			SecurityOpt:    []string{"no-new-privileges"},
			ReadonlyRootfs: true,
			LogConfig:      logConfig,
			Resources:      container.Resources{Memory: 64 << 20, PidsLimit: &pids},
		},
	})
	if err != nil {
		return fmt.Errorf("creating the backstop helper: %w", err)
	}
	if _, err := o.docker.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("starting the backstop helper: %w", err)
	}
	if err := o.backstopNft(ctx, backstopRuleset()); err != nil {
		return fmt.Errorf("loading the backstop: %w", err)
	}
	hash, err := o.backstopHash(ctx)
	if err != nil {
		return fmt.Errorf("loading the backstop: %w", err)
	}
	o.mu.Lock()
	o.backstop = hash
	o.mu.Unlock()
	return nil
}

// verifyBackstop checks, now, that the table in the VM's kernel is the
// one this run loaded. A sandbox is not started behind any other.
func (o *Orchestrator) verifyBackstop(ctx context.Context) error {
	o.mu.Lock()
	loaded := o.backstop
	o.mu.Unlock()
	if loaded == "" {
		return errors.New("the backstop is not loaded")
	}
	live, err := o.backstopHash(ctx)
	if err != nil {
		return fmt.Errorf("the backstop is not in place: %w", err)
	}
	if live != loaded {
		return errors.New("the backstop in the VM is not the one the app loaded")
	}
	return nil
}

// backstopHash identifies the live table.
func (o *Orchestrator) backstopHash(ctx context.Context) (string, error) {
	out, code, err := o.exec(ctx, backstopContainer, "0", nil, "nft", "-s", "list", "table", "inet", backstopTable)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("nft: %s", strings.TrimSpace(out))
	}
	return backstopIdentity(out), nil
}

// removeBackstopHelper removes the helper container. The table stays.
func (o *Orchestrator) removeBackstopHelper(ctx context.Context) {
	o.docker.ContainerRemove(ctx, backstopContainer, client.ContainerRemoveOptions{Force: true})
}

// backstopNft runs an nft script in the VM's network namespace.
func (o *Orchestrator) backstopNft(ctx context.Context, script string) error {
	out, code, err := o.exec(ctx, backstopContainer, "0", []byte(script), "nft", "-f", "-")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("nft: %s", strings.TrimSpace(out))
	}
	return nil
}
