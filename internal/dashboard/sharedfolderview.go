package dashboard

import (
	"fmt"
	"slices"
	"strings"

	"github.com/egoist/mygo/ui"

	"zeolite/internal/sandbox"
)

// sharedFolderKeyPrefix begins the key by which the sidebar chooses a
// Shared Folder. A sandbox's ID holds no slash, so no sandbox has such a
// key.
const sharedFolderKeyPrefix = "shared-folder/"

// sharedFolderKey is the key by which the sidebar chooses a Shared Folder.
func sharedFolderKey(name string) string { return sharedFolderKeyPrefix + name }

// sharedFolderOf reads the name of a Shared Folder out of its key.
func sharedFolderOf(key string) (name string, ok bool) {
	return strings.CutPrefix(key, sharedFolderKeyPrefix)
}

// sharedFolderNames are the Shared Folders there are, in order: the ones
// on this Mac's disk, and the ones a sandbox is given, which the app
// makes when that sandbox starts.
func (d *Dashboard) sharedFolderNames() []string {
	names := append([]string(nil), d.folders...)
	for _, cfg := range d.configs {
		if cfg.SharedFolder != "" {
			names = append(names, cfg.SharedFolder)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// listSharedFolders asks the backend which Shared Folders are on this
// Mac's disk, off the main thread.
func (d *Dashboard) listSharedFolders() {
	go func() {
		names, err := d.backend.SharedFolders()
		d.update(func() {
			if err != nil {
				d.problem = "The Shared Folders could not be listed: " + err.Error()
				return
			}
			d.folders = names
		})
	}()
}

// sharedFolderView is what the dashboard shows and holds for one Shared
// Folder.
type sharedFolderView struct {
	d    *Dashboard
	name string

	// confirmDelete is whether the alert that asks before the folder is
	// deleted is open, and deleting whether it is being deleted.
	confirmDelete bool
	deleting      bool
	// problem is why the folder was not deleted.
	problem string
}

// folderView returns the view of a Shared Folder, making it when it has
// none yet.
func (d *Dashboard) folderView(name string) *sharedFolderView {
	v := d.folderViews[name]
	if v == nil {
		v = &sharedFolderView{d: d, name: name}
		d.folderViews[name] = v
	}
	return v
}

// given are the sandboxes given this Shared Folder.
func (v *sharedFolderView) given() []sandbox.Config { return v.d.givenFolder(v.name) }

// givenFolder are the sandboxes given a Shared Folder.
func (d *Dashboard) givenFolder(name string) []sandbox.Config {
	var out []sandbox.Config
	for _, cfg := range d.configs {
		if cfg.SharedFolder == name {
			out = append(out, cfg)
		}
	}
	return out
}

// givenLine says, in a line, which sandboxes are given a Shared Folder.
func (d *Dashboard) givenLine(name string) string {
	given := d.givenFolder(name)
	if len(given) == 0 {
		return "no sandbox is given it"
	}
	names := make([]string, len(given))
	for i, cfg := range given {
		names[i] = cfg.Name
	}
	return "given to " + strings.Join(names, ", ")
}

// delete deletes the folder from this Mac's disk. The backend refuses
// while a running sandbox mounts it.
func (v *sharedFolderView) delete() {
	d := v.d
	v.deleting, v.problem = true, ""
	go func() {
		err := d.backend.RemoveSharedFolder(v.name)
		names, listErr := d.backend.SharedFolders()
		d.update(func() {
			v.deleting = false
			if err != nil {
				v.problem = "The Shared Folder was not deleted: " + err.Error()
				return
			}
			if listErr == nil {
				d.folders = names
			}
			// A folder no sandbox is given is gone from the sidebar too.
			if !slices.Contains(d.sharedFolderNames(), v.name) {
				delete(d.folderViews, v.name)
			}
		})
	}()
}

func (v *sharedFolderView) view(c *ui.Context) {
	t := c.Theme()
	d := v.d
	path := d.backend.Runtime().SharedFolderRoot + "/" + v.name
	onDisk := slices.Contains(d.folders, v.name)
	given := v.given()
	ui.Column(c).Fill().Children(func() {
		// The header is the window's title bar here: it drags the window.
		ui.Row(c).Height(headerHeight).Shrink(0).Gap(t.Space(2.5)).Padding(0, t.Space(3), 0, t.Space(4)).AlignItems(ui.Center).
			Font(chromeFont).Background(panelTheme.Background).DragWindow().Children(func() {
			glyphFolder.view(c, t.TextMuted, glyphCell).Label("Shared Folder")
			ui.Text(c, v.name).FontSize(textTitle).Bold().MaxLines(1).Shrink(1)
			ui.Spacer(c)
			if glyphButton(c, "delete-shared-folder", glyphDestroy, "Delete").Disabled(v.deleting || !onDisk).Clicked() {
				v.confirmDelete = true
			}
		})
		if v.problem != "" {
			banner(c, v.problem)
		}
		d.runtimeBanner(c)

		ui.Column(c).Shrink(0).Gap(t.Space(2.5)).Padding(t.Space(3), t.Space(4)).Font(chromeFont).Children(func() {
			// Where it is on this Mac, and who works in it.
			ui.Row(c).Gap(t.Space(1.5)).AlignItems(ui.Center).Children(func() {
				ui.Text(c, path).MaxLines(1).Shrink(1)
				if glyphButton(c, "copy-path", glyphClone, "Copy Path").Clicked() {
					c.Services().WriteClipboard(path)
				}
				if glyphButton(c, "show-in-finder", glyphFolder, "Show in Finder").Disabled(!onDisk).Clicked() {
					go d.backend.ShowSharedFolder(v.name)
				}
				about := "Each sandbox given this folder mounts it at " + sandbox.SharedFolderPath + " and reads and writes all of it. What is in it is the agents': treat it on this Mac as you would a download."
				if !onDisk {
					about = "Not on this Mac's disk yet: the app makes it when a sandbox given it starts. " + about
				}
				info(c, "this Shared Folder", about)
			})
			// The sandboxes given the folder.
			ui.Row(c).Wrap().Gap(t.Space(4)).AlignItems(ui.Center).Children(func() {
				for _, cfg := range given {
					status := sandbox.Status{State: sandbox.StateStopped}
					if sv := d.views[cfg.ID]; sv != nil {
						status = sv.status
					}
					ui.Row(c).Key("given-" + cfg.ID).Gap(t.Space(1.5)).AlignItems(ui.Center).Children(func() {
						stateGlyph(c, status)
						ui.Text(c, cfg.Name).MaxLines(1)
					})
				}
			})
			if len(given) == 0 {
				ui.Text(c, "No sandbox is given this folder: name it in a sandbox's Setup.").FontSize(textSmall).TextColor(t.TextMuted)
			}
		})
	})

	question := fmt.Sprintf("Delete the Shared Folder “%s”?", v.name)
	consequence := "Everything in it is deleted from this Mac."
	if len(given) > 0 {
		consequence += " The sandboxes given it get an empty one at their next start."
	}
	if ui.AlertDialog(c, &v.confirmDelete, question, consequence, "Cancel", "Delete") == 1 {
		v.delete()
	}
}
