// Package orchestrator is everything that talks to Docker: it builds the
// sandbox images, creates and removes a sandbox's containers in the
// order that keeps the gate closed, keeps and deletes its Home, sends
// the heartbeat, tails what the containers report, and opens Terminal
// Sessions.
package orchestrator

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"

	"zeolite/internal/sandbox"
	"zeolite/internal/sandboximage"
)

// Labels on everything the orchestrator creates. By them it finds its
// own containers again; a Home carries its sandbox's, for whoever looks
// for one whose sandbox was lost, and is never found by it here.
const (
	labelSandbox = "zeolite.sandbox" // the sandbox's ID
	labelRole    = "zeolite.role"    // gatekeeper or workstation
	// labelACPBridge is on the workstation of a sandbox that was started
	// with an ACP Bridge.
	labelACPBridge = "zeolite.acp-bridge"
)

// ColimaEndpoint is the Docker endpoint of the Colima profile the app
// uses: a VM of its own, which mounts no folder of the Mac but the
// Shared Folders' root.
func ColimaEndpoint() string {
	home, _ := os.UserHomeDir()
	return "unix://" + filepath.Join(home, ".colima", "zeolite", "docker.sock")
}

// Orchestrator runs sandboxes on one Docker daemon.
type Orchestrator struct {
	docker *client.Client
	arch   string
	// listen is told every Update, from whatever goroutine has one.
	listen func(sandbox.Update)

	mu      sync.Mutex
	running map[string]*pod
	// backstop identifies the backstop this run loaded, empty before it
	// did.
	backstop string

	// suspended is true from the Mac's going to sleep until it wakes.
	suspended atomic.Bool
}

// New connects to the Docker daemon at endpoint. listen is called with
// every Update, from other goroutines.
func New(ctx context.Context, endpoint string, listen func(sandbox.Update)) (*Orchestrator, error) {
	docker, err := client.New(client.WithHost(endpoint))
	if err != nil {
		return nil, err
	}
	info, err := docker.Info(ctx, client.InfoOptions{})
	if err != nil {
		docker.Close()
		return nil, fmt.Errorf("the Docker daemon at %s does not answer: %w", endpoint, err)
	}
	return &Orchestrator{
		docker:  docker,
		arch:    info.Info.Architecture,
		listen:  listen,
		running: map[string]*pod{},
	}, nil
}

// Close stops every sandbox, removes the backstop's helper and closes the
// connection. The backstop itself stays in the VM.
func (o *Orchestrator) Close(ctx context.Context) {
	o.mu.Lock()
	ids := make([]string, 0, len(o.running))
	for id := range o.running {
		ids = append(ids, id)
	}
	o.mu.Unlock()
	for _, id := range ids {
		o.Stop(ctx, id)
	}
	o.removeBackstopHelper(ctx)
	o.docker.Close()
}

// GatekeeperContainer and WorkstationContainer name a sandbox's
// containers.
func GatekeeperContainer(sandboxID string) string  { return "zeolite-" + sandboxID + "-gatekeeper" }
func WorkstationContainer(sandboxID string) string { return "zeolite-" + sandboxID + "-workstation" }

// RemoveOrphans removes what a previous run of the app left behind:
// every container of a sandbox, and with a workstation its /tmp. A
// sandbox whose app died is locked for
// good, its heartbeat stream having ended, and has no way out, its
// Packet stream having ended too: nothing of it is worth keeping.
func (o *Orchestrator) RemoveOrphans(ctx context.Context) error {
	containers, err := o.docker.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: client.Filters{}.Add("label", labelSandbox),
	})
	if err != nil {
		return err
	}
	var errs []error
	// Workstations first: a workstation must never outlive its gatekeeper.
	for _, role := range []string{roleWorkstation, roleGatekeeper} {
		for _, c := range containers.Items {
			if c.Labels[labelRole] != role {
				continue
			}
			if err := o.removeContainer(ctx, c.ID); err != nil && !cerrdefs.IsNotFound(err) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Where the steps of an image's build run, as Docker names it. No step
// runs on a bridge.
//
// The gatekeeper's image is built on the VM's own network: its steps are
// the app's own, the install of two of Debian's packages, and they have
// to run before any sandbox has a namespace, since that image is what
// makes one. What they send leaves as the VM's own traffic does, by no
// Allowed Interface. The image they make runs on that network already,
// as the Backstop's helper, with more than they have.
//
// A workstation's image is built in the namespace of the sandbox it is
// built for: its steps are a Capability Pack's, and whoever wrote the
// packages it installs. What they send goes into the sandbox's TUN
// device and to its Carrier, behind its ruleset and under its lease, or
// nowhere.
const vmNetwork = "host"

// namespaceOf names the network namespace of a container, for what
// joins it.
func namespaceOf(containerID string) string { return "container:" + containerID }

// ensureGatekeeperImage builds the gatekeeper's image unless it is
// there, on the VM's own network, and returns it.
func (o *Orchestrator) ensureGatekeeperImage(ctx context.Context) (sandboximage.Image, error) {
	image, err := sandboximage.Gatekeeper(o.arch)
	if err != nil {
		return sandboximage.Image{}, err
	}
	return image, o.ensureImage(ctx, image, vmNetwork)
}

// ensureImage builds an image unless one with its tag exists. The tag
// names the image's content, so an existing one is the right one. Its
// steps run on network.
func (o *Orchestrator) ensureImage(ctx context.Context, img sandboximage.Image, network string) error {
	exists, err := o.imageExists(ctx, img.Tag)
	if err != nil || exists {
		return err
	}
	return o.buildImage(ctx, img, network)
}

// imageExists is whether Docker has an image under a tag.
func (o *Orchestrator) imageExists(ctx context.Context, tag string) (bool, error) {
	_, err := o.docker.ImageInspect(ctx, tag)
	if cerrdefs.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// buildImage builds an image, with its steps on network: the one place
// that asks Docker to build.
func (o *Orchestrator) buildImage(ctx context.Context, img sandboximage.Image, network string) error {
	res, err := o.docker.ImageBuild(ctx, bytes.NewReader(img.Context), client.ImageBuildOptions{
		Tags:        []string{img.Tag},
		NetworkMode: network,
		Remove:      true,
		ForceRemove: true,
	})
	if err != nil {
		return fmt.Errorf("building %s: %w", img.Tag, err)
	}
	defer res.Body.Close()

	// The daemon reports progress, and failure, as JSON lines.
	var tail []string
	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scanner.Scan() {
		var line struct {
			Stream string `json:"stream"`
			Error  string `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &line) != nil {
			continue
		}
		if line.Error != "" {
			return fmt.Errorf("building %s: %s\n%s", img.Tag, line.Error, strings.Join(tail, ""))
		}
		if s := line.Stream; strings.TrimSpace(s) != "" {
			if tail = append(tail, s); len(tail) > 20 {
				tail = tail[1:]
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("building %s: %w", img.Tag, err)
	}
	if exists, err := o.imageExists(ctx, img.Tag); err != nil || !exists {
		return fmt.Errorf("building %s: the daemon reported no error and made no image", img.Tag)
	}
	return nil
}

// maxExecOutput is the most a command run in a container may print: more
// than any answer the app asks for takes, the largest of which is a
// ruleset as nft lists it.
const maxExecOutput = 1 << 20

// errTooMuchOutput is what a capped buffer answers a write past its cap.
var errTooMuchOutput = errors.New("the command printed more than the app takes")

// cappedBuffer holds what a command printed, up to a cap.
type cappedBuffer struct {
	bytes.Buffer
	cap int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.cap {
		return 0, errTooMuchOutput
	}
	return b.Buffer.Write(p)
}

// exec runs a command in a container, with stdin on its standard input
// when there is any, and returns what it printed and its exit code. What
// it printed on standard error is returned where it printed nothing else
// and failed.
func (o *Orchestrator) exec(ctx context.Context, container, user string, stdin []byte, command ...string) (stdout string, exitCode int, err error) {
	created, err := o.docker.ExecCreate(ctx, container, client.ExecCreateOptions{
		User:         user,
		AttachStdin:  stdin != nil,
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          command,
	})
	if err != nil {
		return "", 0, err
	}
	attached, err := o.docker.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return "", 0, err
	}
	defer attached.Close()
	if stdin != nil {
		go func() {
			attached.Conn.Write(stdin)
			attached.CloseWrite()
		}()
	}
	out, errOut := &cappedBuffer{cap: maxExecOutput}, &cappedBuffer{cap: maxExecOutput}
	copied := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(out, errOut, attached.Reader)
		copied <- err
	}()
	select {
	case err := <-copied:
		if err != nil {
			return "", 0, err
		}
	case <-ctx.Done():
		return "", 0, ctx.Err()
	}
	inspected, err := o.docker.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if err != nil {
		return "", 0, err
	}
	if inspected.ExitCode != 0 && out.Len() == 0 {
		return errOut.String(), inspected.ExitCode, nil
	}
	return out.String(), inspected.ExitCode, nil
}

// tail reads the JSON lines a container writes to its standard output,
// calling line with each, until the container stops or ctx ends.
func (o *Orchestrator) tail(ctx context.Context, container string, line func([]byte)) error {
	logs, err := o.docker.ContainerLogs(ctx, container, client.ContainerLogsOptions{ShowStdout: true, Follow: true})
	if err != nil {
		return err
	}
	defer logs.Close()
	// The stream interleaves standard output and error in frames.
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, io.Discard, logs)
		pw.CloseWithError(err)
	}()
	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line(scanner.Bytes())
	}
	pr.Close()
	return scanner.Err()
}
