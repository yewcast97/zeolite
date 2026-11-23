package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"zeolite/internal/sandbox"
	"zeolite/internal/sandboximage"
)

// fakeDocker answers what a build asks of a Docker daemon, and keeps
// where each build's steps were to run.
type fakeDocker struct {
	server *httptest.Server

	mu sync.Mutex
	// built are the tags that were built, each with the network its
	// build's steps were to run on.
	built map[string]string
}

var apiVersion = regexp.MustCompile(`^/v[0-9.]+`)

func newFakeDocker(t *testing.T) (*fakeDocker, *Orchestrator) {
	t.Helper()
	f := &fakeDocker{built: map[string]string{}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.52")
		w.Header().Set("Content-Type", "application/json")
		path := apiVersion.ReplaceAllString(r.URL.Path, "")
		switch {
		case path == "/_ping":
		case path == "/info":
			json.NewEncoder(w).Encode(map[string]string{"Architecture": "aarch64"})
		case path == "/build":
			io.Copy(io.Discard, r.Body)
			f.mu.Lock()
			f.built[r.URL.Query().Get("t")] = r.URL.Query().Get("networkmode")
			f.mu.Unlock()
			io.WriteString(w, `{"stream":"Step 1/1 : FROM scratch"}`+"\n")
		case strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
			tag := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
			f.mu.Lock()
			_, built := f.built[tag]
			f.mu.Unlock()
			if !built {
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]string{"message": "No such image: " + tag})
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"Id": "sha256:" + strings.Repeat("0", 64)})
		default:
			t.Errorf("the orchestrator asked Docker for %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(f.server.Close)
	o, err := New(context.Background(), "tcp://"+strings.TrimPrefix(f.server.URL, "http://"), func(sandbox.Update) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o.docker.Close() })
	return f, o
}

// networkOf is where the steps of the one build of an image ran, by the
// start of its tag.
func (f *fakeDocker) networkOf(t *testing.T, repository string) (network string, built bool) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for tag, on := range f.built {
		if strings.HasPrefix(tag, repository+":") {
			if built {
				t.Errorf("%s was built twice", repository)
			}
			network, built = on, true
		}
	}
	return network, built
}

// A pod as it is while its sandbox starts, with a gatekeeper that says
// what gate says.
func startingPod(gatekeeper string, gate sandbox.GateTelemetry) *pod {
	return &pod{gatekeeper: gatekeeper, gate: gate, gateSaid: make(chan struct{}), gone: make(chan struct{})}
}

// The steps of the gatekeeper's image run on the VM's own network, which
// the Backstop does not judge, and an image that is there is not built
// again.
func TestTheGatekeepersImageIsBuiltOnTheVMsNetwork(t *testing.T) {
	f, o := newFakeDocker(t)
	for range 2 {
		if _, err := o.ensureGatekeeperImage(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if on, built := f.networkOf(t, "zeolite-gatekeeper"); !built || on != "host" {
		t.Errorf("the gatekeeper's image was built (%v) on %q: its steps run on the VM's own network, which Docker calls host", built, on)
	}
}

// The steps of a workstation's image run in the namespace of the sandbox
// the image is built for, and nowhere else: not on a bridge, and not on
// the VM's network, which is the gatekeeper's image's alone.
func TestAWorkstationsImageIsBuiltInItsSandboxsNamespace(t *testing.T) {
	f, o := newFakeDocker(t)
	image, err := sandboximage.Workstation(o.arch, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := startingPod("0123abcd", sandbox.GateTelemetry{Open: true})
	if err := o.ensureWorkstationImage(context.Background(), context.Background(), p, image); err != nil {
		t.Fatal(err)
	}
	if on, built := f.networkOf(t, "zeolite-workstation"); !built || on != "container:0123abcd" {
		t.Errorf("the workstation's image was built (%v) on %q: its steps run in its sandbox's namespace, container:0123abcd", built, on)
	}
}

// A workstation's image is built by way of its sandbox's gate: while the
// gate is locked nothing is built, and the build begins when it opens.
func TestAWorkstationsImageWaitsForItsSandboxsGate(t *testing.T) {
	f, o := newFakeDocker(t)
	image, err := sandboximage.Workstation(o.arch, nil)
	if err != nil {
		t.Fatal(err)
	}

	// A gatekeeper that stopped opens no gate.
	p := startingPod("0123abcd", sandbox.GateTelemetry{LockReason: sandbox.LockNoInterface})
	close(p.gone)
	if err := o.ensureWorkstationImage(context.Background(), context.Background(), p, image); err == nil {
		t.Error("an image was built for a sandbox whose gatekeeper stopped")
	}
	if _, built := f.networkOf(t, "zeolite-workstation"); built {
		t.Fatal("a workstation's image was built behind a locked gate")
	}

	// One whose gate opens in time is built then.
	p = startingPod("0123abcd", sandbox.GateTelemetry{LockReason: sandbox.LockProbeUnanswered})
	done := make(chan error, 1)
	go func() { done <- o.ensureWorkstationImage(context.Background(), context.Background(), p, image) }()
	select {
	case err := <-done:
		t.Fatalf("the build did not wait for the gate: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, built := f.networkOf(t, "zeolite-workstation"); built {
		t.Fatal("a workstation's image was built behind a locked gate")
	}
	p.mu.Lock()
	said := p.gateSaid
	p.gate, p.gateSaid = sandbox.GateTelemetry{Open: true}, make(chan struct{})
	p.mu.Unlock()
	close(said)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the build did not begin when the gate opened")
	}
	if on, built := f.networkOf(t, "zeolite-workstation"); !built || on != "container:0123abcd" {
		t.Errorf("the workstation's image was built (%v) on %q", built, on)
	}
}

// A gate that does not open in time says why it is locked, with the
// address the probe saw where that is the reason.
func TestAGateThatDoesNotOpenSaysWhy(t *testing.T) {
	for _, c := range []struct {
		gate sandbox.GateTelemetry
		want string
	}{
		{sandbox.GateTelemetry{LockReason: sandbox.LockNoInterface}, "No Interface"},
		{sandbox.GateTelemetry{LockReason: sandbox.LockExitAddressMismatch, ExitAddress: "203.0.113.7"}, "Exit Address Mismatch, seen at 203.0.113.7"},
		{sandbox.GateTelemetry{}, "has not said why"},
	} {
		err := awaitOpenGate(context.Background(), startingPod("0123abcd", c.gate), 20*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("a gate locked for %q that did not open: %v, want it to say %q", c.gate.LockReason, err, c.want)
		}
	}
	if err := awaitOpenGate(context.Background(), startingPod("0123abcd", sandbox.GateTelemetry{Open: true}), time.Millisecond); err != nil {
		t.Errorf("an open gate was waited for: %v", err)
	}
}

// One function asks Docker to build, and every build says where its
// steps run: the VM's network is named for the gatekeeper's image and
// for no other, and nothing is built on a network of Docker's.
func TestEveryBuildSaysWhereItsStepsRun(t *testing.T) {
	builds, onVM, files := 0, 0, 0
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		files++
		data, err := os.ReadFile(filepath.Join(".", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		source := string(data)
		builds += strings.Count(source, "ImageBuild(")
		onVM += strings.Count(source, ", vmNetwork)")
		if strings.Contains(source, "ImageBuildOptions{") && !strings.Contains(source, "NetworkMode: network,") {
			t.Errorf("%s builds an image without saying where its steps run", e.Name())
		}
		for _, call := range regexp.MustCompile(`\.(ensureImage|buildImage)\(([^\n]*)\)`).FindAllStringSubmatch(source, -1) {
			if args := call[2]; !strings.HasSuffix(args, ", vmNetwork") && !strings.HasSuffix(args, ", network") && !strings.HasSuffix(args, ", namespaceOf(p.gatekeeper)") {
				t.Errorf("%s builds an image with %s(%s): its steps run on the VM's network, the gatekeeper's image's, or in a sandbox's namespace", e.Name(), call[1], args)
			}
		}
	}
	if files < 5 || builds != 1 {
		t.Errorf("read %d files in which Docker is asked to build %d times: want once, in buildImage", files, builds)
	}
	if onVM != 1 {
		t.Errorf("%d builds run on the VM's network: want one, the gatekeeper's image's", onVM)
	}
}
