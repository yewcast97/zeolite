package hosttool

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"zeolite/internal/sandbox"
)

// Served is the Host Tools of one sandbox, while they are served: from
// its start to its stop. Each request is one call of Run.
type Served struct {
	t        *Tools
	settings Settings
	sc       scratch
	// shared is the Shared Folder as Seatbelt names it.
	shared string
	// slots bounds the runs that go on at once.
	slots chan struct{}

	// ctx is done when the sandbox's Host Tools are served no more:
	// every run ends with it.
	ctx   context.Context
	close context.CancelFunc
	runs  sync.WaitGroup
	once  sync.Once
}

// Serve readies the Host Tools of one sandbox: its Host Tool Scratch,
// and with the GPU the Homebrew Wrappers. Until Close, the sandbox's
// Scratch is not deleted.
func (t *Tools) Serve(s Settings) (*Served, error) {
	sc, err := t.prepare(s.SandboxID)
	if err != nil {
		return nil, err
	}
	// Seatbelt names a folder by the path it resolves to. The Shared
	// Folders' root is resolved, which is the app's; the folder's own
	// name is taken as it is.
	root, err := filepath.EvalSymlinks(filepath.Dir(s.SharedFolder))
	if err != nil {
		return nil, fmt.Errorf("the Shared Folders' root: %w", err)
	}
	if s.GPU {
		// What Homebrew holds changes between two starts of a sandbox.
		if err := t.syncWrappers(); err != nil {
			return nil, fmt.Errorf("the Homebrew Wrappers: %w", err)
		}
	}
	t.mu.Lock()
	t.serving[s.SandboxID]++
	t.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	return &Served{
		t: t, settings: s, sc: sc,
		shared: filepath.Join(root, filepath.Base(s.SharedFolder)),
		slots:  make(chan struct{}, sandbox.HostToolMaxRuns),
		ctx:    ctx, close: cancel,
	}, nil
}

// Run runs one request to its end, or until ctx is done, or until the
// sandbox's Host Tools are served no more, and returns what the program
// printed. A few run at once, and one more is refused.
//
// The request comes from a workstation, which is not trusted: it is
// checked before anything of it is run, and whatever it asks runs under
// the Wall.
func (sv *Served) Run(ctx context.Context, r sandbox.HostToolRequest) sandbox.HostToolResult {
	select {
	case sv.slots <- struct{}{}:
	default:
		return sandbox.HostToolResult{Refused: fmt.Sprintf("%d runs are under way, which is as many as there are at once: wait for one to end", sandbox.HostToolMaxRuns)}
	}
	defer func() { <-sv.slots }()
	sv.runs.Add(1)
	defer sv.runs.Done()
	// The run ends with whichever ends first: its asker, or the serving.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(sv.ctx, cancel)
	defer stop()
	if sv.ctx.Err() != nil {
		return sandbox.HostToolResult{Refused: "no more programs are run on the Mac from here"}
	}
	return sv.t.run(ctx, sv.settings, sv.sc, sv.shared, r)
}

// Close ends every run of the sandbox's Host Tools and waits for them:
// its Scratch may then be deleted.
func (sv *Served) Close() {
	sv.once.Do(func() {
		sv.close()
		sv.runs.Wait()
		sv.t.mu.Lock()
		sv.t.serving[sv.settings.SandboxID]--
		sv.t.mu.Unlock()
	})
}
