# Layout

```
main.go, backend.go     the app: wires the pieces behind the dashboard's Backend
acpbridge.go            the same executable as a sandbox's ACP Bridge, when an ACP client starts it as one
internal/sandbox/       core types: a sandbox's config, its validation, its status, what the containers report
internal/pack/          Capability Pack manifests and the Pack Catalog; builtin/*.yaml
internal/orchestrator/  everything that talks to Docker
internal/sandboximage/  the images' build contexts, with the Linux binaries embedded
internal/hostwatch/     the Host Watchdog: evaluates Host Conditions on the Mac, and reads this Mac's physical exit addresses
internal/probe/         a probe's request, as the gatekeeper and the Host Watchdog both send it
internal/viewport/      Viewport Streams
internal/configstore/   the sandboxes' config file
internal/sharedfolder/  the Shared Folders on this Mac's disk: made, listed by name and deleted, never read
internal/agentfiles/    what agents wrote on this Mac's disk, deleted: by names, however it was left, never read and no link followed
internal/carrier/       the app's side of a sandbox's Carrier: the program, embedded, started under the Carrier Wall
internal/link/          the app's end of a sandbox's Link: reads what a workstation asks, and hands each ask to what answers its kind
internal/hosttool/      a sandbox's Host Tools: the Host Tool Wall, the GPU Shim, and the launch of a program under them
internal/acpbridge/     what crosses a sandbox's ACP Bridge: each message of the Agent Client Protocol judged and written anew, paths mapped by their text
internal/palette/       the app's colors
internal/cage/          the sodalite cage, turning, as characters on a grid: the Opening's and the Masthead's
internal/chargrid/      characters drawn into an image, each in a cell of a grid: the icon's and the Masthead's
internal/dashboard/     the window: MyGo native UI views and their state
cmd/gatekeeper/         runs in the gatekeeper container: the ruleset, the probe, the lease
cmd/tun/                in the gatekeeper's image: makes a sandbox's TUN device and moves its packets
cmd/supervisor/         PID 1 of the workstation; also the MCP servers `macos` and `sandbox`, their bridge, the workstation's end of the sandbox's Link, and what starts an agent for the sandbox's ACP Bridge
cmd/carrier/            runs on the Mac, one for a sandbox: carries its traffic on sockets bound to an interface; with the Carrier Wall
cmd/appicon/            draws resources/icon.png: a zeolite's cage in ASCII art, in the palette's colors
cmd/masthead/           draws docs/masthead.gif, the README's Masthead: the cage turning, and the app's name in italic letters made of characters that tremble
integration/            tests against the Colima profile
```

Imports point inward to `internal/sandbox`. The UI is [MyGo](https://github.com/egoist/mygo) native UI, pinned at v0.10.0; only the main package and `internal/dashboard` use it, so an upgrade that changes its API, or another UI, touches one package. MyGo is young and changes fast: upgrade it deliberately and rerun every [test](tests.md).

The terminal is MyGo's terminal plugin, which loads Ghostty's `libghostty-vt`. `go tool mygo build` puts the library in the app and signs it; `go test` and `go run` download it once into `~/Library/Caches/mygo/natives/` and check its hash against the one the plugin pins.

## Where the app keeps things

`--activate=false` leaves your current Docker context alone; the app talks to the profile's own socket. At launch the app reads the VM's mount table and says in a banner if the VM mounts any other folder of the Mac. A VM started with `--mount none` works too: no sandbox can then be started with a Shared Folder.

Each sandbox has a **Home**, a Docker volume in the VM named `zeolite-<id>-home`, which its workstation mounts at `/workspace`. Destroy deletes it, and nothing else does: `colima delete --data` deletes every Home with the VM's disk, and `docker volume ls --filter label=zeolite.sandbox` on the profile's socket lists them, a Home whose sandbox was lost among them.

The app keeps its data in `~/Library/Application Support/zeolite/`: `sandboxes.json` (readable only by you), your own packs in `packs/*.yaml`, and the Carrier's program, which the app writes at launch: `bin/carrier`. The directory itself is yours alone. What agents keep in a Shared Folder lives elsewhere, in `/Users/Shared/zeolite/shared.noindex/`, and beside it, in `/Users/Shared/zeolite/hosttool.noindex/`, what their [Host Tools](host-tools.md) keep, and in its `gpu/` the GPU Shim with the Shell Startup File and the Homebrew Wrappers.
