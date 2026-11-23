package orchestrator

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"

	"zeolite/internal/pack"
	"zeolite/internal/sandbox"
)

func TestGateStatus(t *testing.T) {
	tests := []struct {
		tel  sandbox.GateTelemetry
		want string
	}{
		{sandbox.GateTelemetry{Open: true, ExitAddress: "203.0.113.7"}, "GATE OPEN [203.0.113.7]"},
		{sandbox.GateTelemetry{LockReason: sandbox.LockExitAddressMismatch, ExitAddress: "198.51.100.9"}, "GATE LOCKED [Exit Address Mismatch]"},
		{sandbox.GateTelemetry{LockReason: sandbox.LockProbeUnanswered}, "GATE LOCKED [Probe Unanswered]"},
		{sandbox.GateTelemetry{LockReason: sandbox.LockHostCondition}, "GATE LOCKED [Host Condition]"},
	}
	for _, tt := range tests {
		if got := gateStatus(tt.tel).String(); got != tt.want {
			t.Errorf("%+v: the status reads %q, want %q", tt.tel, got, tt.want)
		}
	}
	// The address is shown for a mismatch only: after a probe went
	// unanswered, the last address read says nothing about now.
	if s := gateStatus(sandbox.GateTelemetry{LockReason: sandbox.LockProbeUnanswered, ExitAddress: "203.0.113.7"}); s.ExitAddress != "" {
		t.Errorf("the status of an unanswered probe carries the address %q", s.ExitAddress)
	}
}

func TestBackstopRuleset(t *testing.T) {
	rules := backstopRuleset()
	for _, want := range []string{
		// Reloading replaces the table instead of adding to it.
		"table inet zeolite_backstop { }\ndelete table inet zeolite_backstop\n",
		// Every container bridge is judged, whatever its name or subnet.
		"type filter hook forward priority -10; policy accept;\n    meta iifkind \"bridge\" jump bridged_forward",
		"type filter hook input priority -10; policy accept;\n    meta iifkind \"bridge\" jump bridged_input",
		"192.168.0.0/16", "10.0.0.0/8", "169.254.0.0/16", "100.64.0.0/10", "172.16.0.0/12",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("the backstop lacks %q", want)
		}
	}
	chain := func(name string) []string {
		start := strings.Index(rules, "chain "+name+" {")
		if start < 0 {
			t.Fatalf("the backstop has no chain %s", name)
		}
		body := rules[start:]
		body = body[strings.Index(body, "\n")+1 : strings.Index(body, "\n  }")]
		var lines []string
		for _, l := range strings.Split(body, "\n") {
			lines = append(lines, strings.TrimSpace(l))
		}
		return lines
	}
	// What a bridge sends through the VM: no IPv6, and nothing to an
	// address that is not public. No sandbox is on a bridge, and no rule
	// speaks of one.
	if got, want := chain("bridged_forward"), []string{
		"meta nfproto ipv6 drop",
		"ip daddr @nonpublic4 drop",
	}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("bridged_forward is\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// What a bridge sends to the VM itself: DNS queries, and replies to
	// what the VM began.
	if got, want := chain("bridged_input"), []string{
		"meta nfproto ipv6 drop",
		"ct state established,related accept",
		"meta l4proto { tcp, udp } th dport 53 accept",
		"drop",
	}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("bridged_input is\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestBackstopIdentity(t *testing.T) {
	listing := "table inet zeolite_backstop {\n\tset nonpublic4 {\n\t\ttype ipv4_addr\n\t\tflags interval\n\t\telements = { 10.0.0.0/8, 127.0.0.0/8 }\n\t}\n\n\tchain bridged_forward {\n\t\tip daddr @nonpublic4 drop\n\t}\n}\n"
	if backstopIdentity(listing) != backstopIdentity(listing) || len(backstopIdentity(listing)) != 64 {
		t.Error("a listing has no one identity")
	}
	// A rule that changed, or a non-public range that went missing, is
	// another backstop.
	for what, tampered := range map[string]string{
		"an added rule":   strings.Replace(listing, "nonpublic4 drop\n", "nonpublic4 drop\n\t\taccept\n", 1),
		"a missing range": strings.Replace(listing, "10.0.0.0/8, ", "", 1),
	} {
		if tampered == listing || backstopIdentity(tampered) == backstopIdentity(listing) {
			t.Errorf("%s leaves the backstop's identity unchanged", what)
		}
	}
}

func TestNames(t *testing.T) {
	if GatekeeperContainer("s1") != "zeolite-s1-gatekeeper" || WorkstationContainer("s1") != "zeolite-s1-workstation" {
		t.Error("the names of a sandbox's containers changed")
	}
	// A Home is found again by its name: a changed name would leave
	// every Home behind.
	if HomeVolume("s1") != "zeolite-s1-home" {
		t.Error("the name of a sandbox's Home changed")
	}
}

func TestMacMounts(t *testing.T) {
	table := `sysfs /sys sysfs rw,nosuid,nodev,noexec,relatime 0 0
/dev/vda1 / ext4 rw,relatime,discard,errors=remount-ro 0 0
tmpfs /run tmpfs rw,nosuid,nodev,size=1218992k,nr_inodes=819200,mode=755 0 0
lima-ecd81dc6b8fa9b44 /Users/Shared/work virtiofs rw,relatime 0 0
mount0 /Users/someone 9p rw,relatime 0 0
:/Users/someone/My\040Files /Users/someone/My\040Files fuse.sshfs rw,nosuid,nodev 0 0
/dev/vdb1 /mnt/lima-colima ext4 rw,relatime 0 0
`
	mounts := macMounts(table)
	var got []string
	for _, m := range mounts {
		got = append(got, m.point+" by "+m.fstype)
	}
	want := []string{"/Users/Shared/work by virtiofs", "/Users/someone by 9p", "/Users/someone/My Files by fuse.sshfs"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("the Mac's folders in the mount table are %q, want %q", got, want)
	}
	if got := macMounts("/dev/vda1 / ext4 rw 0 0\n"); len(got) != 0 {
		t.Errorf("a VM that mounts nothing of the Mac shows %q", got)
	}

	// The VM may mount the Shared Folders' root, by virtiofs, and nothing
	// else of the Mac.
	root := "/Users/Shared/zeolite/shared.noindex"
	for _, c := range []struct {
		what      string
		mounts    []macMount
		exposed   string
		candidate bool
	}{
		{"a VM that mounts nothing", nil, "", false},
		{"the root alone", []macMount{{root, "virtiofs"}}, "", true},
		{"the root and a home", []macMount{{root, "virtiofs"}, {"/Users/someone", "virtiofs"}}, "/Users/someone", true},
		{"the folder above the root", []macMount{{"/Users/Shared/zeolite", "virtiofs"}}, "/Users/Shared/zeolite", false},
		{"a folder under the root", []macMount{{root + "/s1", "virtiofs"}}, root + "/s1", false},
		{"the root by sshfs", []macMount{{root, "fuse.sshfs"}}, root, false},
		{"the root by 9p", []macMount{{root, "9p"}}, root, false},
	} {
		exposed, candidate := sortMounts(c.mounts, root)
		if strings.Join(exposed, "|") != c.exposed || candidate != c.candidate {
			t.Errorf("%s: exposed %q, the root's mount %v; want %q, %v", c.what, exposed, candidate, c.exposed, c.candidate)
		}
	}
}

func TestWorkstationMounts(t *testing.T) {
	packs := []pack.Manifest{{RuntimeLayer: pack.RuntimeLayer{Volumes: []pack.Volume{{Name: "models", Path: "/opt/models"}}}}}
	// The Home comes first, whatever else is mounted: the sandbox's own
	// volume, at the home directory, written to, and labelled as the
	// sandbox's when Docker makes it.
	home := func(m mount.Mount) bool {
		return m.Type == mount.TypeVolume && m.Source == "zeolite-s1-home" && m.Target == "/workspace" && !m.ReadOnly &&
			m.VolumeOptions != nil && m.VolumeOptions.Labels[labelSandbox] == "s1" && len(m.VolumeOptions.Labels) == 1
	}
	// /tmp comes next: a volume with no name, which Docker therefore
	// makes with the workstation and removes with it, written to, and
	// labelled as the sandbox's. It is no memory: nothing is a tmpfs.
	tmp := func(m mount.Mount) bool {
		return m.Type == mount.TypeVolume && m.Source == "" && m.Target == "/tmp" && !m.ReadOnly &&
			m.VolumeOptions != nil && m.VolumeOptions.Labels[labelSandbox] == "s1" && len(m.VolumeOptions.Labels) == 1
	}
	if got := workstationMounts("s1", nil, ""); len(got) != 2 || !home(got[0]) || !tmp(got[1]) {
		t.Errorf("a sandbox with no pack and no Shared Folder mounts %+v, want its Home and its /tmp", got)
	}
	got := workstationMounts("s1", packs, "/Users/Shared/zeolite/shared.noindex/research")
	if len(got) != 4 {
		t.Fatalf("mounts %v, want the Home, /tmp, a pack's volume and the Shared Folder", got)
	}
	if !home(got[0]) {
		t.Errorf("the Home is mounted as %+v", got[0])
	}
	if !tmp(got[1]) {
		t.Errorf("/tmp is mounted as %+v", got[1])
	}
	if v := got[2]; v.Type != mount.TypeVolume || v.Source != "models" || v.Target != "/opt/models" || !v.ReadOnly {
		t.Errorf("a pack's volume is mounted as %+v", v)
	}
	if b := got[3]; b.Type != mount.TypeBind || b.Source != "/Users/Shared/zeolite/shared.noindex/research" || b.Target != sandbox.SharedFolderPath || b.ReadOnly {
		t.Errorf("the Shared Folder is mounted as %+v", b)
	}
	for _, m := range got {
		if m.Type == mount.TypeTmpfs {
			t.Errorf("%s is memory", m.Target)
		}
	}
	if got := workstationMounts("s1", packs, ""); len(got) != 3 || !home(got[0]) || !tmp(got[1]) || got[2].Source != "models" {
		t.Errorf("a sandbox with no Shared Folder mounts %v", got)
	}
	// Another sandbox mounts another Home.
	if other := workstationMounts("s2", nil, ""); other[0].Source == got[0].Source {
		t.Error("two sandboxes mount one Home")
	}
}

// A Home is not deleted under a sandbox that is starting or runs: the
// refusal comes before anything is asked of Docker, which this
// orchestrator has none of.
func TestAHomeIsNotDeletedWhileItsSandboxRuns(t *testing.T) {
	o := &Orchestrator{running: map[string]*pod{"s1": {}}}
	if err := o.RemoveHome(context.Background(), "s1"); err == nil || !strings.Contains(err.Error(), "running") {
		t.Errorf("deleting the Home of a sandbox that runs: %v", err)
	}
	for _, id := range []string{"", "../s1", "S1", "s1/x", "s1 "} {
		if err := o.RemoveHome(context.Background(), id); err == nil || !strings.Contains(err.Error(), "not a sandbox's ID") {
			t.Errorf("deleting the Home of %q: %v", id, err)
		}
	}
}

// A Home is deleted in one place, by its sandbox's ID. Nothing in the
// app removes a volume by its name otherwise, prunes volumes or lists
// them: a failed read of the configs, or a crash, must not be able to
// cost every Home.
//
// One function has Docker remove a container's volumes with it, the one
// that removes a sandbox's container: Docker removes with a container
// the volumes that have no name, which is a workstation's /tmp, and
// never one that has a name, which a Home has.
func TestAHomeIsDeletedInOnePlace(t *testing.T) {
	root := filepath.Join("..", "..")
	removes, withContainer, files := 0, 0, 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "build" || name == "integration") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		source := string(data)
		rel, _ := filepath.Rel(root, path)
		for _, forbidden := range []string{"VolumesPrune", "VolumePrune", "VolumeList"} {
			if strings.Contains(source, forbidden) {
				t.Errorf("%s uses %s: Homes are never listed or pruned", rel, forbidden)
			}
		}
		if n := strings.Count(source, "RemoveVolumes"); n > 0 {
			withContainer += n
			if filepath.ToSlash(rel) != "internal/orchestrator/pod.go" {
				t.Errorf("%s has Docker remove a container's volumes: only removeContainer does", rel)
			}
		}
		if n := strings.Count(source, "VolumeRemove("); n > 0 {
			removes += n
			if filepath.ToSlash(rel) != "internal/orchestrator/home.go" {
				t.Errorf("%s removes a volume: only RemoveHome does", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 40 || removes != 1 {
		t.Errorf("read %d files with %d removals of a volume: want one, in RemoveHome", files, removes)
	}
	if withContainer != 1 {
		t.Errorf("%d places have Docker remove a container's volumes: want one, in removeContainer", withContainer)
	}
}

// A workstation's Home has a name, and its /tmp has none: that is all
// that keeps the one and removes the other when the workstation goes.
func TestOnlyTmpIsRemovedWithItsWorkstation(t *testing.T) {
	for _, m := range workstationMounts("s1", []pack.Manifest{{RuntimeLayer: pack.RuntimeLayer{Volumes: []pack.Volume{{Name: "models", Path: "/opt/models"}}}}}, "/shared") {
		if m.Type != mount.TypeVolume {
			continue
		}
		if named := m.Source != ""; named == (m.Target == sandbox.TmpPath) {
			t.Errorf("the volume at %s has the name %q: a volume with no name is removed with the workstation, and only /tmp is to be", m.Target, m.Source)
		}
	}
}

// What a command run in a container prints is taken up to a cap, and
// no further.
func TestExecOutputIsCapped(t *testing.T) {
	b := &cappedBuffer{cap: 8}
	if _, err := b.Write([]byte("12345678")); err != nil {
		t.Error(err)
	}
	if _, err := b.Write([]byte("9")); err == nil {
		t.Error("a write past the cap was taken")
	}
	if b.String() != "12345678" {
		t.Errorf("the buffer holds %q", b.String())
	}
}

// A Shared Folder is mounted while a sandbox given it is starting or
// running, whichever sandbox that is, and by its path alone.
func TestSharedFolderMounted(t *testing.T) {
	research, notes := "/Users/Shared/zeolite/shared.noindex/research", "/Users/Shared/zeolite/shared.noindex/notes"
	o := &Orchestrator{running: map[string]*pod{
		"s1": {sharedFolder: research},
		"s2": {sharedFolder: research},
		"s3": {},
	}}
	if !o.SharedFolderMounted(research) {
		t.Error("a folder two sandboxes mount is not said to be mounted")
	}
	if o.SharedFolderMounted(notes) || o.SharedFolderMounted("") {
		t.Error("a folder no sandbox mounts is said to be mounted")
	}
	delete(o.running, "s1")
	if !o.SharedFolderMounted(research) {
		t.Error("a folder is said to be free while one of its sandboxes still runs")
	}
	delete(o.running, "s2")
	if o.SharedFolderMounted(research) {
		t.Error("a folder is said to be mounted after its last sandbox stopped")
	}
}

// A Terminal Session is a login shell, recorded by the supervisor under
// the session's ID: the command names that ID and no program. And a
// sandbox that does not run has no workstation to open one in.
func TestSessionCommand(t *testing.T) {
	if got := strings.Join(sessionCommand("abc"), " "); got != "/usr/local/bin/supervisor shell abc" {
		t.Errorf("a session runs %q", got)
	}
	o := &Orchestrator{running: map[string]*pod{"starting": {}}}
	for _, id := range []string{"starting", "gone"} {
		if _, err := o.OpenTerminalSession(context.Background(), id); err == nil || !strings.Contains(err.Error(), "not running") {
			t.Errorf("a Terminal Session in the sandbox %s: %v", id, err)
		}
	}
}

// What a workstation says of a Terminal Session is taken only for one
// the app opened in that sandbox and has not closed.
func TestHasSession(t *testing.T) {
	one, other := &pod{sessions: map[string]bool{"aaa": true}}, &pod{}
	o := &Orchestrator{running: map[string]*pod{"s1": one, "s2": other}}
	for _, c := range []struct {
		sandbox, session string
		want             bool
	}{
		{"s1", "aaa", true}, {"s1", "bbb", false}, {"s1", "", false},
		{"s2", "aaa", false}, {"gone", "aaa", false},
	} {
		if got := o.HasSession(c.sandbox, c.session); got != c.want {
			t.Errorf("the session %q of %s is the app's: %v, want %v", c.session, c.sandbox, got, c.want)
		}
	}
	// Closed, it is the app's no more.
	session := &TerminalSession{o: o, pod: one, id: "aaa"}
	one.mu.Lock()
	delete(one.sessions, session.id)
	one.mu.Unlock()
	if o.HasSession("s1", "aaa") {
		t.Error("a session that was closed is still taken for the app's")
	}
}

// A heartbeat tells the gatekeeper this Mac's physical exit addresses only
// while the sandbox leaves by an Interface that expects those, or any exit
// address but those: for no other Interface is the Mac asked for them.
// Where any but those is expected, the Interface in use is the one that is
// not counted among this Mac's physical Interfaces; where those are, every
// one is counted.
func TestHeartbeatNamesThisMacsPhysicalExitAddresses(t *testing.T) {
	o := &Orchestrator{}
	p := &pod{cfg: sandbox.Config{AllowedInterfaces: []sandbox.AllowedInterface{
		{Name: "utun9", AnyExitAddressButPhysical: true},
		{Name: "utun4", AnyExitAddress: true},
		{Name: "en0", ExpectedExitAddresses: []string{"203.0.113.7"}},
		{Name: "en5", PhysicalExitAddress: true},
	}}}
	var asked []string
	physical, known := []string{"203.0.113.7"}, true
	opts := StartOptions{
		ConditionsMet: func() bool { return true },
		PhysicalExits: func(inUse string) ([]string, bool) {
			asked = append(asked, inUse)
			return physical, known
		},
	}
	use := func(name string, change uint64) {
		p.mu.Lock()
		p.inUse = sandbox.CarrierReport{Interface: name, InterfaceChange: change}
		p.mu.Unlock()
	}

	// No Interface in use yet: nothing is asked, and nothing is known.
	if h := o.heartbeat(p, opts); !h.HostConditionsMet || h.Interface != "" || h.PhysicalExitKnown || h.PhysicalExitAddresses != nil || len(asked) != 0 {
		t.Fatalf("with no Interface in use the heartbeat is %+v, after asking for %v", h, asked)
	}
	// The Interface that expects them: asked, by its name.
	use("utun9", 1)
	if h := o.heartbeat(p, opts); h.Interface != "utun9" || h.InterfaceChange != 1 || !h.PhysicalExitKnown || strings.Join(h.PhysicalExitAddresses, " ") != "203.0.113.7" || strings.Join(asked, " ") != "utun9" {
		t.Fatalf("by utun9 the heartbeat is %+v, after asking for %v", h, asked)
	}
	physical, known = nil, false
	if h := o.heartbeat(p, opts); h.PhysicalExitKnown || h.PhysicalExitAddresses != nil {
		t.Errorf("with them unknown the heartbeat is %+v", h)
	}
	// Known, with none: this Mac has no other way out.
	known = true
	if h := o.heartbeat(p, opts); !h.PhysicalExitKnown || len(h.PhysicalExitAddresses) != 0 {
		t.Errorf("known with none, the heartbeat is %+v", h)
	}
	// Any other Interface in use: not asked.
	asked = nil
	for i, name := range []string{"utun4", "en0", "en9"} {
		use(name, uint64(i+2))
		if h := o.heartbeat(p, opts); h.Interface != name || h.PhysicalExitKnown || h.PhysicalExitAddresses != nil || len(asked) != 0 {
			t.Errorf("by %s the heartbeat is %+v, after asking for %v", name, h, asked)
		}
	}

	// The Interface that expects this Mac's physical exit
	// address: asked, with no interface left out, itself among
	// them.
	use("en5", 5)
	if h := o.heartbeat(p, opts); h.Interface != "en5" || !h.PhysicalExitKnown || len(asked) != 1 || asked[0] != "" {
		t.Errorf("by en5 the heartbeat is %+v, after asking with %q left out", h, asked)
	}
	physical = []string{"203.0.113.7"}
	if h := o.heartbeat(p, opts); strings.Join(h.PhysicalExitAddresses, " ") != "203.0.113.7" {
		t.Errorf("by en5 the heartbeat names %v", h.PhysicalExitAddresses)
	}
	physical = nil

	// Asleep, nothing passes, whatever is known.
	use("utun9", 6)
	o.suspended.Store(true)
	if h := o.heartbeat(p, opts); h.HostConditionsMet {
		t.Errorf("while the Mac sleeps the heartbeat is %+v", h)
	}
	o.suspended.Store(false)
	// A Host Condition unmet is said as before.
	opts.ConditionsMet = func() bool { return false }
	if h := o.heartbeat(p, opts); h.HostConditionsMet || !h.PhysicalExitKnown {
		t.Errorf("with a Host Condition unmet the heartbeat is %+v", h)
	}
	// With nobody to ask, they are not known: the gate that needs them
	// stays locked.
	if h := o.heartbeat(p, StartOptions{}); !h.HostConditionsMet || h.PhysicalExitKnown || h.Interface != "utun9" {
		t.Errorf("with nobody to ask the heartbeat is %+v", h)
	}
}

// BeatNow asks every running sandbox for a heartbeat at once, and waits
// for none of them: one that is already asked is asked once.
func TestBeatNow(t *testing.T) {
	// One that is starting has no gatekeeper yet to ask.
	gated := func() *pod { return &pod{beatNow: make(chan struct{}, 1)} }
	a, b, starting := gated(), gated(), &pod{}
	o := &Orchestrator{running: map[string]*pod{"a": a, "b": b, "starting": starting}}
	o.BeatNow()
	o.BeatNow()
	for name, p := range map[string]*pod{"a": a, "b": b} {
		if len(p.beatNow) != 1 {
			t.Errorf("sandbox %s is asked for %d heartbeats", name, len(p.beatNow))
		}
	}
	// Going to sleep and waking ask for one too.
	<-a.beatNow
	o.SetSuspended(true)
	if len(a.beatNow) != 1 || !o.suspended.Load() {
		t.Errorf("going to sleep asked for %d heartbeats, suspended %v", len(a.beatNow), o.suspended.Load())
	}
	<-a.beatNow
	o.SetSuspended(false)
	if len(a.beatNow) != 1 || o.suspended.Load() {
		t.Errorf("waking asked for %d heartbeats, suspended %v", len(a.beatNow), o.suspended.Load())
	}
}
