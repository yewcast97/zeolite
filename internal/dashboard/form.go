package dashboard

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"

	"zeolite/internal/pack"
	"zeolite/internal/sandbox"
)

// What the form calls the choices of a config. The core's own names are
// identifiers; these are for reading.
var conditionLabels = map[sandbox.HostConditionKind]string{
	sandbox.WiFiSSID: "Wi-Fi network is",
}

// timezoneOfExit is what the form calls the timezone that is taken from
// the exit address, the first of the timezones it offers.
const timezoneOfExit = "That of the exit address"

// timezoneChoices are the timezones the form offers a sandbox that holds
// this one: that of the exit address, the core's, and the one held where
// it is none of them, so that it shows as what it is.
func timezoneChoices(held string) []string {
	choices := append([]string{timezoneOfExit}, sandbox.Timezones...)
	if held != "" && !slices.Contains(choices, held) {
		choices = append(choices, held)
	}
	return choices
}

// What a row of the Allowed Interfaces offers to expect of the exit
// address there, in the order it offers them.
const (
	expectListed         = "these addresses"
	expectAny            = "any"
	expectAnyButPhysical = "any but physical"
	expectPhysical       = "physical"
)

var expectations = []string{expectListed, expectAny, expectAnyButPhysical, expectPhysical}

// draft is a config as the form edits it: the text of each field, kept
// as typed until Save makes a config of it.
type draft struct {
	name string

	// interfaces are the rows of the Allowed Interfaces, most preferred
	// first, and nextInterface the ID the next row gets.
	interfaces    []*interfaceDraft
	nextInterface int
	probeURLs     string // one per line
	probePin      string
	resolver      string
	// hold is whether the sandbox's gate keeps a Hold.
	hold       bool
	conditions []*conditionDraft

	// timezone is the timezone chosen, as the form offers it.
	timezone         string
	timezoneProbeURL string

	cpus, memoryMB, shmMB float64

	packs []*packDraft
	// folderChoice is what the choice of the sandbox's Shared Folder
	// shows: a folder's name, or that it is given none, or that a new
	// one is named; and newFolder the name typed for a new one.
	folderChoice, newFolder string
	// hostTools is whether the sandbox is given Host Tools, and hostGPU
	// and hostMacName what is added to their Wall. They are kept as set
	// while the sandbox is given no Shared Folder, and so has none.
	hostTools, hostGPU, hostMacName bool
	// applications are the rows of the sandbox's Allowed Applications,
	// kept in the same way.
	applications []*applicationDraft
}

// interfaceDraft is one row of the Allowed Interfaces. Its ID is its
// own for as long as the form lives: a row that is moved keeps what was
// typed in it.
type interfaceDraft struct {
	id   int
	name string
	// choice is what the row's choice among this Mac's ways out shows:
	// the label of the one named, as the form last built it.
	choice string
	// expect is which of the expectations the row has.
	expect string
	// exits are the expected exit addresses, apart by commas or spaces.
	// They are kept as typed whatever the row expects, and are the
	// Interface's only while it expects these addresses.
	exits string
	// typingExit is whether the last of them was being typed when the
	// form was last built: one that is taken then stands in its place.
	// exitTaken is whether one was taken then: the caret goes after it.
	typingExit, exitTaken bool
	// standIns is whether names are answered with stand-in addresses
	// there.
	standIns bool
}

// addInterface adds a row to the Allowed Interfaces, the least preferred.
func (d *draft) addInterface(ifc sandbox.AllowedInterface) {
	d.nextInterface++
	expect := expectListed
	switch {
	case ifc.PhysicalExitAddress:
		expect = expectPhysical
	case ifc.AnyExitAddressButPhysical:
		expect = expectAnyButPhysical
	case ifc.AnyExitAddress:
		expect = expectAny
	}
	d.interfaces = append(d.interfaces, &interfaceDraft{
		id:       d.nextInterface,
		name:     ifc.Name,
		expect:   expect,
		exits:    strings.Join(ifc.ExpectedExitAddresses, ", "),
		standIns: ifc.StandInAddresses,
	})
}

type conditionDraft struct {
	kind  string
	value string
}

// applicationDraft is one row of the Allowed Applications: the name of
// a folder of /Applications.
type applicationDraft struct {
	name string
}

type packDraft struct {
	manifest pack.Manifest
	enabled  bool
}

func newDraft(cfg sandbox.Config, packs []pack.Manifest) *draft {
	d := &draft{
		name:             cfg.Name,
		probeURLs:        strings.Join(cfg.Gate.ProbeURLs, "\n"),
		probePin:         cfg.Gate.ProbePin,
		resolver:         cfg.Gate.Resolver,
		hold:             cfg.Gate.Hold,
		timezone:         cfg.Clock.Timezone,
		timezoneProbeURL: cfg.Clock.TimezoneProbeURL,
		cpus:             cfg.Resources.CPUs,
		memoryMB:         float64(cfg.Resources.MemoryMB),
		shmMB:            float64(cfg.Resources.ShmMB),
		folderChoice:     cfg.SharedFolder,
		hostTools:        cfg.HostTools.Enabled,
		hostGPU:          cfg.HostTools.GPU,
		hostMacName:      cfg.HostTools.MacName,
	}
	if cfg.Clock.Timezone == sandbox.TimezoneAuto {
		d.timezone = timezoneOfExit
	}
	if cfg.SharedFolder == "" {
		d.folderChoice = sharedFolderNone
	}
	for _, ifc := range cfg.AllowedInterfaces {
		d.addInterface(ifc)
	}
	// A sandbox with none yet has a row to choose the first one in.
	if len(d.interfaces) == 0 {
		d.addInterface(sandbox.AllowedInterface{})
	}
	for _, h := range cfg.HostConditions {
		d.conditions = append(d.conditions, &conditionDraft{kind: conditionLabels[h.Kind], value: h.Value})
	}
	for _, name := range cfg.HostTools.Applications {
		d.applications = append(d.applications, &applicationDraft{name: name})
	}
	enabled := map[string]bool{}
	for _, name := range cfg.Packs {
		enabled[name] = true
	}
	for _, m := range packs {
		d.packs = append(d.packs, &packDraft{manifest: m, enabled: enabled[m.Name]})
	}
	return d
}

// lines splits a field of one value per line.
func lines(s string) []string {
	var out []string
	for _, l := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ',' }) {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// labelsOf lists the labels of choices, in the order the core lists the
// choices.
func labelsOf[K comparable](keys []K, labels map[K]string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = labels[k]
	}
	return out
}

// keyOf finds the key a label stands for.
func keyOf[K comparable](labels map[K]string, label string) K {
	for k, l := range labels {
		if l == label {
			return k
		}
	}
	var zero K
	return zero
}

// config makes a config of the draft, keeping the identity of base. It
// returns what could not be read as typed; the config's own validation
// comes after.
func (d *draft) config(base sandbox.Config) (sandbox.Config, []sandbox.FieldError) {
	var errs []sandbox.FieldError
	cfg := base
	cfg.Name = strings.TrimSpace(d.name)
	cfg.AllowedInterfaces = nil
	for _, row := range d.interfaces {
		ifc := sandbox.AllowedInterface{Name: strings.TrimSpace(row.name)}
		switch row.expect {
		case expectAny:
			ifc.AnyExitAddress = true
		case expectAnyButPhysical:
			ifc.AnyExitAddressButPhysical = true
		case expectPhysical:
			ifc.PhysicalExitAddress = true
		default:
			ifc.ExpectedExitAddresses = exitsOf(row.exits)
		}
		// A row nothing was typed in is no Interface.
		if ifc.Name == "" && ifc.Expectations() == 0 {
			continue
		}
		ifc.StandInAddresses = row.standIns
		cfg.AllowedInterfaces = append(cfg.AllowedInterfaces, ifc)
	}
	cfg.Gate = sandbox.GateConfig{
		ProbeURLs: lines(d.probeURLs),
		ProbePin:  strings.TrimSpace(d.probePin),
		Resolver:  strings.TrimSpace(d.resolver),
		Hold:      d.hold,
	}
	cfg.HostConditions = nil
	for _, h := range d.conditions {
		cfg.HostConditions = append(cfg.HostConditions, sandbox.HostCondition{
			Kind:  keyOf(conditionLabels, h.kind),
			Value: strings.TrimSpace(h.value),
		})
	}

	cfg.Clock = sandbox.ClockPolicy{Timezone: d.timezone, TimezoneProbeURL: strings.TrimSpace(d.timezoneProbeURL)}
	if d.timezone == timezoneOfExit {
		cfg.Clock.Timezone = sandbox.TimezoneAuto
	}

	cfg.Resources = sandbox.Resources{CPUs: d.cpus, MemoryMB: int(d.memoryMB), ShmMB: int(d.shmMB)}
	cfg.Packs = nil
	for _, p := range d.packs {
		if p.enabled {
			cfg.Packs = append(cfg.Packs, p.manifest.Name)
		}
	}
	cfg.SharedFolder = d.sharedFolder()
	if d.folderChoice == sharedFolderNew && cfg.SharedFolder == "" {
		errs = append(errs, sandbox.FieldError{Field: sandbox.FieldSharedFolder, Message: "name the new Shared Folder, or choose " + sharedFolderNone})
	}
	// Host Tools run in the Shared Folder: a sandbox that is given none
	// has no Host Tools, and what is added to their Wall is added only
	// where there are some.
	cfg.HostTools = sandbox.HostTools{}
	if cfg.SharedFolder != "" && d.hostTools {
		cfg.HostTools = sandbox.HostTools{Enabled: true, GPU: d.hostGPU, MacName: d.hostMacName}
		for _, application := range d.applications {
			cfg.HostTools.Applications = append(cfg.HostTools.Applications, application.name)
		}
	}
	return cfg, errs
}

// errorOn joins the errors on a field, for the field to show.
func errorOn(errs []sandbox.FieldError, field sandbox.Field) string {
	var messages []string
	for _, e := range errs {
		if e.Field == field {
			messages = append(messages, e.Message)
		}
	}
	return strings.Join(messages, " ")
}

// save makes a config of the draft and, when nothing is wrong with it,
// replaces the sandbox's saved config. It says whether it did.
func (v *sandboxView) save() bool {
	cfg := v.d.config(v.id)
	if cfg == nil {
		return false
	}
	next, errs := v.draft.config(*cfg)
	if len(errs) == 0 {
		errs = append(v.d.backend.Validate(next), v.d.nameErrors(next)...)
	}
	v.errs, v.saved = errs, false
	if len(errs) > 0 {
		return false
	}
	*cfg = next
	v.saved = true
	// A new folder that was saved is one of those there are from here on.
	if v.draft.folderChoice == sharedFolderNew {
		v.draft.folderChoice, v.draft.newFolder = next.SharedFolder, ""
	}
	v.d.save()
	return true
}

// chooseWay is what a row of the ways out shows before one is chosen.
const chooseWay = "Choose…"

// wayLabel is how the form names one of this Mac's ways out: what the
// system calls it, where it has a title, then its name. Its address on
// its own network says nothing of where it leads, and is not shown.
func wayLabel(ifc Interface) string {
	if ifc.Title == "" {
		return ifc.Name
	}
	return ifc.Title + " · " + ifc.Name
}

// leavesByField is the rows of the Allowed Interfaces, which the form
// calls what the sandbox leaves by: for each, a choice among the ways
// out this Mac has now, what is expected of the exit address there,
// where it stands among the others and, where addresses are expected,
// those addresses under it.
func (v *sandboxView) leavesByField(c *ui.Context, row func(about, help string, align ui.Align, build func())) {
	t := c.Theme()
	d := v.draft
	offered := v.d.backend.Interfaces()
	ui.Field(c, "Leaves by", func() {
		ui.Column(c).Gap(t.Space(2)).Children(func() {
			for i, way := range d.interfaces {
				id, n := fmt.Sprint(way.id), fmt.Sprint(i+1)
				// What is offered: this Mac's ways out as they are now, and
				// the one this row holds where it is not among them.
				names, labels := map[string]string{}, make([]string, 0, len(offered)+1)
				held := ""
				for _, ifc := range offered {
					label := wayLabel(ifc)
					names[label], labels = ifc.Name, append(labels, label)
					if ifc.Name == way.name {
						held = label
					}
				}
				if held == "" && way.name != "" {
					held = way.name + " · not up now"
					names[held], labels = way.name, append(labels, held)
				}
				// What was chosen is the row's; before anything is, and
				// when its label changed, as when it came up or the system
				// gave its title, the row says what it holds.
				if name, chosen := names[way.choice]; chosen {
					way.name = name
				} else if way.choice = held; held == "" {
					way.choice = chooseWay
				}
				ui.Column(c).Key("interface-" + id).Gap(t.Space(1.5)).Children(func() {
					row("", "", ui.Center, func() {
						c.Key("interface-name-" + id)
						ui.Select(c, &way.choice, labels).Label("Way out " + n).Grow(1).Shrink(1)
						if glyphButton(c, "interface-up-"+id, glyphUp, "Move way out "+n+" up").Disabled(i == 0).Clicked() {
							d.interfaces[i-1], d.interfaces[i] = d.interfaces[i], d.interfaces[i-1]
						}
						if glyphButton(c, "interface-remove-"+id, glyphRemove, "Remove way out "+n).Clicked() {
							d.interfaces = append(d.interfaces[:i:i], d.interfaces[i+1:]...)
						}
					})
					// What is expected of the exit address there, and the
					// addresses where addresses are. What was typed is kept
					// while the row expects something else, and is not
					// shown then.
					row("", "", ui.Center, func() {
						c.Key("interface-expect-" + id)
						expect := ui.Select(c, &way.expect, expectations).Label("Expected at way out " + n)
						if way.expect != expectListed {
							expect.Grow(1)
							return
						}
						expect.Width(fieldChoice).Shrink(0)
						c.Key("interface-exits-" + id)
						v.exitsField(c, way, "Expected exit addresses of way out "+n)
					})
					// Whether names are answered with stand-in addresses
					// there: what it means is behind the row's glyph.
					row(standInsAbout+" at way out "+n, standInsHelp, ui.Center, func() {
						c.Key("interface-stand-ins-" + id)
						ui.Checkbox(c, &way.standIns, standInsLabel).Label(standInsAbout + " at way out " + n)
					})
				})
			}
			row("Leaves by", leavesByHelp, ui.Center, func() {
				if glyphButton(c, "interface-add", glyphAdd, "Add a way out").Clicked() {
					d.addInterface(sandbox.AllowedInterface{})
				}
			})
		})
	}).Error(errorOn(v.errs, sandbox.FieldAllowedInterfaces))
}

// exitSeparator is whether a character stands between two of the exit
// addresses typed in a row.
func exitSeparator(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }

// exitsOf are the exit addresses typed in a row, apart by commas or
// spaces.
func exitsOf(typed string) []string { return strings.FieldsFunc(typed, exitSeparator) }

// maxSeenExits is how many of the exit addresses seen at one Interface
// are kept to offer: the latest.
const maxSeenExits = 8

// sawExit takes note of an exit address the sandbox's gate saw at an
// Interface, for the Setup to offer among the addresses expected there:
// what its probe answered, expected or not. Only what a config may
// expect is kept, a public IPv4 address.
func (v *sandboxView) sawExit(iface, address string) {
	if iface == "" {
		return
	}
	if _, err := sandbox.ParsePublicIPv4(address); err != nil {
		return
	}
	seen := slices.DeleteFunc(v.seenExits[iface], func(a string) bool { return a == address })
	seen = slices.Insert(seen, 0, address)
	v.seenExits[iface] = seen[:min(len(seen), maxSeenExits)]
}

// seenByGate is what stands over the exit addresses a row offers.
const seenByGate = "Seen here by this sandbox's gate"

// exitsField is the field of the exit addresses expected at a way out:
// they are typed there, and the ones the sandbox's own gate saw at that
// way out are offered under it, to take instead. A click in the field
// shows those not yet listed; while an address is typed, those that
// begin as it does.
func (v *sandboxView) exitsField(c *ui.Context, way *interfaceDraft, label string) {
	t := c.Theme()
	cb := ui.ComboboxBase(c, &way.exits)
	in := cb.Input.Padding(t.Space(1.5), t.Space(2.5)).Radius(t.Radius).Background(t.Surface).Border(1, t.Border).
		Placeholder("e.g. 203.0.113.7").Label(label).Grow(1)
	if in.Focused() {
		in.Border(1, t.Accent)
	}
	// What is typed next stands after an address that was taken: the
	// field has the row's new text only now, a build later.
	if way.exitTaken {
		end := utf8.RuneCountInString(way.exits)
		in.SetTextSelection(end, end)
		way.exitTaken = false
	}
	// What is listed already, and what is being typed: the last of the
	// addresses, while nothing stands after it.
	split := func(typing bool) (listed []string, partial string) {
		listed = exitsOf(way.exits)
		if n := len(listed); typing && n > 0 && !exitSeparator(rune(way.exits[len(way.exits)-1])) {
			listed, partial = listed[:n-1], listed[n-1]
		}
		return listed, partial
	}
	listed, partial := split(cb.Filtering())
	var offered []string
	for _, address := range v.seenExits[way.name] {
		if !slices.Contains(listed, address) && address != partial && strings.HasPrefix(address, partial) {
			offered = append(offered, address)
		}
	}
	cb.Popup(func(panel ui.Element) {
		panel.Margin(t.Space(1), 0, 0, 0).Padding(t.Space(1)).Radius(t.Radius+2).Background(t.Background).Border(1, t.Border)
		if len(offered) == 0 {
			return
		}
		ui.Text(c, seenByGate).FontSize(textSmall).TextColor(t.TextMuted).Padding(t.Space(1), t.Space(2.5))
		for _, address := range offered {
			item := cb.Item(address).Padding(t.Space(1.5), t.Space(2.5)).Radius(t.Radius)
			if item.Highlighted() {
				item.Background(t.Accent).TextColor(t.AccentText)
			}
			item.Children(func() { ui.Text(c, address).SingleLine() })
		}
	})
	// One that is taken stands in the place of what was being typed, or
	// after the ones listed. Taking it ended the typing, so whether an
	// address was being typed is what the build before this one left.
	if taken, chosen := cb.Chosen(); chosen {
		listed, _ := split(way.typingExit)
		way.exits, way.exitTaken = strings.Join(append(slices.Clone(listed), taken), ", "), true
	}
	way.typingExit = cb.Filtering()
}

// leavesByHelp is what there is to know of the ways out.
const leavesByHelp = "What the sandbox may leave this Mac by, most preferred first. The list is what is up now. It leaves by the first that is up and by no other: TCP and name queries, and only while the probe sees an exit address expected there. For each, say what is expected: the public IPv4 addresses you list, among which the field offers those this sandbox's gate has seen there; any; any but physical; or physical and no other. A physical exit address is where Wi-Fi and Ethernet lead with nothing between, and where a tunnel leads once it leads nowhere else. For the last two the app asks the probe URLs by Wi-Fi and Ethernet too, and the gate is locked while it does not know what they answer."

// What a row of the ways out says of stand-in addresses: the words
// beside its switch, what the switch and its glyph are called for
// whoever does not see them, and what there is to know of it.
const (
	standInsLabel = "Answers names with stand-in addresses"
	standInsAbout = "Stand-in addresses"
	standInsHelp  = "For a tunnel that answers a name with a stand-in address, one of 198.18.0.0/15, in place of the name's own. A sandbox connects to public addresses and no others, so by such a tunnel its names resolve and nothing connects. With this it connects to stand-in addresses too, while it leaves by this way out and by no other. Where one leads is then decided where the way out leads, by the name it stands for: a name that is led to this Mac or to your own network there reaches it. See that what keeps the tunnel up sends all the sandbox sends one way. This Mac's own address in the range stays out of reach."
)

// holdHelp is what there is to know of a Hold.
var holdHelp = fmt.Sprintf("For a way out whose probes are slow at times. Without a Hold the gate is locked %g seconds after the last probe that was answered as expected, and every connection is cut. With one it is locked sooner, about two seconds after that probe with three probe URLs, and cuts nothing: the sandbox's connections are kept for %g seconds from that probe. What they receive still arrives. What the sandbox sends on them waits on this Mac until a probe is answered again, and it makes no new connection and asks no name. This Mac still acknowledges what arrives, which shows a new exit address if it changed meanwhile. An answer that is not expected ends the Hold at once, and so does everything else that locks the gate.", sandbox.LeaseDuration.Seconds(), sandbox.HoldDuration.Seconds())

// timezoneHelp is what there is to know of a sandbox's clock.
const timezoneHelp = "The timezone the sandbox's clock reads in. The clock itself is this Mac's, and nothing shifts it. Type to find one, as Berlin finds Europe/Berlin. That of the exit address is read once, when the sandbox starts, and does not follow a change of way out."

// shmHelp is what there is to know of a workstation's /dev/shm.
var shmHelp = fmt.Sprintf("Shared memory, which a browser's pages and a 3D engine's frames pass through. Chromium and 3D engines crash with less than %d MB. It counts against the sandbox's memory, which must be at least 512 MB more, and is gone when the sandbox stops.", sandbox.MinShmMB)

// What the choice of a sandbox's Shared Folder offers besides the
// folders there are. Neither is a folder's name: a name has no capital
// and no space.
const (
	sharedFolderNone = "None"
	sharedFolderNew  = "New folder…"
)

// sharedFolder is the name of the Shared Folder the draft gives the
// sandbox, empty for none: the one chosen, or the one typed for a new
// one.
func (d *draft) sharedFolder() string {
	switch d.folderChoice {
	case sharedFolderNone:
		return ""
	case sharedFolderNew:
		return strings.TrimSpace(d.newFolder)
	}
	return d.folderChoice
}

// sharedFolderField is the choice of the sandbox's Shared Folder: none,
// one of those there are, or a new one, whose name is then asked under
// the choice.
func (v *sandboxView) sharedFolderField(c *ui.Context, row formRow) {
	t := c.Theme()
	d := v.draft
	names := v.d.sharedFolderNames()
	choices := append([]string{sharedFolderNone}, names...)
	// The one the draft holds is offered though it is there no longer.
	if d.sharedFolder() != "" && d.folderChoice != sharedFolderNew && !slices.Contains(names, d.folderChoice) {
		choices = append(choices, d.folderChoice)
	}
	choices = append(choices, sharedFolderNew)
	ui.Field(c, sharedFolderField, func() {
		ui.Column(c).Gap(t.Space(1.5)).Children(func() {
			row(sharedFolderField, sharedFolderHelp(v.d.backend.Runtime().SharedFolderRoot), ui.Center, func() {
				c.Key("shared-folder")
				ui.Select(c, &d.folderChoice, choices).Label(sharedFolderChoice).Grow(1)
			})
			if d.folderChoice == sharedFolderNew {
				row("", "", ui.Center, func() {
					c.Key("shared-folder-new")
					ui.TextInput(c, &d.newFolder).Placeholder("research").Label(newSharedFolderName).AutoFocus().Grow(1)
				})
			}
		})
	}).Error(errorOn(v.errs, sandbox.FieldSharedFolder))
}

// allowedApplicationRows are the sandbox's Allowed Applications: what
// they are, with the control that adds a row while a folder of this
// Mac's /Applications is left to name; and under it a row for each, a
// choice among the folders that no other row holds, with the one it
// holds though it is there no longer.
func (v *sandboxView) allowedApplicationRows(c *ui.Context, row formRow) {
	d := v.draft
	offered := v.d.backend.Applications()
	held := func(name string, but int) bool {
		for i, application := range d.applications {
			if i != but && application.name == name {
				return true
			}
		}
		return false
	}
	next := ""
	for _, name := range offered {
		if !held(name, -1) {
			next = name
			break
		}
	}
	row("Allowed Applications", allowedApplicationsHelp, ui.Center, func() {
		ui.Text(c, "Allowed Applications").Grow(1)
		if next != "" && glyphButton(c, "host-tools-application-add", glyphAdd, "Add Allowed Application").Clicked() {
			d.applications = append(d.applications, &applicationDraft{name: next})
		}
	})
	for i, application := range d.applications {
		n := fmt.Sprint(i + 1)
		choices := []string{}
		for _, name := range offered {
			if !held(name, i) {
				choices = append(choices, name)
			}
		}
		if !slices.Contains(choices, application.name) {
			choices = append(choices, application.name)
		}
		row("", "", ui.Center, func() {
			c.Key("host-tools-application-" + n)
			ui.Select(c, &application.name, choices).Label("Allowed Application " + n).Grow(1)
			if glyphButton(c, "host-tools-application-remove-"+n, glyphRemove, "Remove Allowed Application "+n).Clicked() {
				d.applications = append(d.applications[:i:i], d.applications[i+1:]...)
			}
		})
	}
}

// sharedFolderHelp says what a Shared Folder is, where it is and who
// else has it.
func sharedFolderHelp(root string) string {
	return "A folder of this Mac, mounted at " + sandbox.SharedFolderPath + ", beside the sandbox's Home, " + sandbox.HomePath + ", which is its own. Choose one there is, or " + sharedFolderNew + " and name it: the app makes " + root + "/<name> when the sandbox starts. Sandboxes given the same folder mount the same one: each reads and writes all of it. Its Host Tools run there. It is kept until you delete it on its own page."
}

// What there is to know of Host Tools, and of what a Setup adds to their
// Wall.
const (
	hostToolsHelp           = "Lets the sandbox's agent run programs of this Mac by the tool run of the MCP server macos. Each runs under the Host Tool Wall: it reads the system's own software, Homebrew's and the sandbox's Allowed Applications, reads and writes this Shared Folder and a folder of its own, and has nothing else of this Mac: none of your files, no network, no other program, no window. So the agent learns which packages of Homebrew's you have installed, this Mac's model and its chip, and what a program writes fills this Mac's disk, which nothing bounds. Its calls show under MCP Calls. Look at what a program made; do not open it as a program."
	hostGPUHelp             = "Adds this Mac's GPU to the Wall, with the system's shader compiler and its hardware video encoder and decoder: what Metal and VideoToolbox need. Without it a program works on the CPU. Blender needs it even to start."
	allowedApplicationsHelp = "The folders of /Applications that the sandbox's Host Tools read and run. Of any other the Wall tells them nothing, not even whether it is installed, and it lists none. Xcode is one of them: a sandbox that is not given it compiles with the command line tools, where this Mac has them. A link in /Applications is not offered: where it leads is not the Wall's to open."
	hostMacNameHelp         = "Adds this Mac's name to the Wall. Without it uname fails, and with it whatever asks the platform's name: Python's platform module, numpy, Blender, the Swift compiler. The name often holds its owner's: give this Mac a plain one in System Settings, under Sharing, if that matters."
)

// packNote says, beside a pack's checkbox, what enabling it means.
func packNote(m pack.Manifest) string {
	return strings.TrimSpace(m.Description)
}
