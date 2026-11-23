package sandbox

// A sandbox's Link is the one stream by which what runs in its
// workstation asks something of the app: the standard streams of
// `supervisor link`, run in the workstation and carried by the Docker
// API, so that nothing listens for it on the Mac and no port of the
// sandbox is published. Each ask is a JSON value on a line of its own,
// and so is each answer. A workstation is not trusted: the app checks
// every ask again, and takes nothing an ask says of who asks.

// LinkRequest is one ask on a sandbox's Link, or the withdrawal of one:
// exactly one kind of ask is set.
type LinkRequest struct {
	// ID tells the asks of a Link apart. The workstation's end of the
	// Link gives it; an agent does not.
	ID uint64 `json:"id"`
	// Cancel withdraws the ask that has this ID: what it started is
	// ended, and nothing is answered.
	Cancel bool `json:"cancel,omitempty"`
	// HostTool asks for one run of a Host Tool.
	HostTool *HostToolRequest `json:"hostTool,omitempty"`
	// Hand raises an agent's hand. Its answer is that it was not
	// refused.
	Hand *HandRequest `json:"hand,omitempty"`
}

// HandRequest says that the agent of a Terminal Session raises its
// hand: it cannot go on without its user. It carries no words.
type HandRequest struct {
	// Session is the ID of the Terminal Session the agent runs in, as
	// the workstation found it. The app takes it for one of the sessions
	// it opened in that sandbox, or refuses.
	Session string `json:"session"`
}

// StateServer is the name of the MCP server by which a sandbox's agent
// tells its user of its state, and RaiseHandTool the name of its tool
// that raises a hand. No Capability Pack's server has that name.
const (
	StateServer   = "sandbox"
	RaiseHandTool = "raise_hand"
)

// LinkAnswer answers a LinkRequest: it is refused, or the answer to the
// kind of ask it was is set.
type LinkAnswer struct {
	ID uint64 `json:"id"`
	// Refused is why the app did not take the ask at all, empty where it
	// did: the sandbox is given nothing of that kind, or the ask was
	// none.
	Refused  string          `json:"refused,omitempty"`
	HostTool *HostToolResult `json:"hostTool,omitempty"`
}

// LinkMaxAsks is how many asks of one Link the app works on at once:
// more than a workstation's agents make together, and few enough that a
// workstation cannot make the app hold any number of them.
const LinkMaxAsks = 64

// LinkMaxLine bounds one ask as it arrives, before it is read as JSON.
const LinkMaxLine = 4 * HostToolMaxArgBytes
