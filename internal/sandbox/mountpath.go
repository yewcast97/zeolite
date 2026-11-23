package sandbox

// HomePath is where a workstation mounts its sandbox's Home: the home
// directory of the agent's user, and where a Terminal Session starts.
const HomePath = "/workspace"

// SharedFolderPath is where a workstation mounts the Shared Folder. It
// is under HomePath, so a Terminal Session reaches it as it reaches any
// other folder.
const SharedFolderPath = HomePath + "/shared"

// TmpPath is a workstation's /tmp: a volume on the VM's disk that is
// made with the workstation and removed with it, so that what is there
// lasts from a start to the stop and no longer.
const TmpPath = "/tmp"
