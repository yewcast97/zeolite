# Shared Folders

A sandbox's `/workspace` is its Home, in the VM, and its own. A Shared Folder is where sandboxes keep files on this Mac's disk, and where they work together: choose one in a sandbox's Setup, or name a new one there, and its workstation mounts it at `/workspace/shared`, where a Terminal Session reaches it as any other folder. Sandboxes given the same Shared Folder mount the same folder.

- **The app makes the folder, empty**, at `/Users/Shared/zeolite/shared.noindex/<name>`, when a sandbox given it starts: outside every home directory, so that no path a sandbox sees carries your name, and yours alone. There is no choosing a folder you already have: what such a folder holds (a checkout's `.git/config`, a `.env`) would be the sandboxes' to read.
- **Each has a page in the dashboard**, behind its row in the sidebar: its path on this Mac, the sandboxes given it, and Delete.
- **A folder is kept until you delete it there.** Stopping or destroying a sandbox, or changing its Setup, deletes nothing. Delete asks first, and is refused while a running sandbox mounts the folder.
- **The app reads nothing in it.** It makes, lists by name and deletes the folders, and passes a folder's path to Docker. A sandbox's [ACP Bridge](acp-bridge.md) writes paths in it as an editor on this Mac and the agent each have them, by their text alone. One package names their root, that package reads no file, and tests hold both. Deleting takes names only beneath the root: a link an agent left that leads elsewhere is deleted, never followed. How a folder was left does not keep it: what a build left read-only, and what a Host Tool put under a flag that forbids deleting it (`chflags uchg`), is deleted all the same.

What you accept by giving sandboxes one:

- **Every sandbox given a Shared Folder reads and writes all of it.** What one wrote, another reads, changes and deletes. Sandboxes that share a folder can tell each other what each knows, their exit addresses included, and each learns the others' names: give one folder only to sandboxes that may know of each other.
- **What is in it is the agents'.** Treat it on this Mac as you would a download: opening, building or running any of it here reads what an agent wrote, and some of that runs it. An editor that opens a folder of it, as one does to work with an agent by the sandbox's ACP Bridge, reads what the folder holds for editors too.
- **It has no quota.** An agent can fill this Mac's disk.
- **It shows a sandbox a little of this Mac** that is not about its network: the folder's name; that the VM is Lima's, by the name its mount carries; the size and free space of the disk; and the extended attributes macOS puts on files, among them `com.apple.provenance`, a few bytes that stand for the program that made a file. Files show as the sandbox's own user's, whoever made them.
