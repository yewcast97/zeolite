package dashboard

import (
	"fmt"

	"github.com/egoist/mygo/ui"

	"zeolite/internal/sandbox"
)

// The parts of a sandbox's Setup that the tests name, as the form names
// them; the field that holds its Shared Folder, and what its two
// controls are called for whoever does not see them.
const (
	partNetwork         = "Network"
	partSharedFolder    = "Shared Folder"
	sharedFolderField   = "Folder"
	sharedFolderChoice  = "The sandbox's Shared Folder"
	newSharedFolderName = "New Shared Folder's name"
)

// formRow builds the line of a field: what build makes, then the glyph
// that explains it, at the row's end, where there is something to
// explain.
type formRow func(about, help string, align ui.Align, build func())

// setupView is the sandbox's Setup: its name, its network, which Check
// tries, its Shared Folder, and the rest of its configuration. It is a form: a column of labels, a column of
// controls that begin on one line and end on another, and at the end of
// a row the glyph that holds what there is to know of it.
func (v *sandboxView) setupView(c *ui.Context) {
	t := c.Theme()
	d := v.draft
	if v.d.config(v.id) == nil {
		return
	}
	row := formRow(func(about, help string, align ui.Align, build func()) {
		ui.Row(c).Gap(t.Space(1.5)).AlignItems(align).Children(func() {
			// What build makes has the row but for the glyph's place: a
			// control that grows ends where the glyphs stand.
			ui.Row(c).Grow(1).Shrink(1).MinWidth(0).Gap(t.Space(1.5)).AlignItems(align).Children(build)
			if help != "" {
				info(c, about, help)
			} else {
				ui.Box(c).Width(glyphButtonSize).Shrink(0)
			}
		})
	})
	field := func(label string, f sandbox.Field, help string, build func()) {
		ui.Field(c, label, func() { row(label, help, ui.Center, build) }).Error(errorOn(v.errs, f))
	}

	ui.Scroll(c).Grow(1).FillWidth().Children(func() {
		ui.Column(c).Gap(t.Space(4)).Padding(t.Space(5), t.Space(4), t.Space(8), t.Space(4)).MaxWidth(formWidth).Font(chromeFont).Children(func() {
			if running(v.status.State) {
				ui.Text(c, "Running: Check starts it again where this changed; what is in its Panes ends with that.").FontSize(textSmall).TextColor(t.TextMuted)
			}
			ui.Form(c, func() {
				field("Name", sandbox.FieldName, "", func() {
					c.Key("name")
					ui.TextInput(c, &d.name).Grow(1)
				})

				ui.Fieldset(c, partNetwork, func() {
					v.networkCheck(c, row)
					v.leavesByField(c, row)
					ui.Field(c, "Probe URLs", func() {
						row("Probe URLs", "HTTPS endpoints that answer with the caller's address as plain text, one per line. Each is asked every two seconds, and every second while an answer is overdue. An answer with an expected exit address keeps the gate open; one with another locks it.", ui.Start, func() {
							c.Key("probe")
							ui.TextArea(c, &d.probeURLs).Height(t.Space(18)).Grow(1)
						})
					}).Error(errorOn(v.errs, sandbox.FieldProbeURLs))
					field("Probe pin", sandbox.FieldProbePin, "Optional: the base64 SHA-256 of the public key the probe endpoints must present.", func() {
						c.Key("pin")
						ui.TextInput(c, &d.probePin).Placeholder("none").Grow(1)
					})
					field("Resolver", sandbox.FieldResolver, "The public DNS resolver the sandbox's name queries go to, as an IPv4 address. It is the only one the sandbox reaches.", func() {
						c.Key("resolver")
						ui.TextInput(c, &d.resolver).Placeholder(sandbox.DefaultResolver).Grow(1)
					})
					ui.Field(c, "Hold", func() {
						row("Hold", holdHelp, ui.Center, func() {
							c.Key("hold")
							ui.Checkbox(c, &d.hold, "Keep its connections while no probe answers")
						})
					})
					// The Host Conditions, under a label as short as
					// the others: the form's labels share one column.
					ui.Field(c, "Requires", func() {
						ui.Column(c).Gap(t.Space(2)).Children(func() {
							for i, h := range d.conditions {
								n := fmt.Sprint(i + 1)
								row("", "", ui.Center, func() {
									c.Key("condition-kind-" + n)
									ui.Select(c, &h.kind, labelsOf(sandbox.HostConditionKinds, conditionLabels)).Width(fieldChoice)
									c.Key("condition-value-" + n)
									ui.TextInput(c, &h.value).Placeholder("network name").Label("Host Condition " + n).Grow(1)
									if glyphButton(c, "condition-remove-"+n, glyphRemove, "Remove Host Condition "+n).Clicked() {
										d.conditions = append(d.conditions[:i:i], d.conditions[i+1:]...)
									}
								})
							}
							row("Host Conditions", "The gate is locked while any of these does not hold on this Mac, or cannot be read.", ui.Center, func() {
								if glyphButton(c, "condition-add", glyphAdd, "Add Host Condition").Clicked() {
									d.conditions = append(d.conditions, &conditionDraft{kind: conditionLabels[sandbox.HostConditionKinds[0]]})
								}
							})
						})
					}).Error(errorOn(v.errs, sandbox.FieldHostConditions))
				})

				ui.Fieldset(c, partSharedFolder, func() {
					v.sharedFolderField(c, row)
					// Host Tools run in the Shared Folder: they are
					// offered once the sandbox is given one.
					if d.sharedFolder() != "" {
						ui.Field(c, "Host Tools", func() {
							ui.Column(c).Gap(t.Space(1)).Children(func() {
								row("Host Tools", hostToolsHelp, ui.Center, func() {
									c.Key("host-tools")
									ui.Checkbox(c, &d.hostTools, "Run this Mac's programs in the folder")
								})
								if d.hostTools {
									row("The GPU", hostGPUHelp, ui.Center, func() {
										c.Key("host-tools-gpu")
										ui.Checkbox(c, &d.hostGPU, "With this Mac's GPU")
									})
									row("This Mac's name", hostMacNameHelp, ui.Center, func() {
										c.Key("host-tools-name")
										ui.Checkbox(c, &d.hostMacName, "With this Mac's name")
									})
									v.allowedApplicationRows(c, row)
								}
							})
						}).Error(errorOn(v.errs, sandbox.FieldHostTools))
					}
				})

				ui.Fieldset(c, "Clock Policy", func() {
					field("Timezone", sandbox.FieldTimezone, timezoneHelp, func() {
						c.Key("timezone")
						ui.Combobox(c, &d.timezone, timezoneChoices(d.timezone)).Grow(1)
					})
					if d.timezone == timezoneOfExit {
						field("Timezone probe", sandbox.FieldTimezoneProbeURL, "An HTTPS endpoint that answers with the IANA timezone of the caller's address.", func() {
							c.Key("timezone-probe")
							ui.TextInput(c, &d.timezoneProbeURL).Placeholder("https://").Label("Timezone probe URL").Grow(1)
						})
					}
				})

				ui.Fieldset(c, "Resources", func() {
					field("CPUs", sandbox.FieldCPUs, "", func() {
						c.Key("cpus")
						ui.NumberInput(c, &d.cpus, 0.25, 64, 0.25).Width(fieldNarrow)
					})
					field("Memory (MB)", sandbox.FieldMemory, "/dev/shm is memory and counts against it. The sandbox's Home, /workspace, is on the VM's disk: it is kept when the sandbox stops, and has no limit of its own. So is /tmp, which is emptied when the sandbox stops.", func() {
						c.Key("memory")
						ui.NumberInput(c, &d.memoryMB, 512, 262144, 256).Width(fieldNarrow)
					})
					field("/dev/shm (MB)", sandbox.FieldShm, shmHelp, func() {
						c.Key("shm")
						ui.NumberInput(c, &d.shmMB, sandbox.MinShmMB, 65536, 256).Width(fieldNarrow)
					})
				})

				if len(d.packs) > 0 {
					ui.Fieldset(c, "Capability Packs", func() {
						ui.Field(c, "Enabled", func() {
							ui.Column(c).Gap(t.Space(1)).Children(func() {
								for _, p := range d.packs {
									row(p.manifest.Title, packNote(p.manifest), ui.Center, func() {
										c.Key("pack-" + p.manifest.Name)
										ui.Checkbox(c, &p.enabled, p.manifest.Title)
									})
								}
							})
						}).Error(errorOn(v.errs, sandbox.FieldPacks))
					})
				}

				ui.Field(c, "", func() {
					ui.Row(c).Gap(t.Space(2.5)).AlignItems(ui.Center).Children(func() {
						if ui.PrimaryButton(c, "Save").Key("save").Clicked() {
							v.save()
						}
						switch {
						case len(v.errs) > 0:
							ui.Text(c, "Not saved: see the fields marked above.").TextColor(t.Danger)
						case v.saved:
							ui.Text(c, "Saved.").TextColor(t.TextMuted)
						}
					})
				})
			})
		})
	})
}

// checkLine is the first line of a part that the app can try: a tick
// once it holds and a ring until then, what the app has to say of it,
// and after it what controls builds.
func checkLine(c *ui.Context, row formRow, holds bool, says string, controls func()) {
	t := c.Theme()
	ui.Field(c, "", func() {
		row("", "", ui.Center, func() {
			if holds {
				glyphCheck.view(c, t.Success, glyphCell).Label("This holds")
			} else {
				stateGlyphs[sandbox.StateStopped].view(c, t.TextMuted, glyphCell).Label("This does not hold yet")
			}
			ui.Text(c, says).FontSize(textSmall).TextColor(t.TextMuted).Grow(1).Shrink(1)
			controls()
		})
	})
}

// networkCheck is the first line of the network: what the sandbox's gate
// says of it now, in the app's own words, and Check, which saves the
// Setup and starts the sandbox to find out, with the Setup still shown.
func (v *sandboxView) networkCheck(c *ui.Context, row formRow) {
	t := c.Theme()
	holds, says := v.networkLine()
	checkLine(c, row, holds, says, func() {
		tip := "Check: save this and start the sandbox, to see what its gate says here."
		if glyphButtonIn(c, "check", glyphCheck, "Check", tip, t.Accent).Disabled(v.busy).Clicked() {
			v.d.check(v.id)
		}
	})
}

// heldSays is what the app says of a gate that is locked while its Hold
// lasts.
const heldSays = "its connections are kept, what it sends waits, and it makes no new one"

// networkLine is whether the sandbox's network is as required now, and
// what the app says of it: what its gate does.
func (v *sandboxView) networkLine() (holds bool, says string) {
	switch v.status.State {
	case sandbox.StateStarting:
		return false, "Starting…"
	case sandbox.StateGateOpen:
		says = "Gate open"
		if v.gate.Interface != "" {
			says += " · leaves by " + v.gate.Interface
		}
		if v.status.ExitAddress != "" {
			says += " · seen at " + v.status.ExitAddress
		}
		return true, says
	case sandbox.StateGateLocked:
		says = "Gate locked"
		if v.status.LockReason != "" {
			says += ": " + string(v.status.LockReason)
		}
		if v.gate.Held {
			says += " · " + heldSays
		}
		return false, says
	}
	return false, "Not checked yet: Check starts the sandbox, and its gate says."
}
