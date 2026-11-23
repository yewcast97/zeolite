package dashboard

import (
	"fmt"
	"slices"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"

	"zeolite/internal/sandbox"
)

// maxPanes is how many Panes a running sandbox's page holds at most:
// two rows of two.
const maxPanes = 4

// session is one Terminal Session held by the dashboard: a login shell
// in a sandbox's workstation, and whatever its user started in it. It
// ends when its Pane closes while it is a plain shell's, when its shell
// exits, or with its sandbox.
type session struct {
	// id is the session's ID in its workstation.
	id   string
	term *terminal.Terminal
	// agent is the Agent Executable of the agent that runs in the
	// session, as its workstation last said, empty for a plain shell's.
	// An Agent Session is a session that has one: it has a glyph in the
	// Header, and can be Hidden.
	agent string
	// agentEnded is whether the session's agent ended while the session
	// was Hidden: the session keeps its glyph, with the Attention mark,
	// until it is looked at, since what the agent last said is on its
	// screen. over is whether its shell ended there too: its terminal is
	// kept, for that screen, until then.
	agentEnded bool
	over       bool
	// attention is whether the session's agent raised its hand, or the
	// session rang or sent a notification, since the user last looked at
	// it. The workstation says the first, by the sandbox's Link; the
	// terminal says the others, and the app reads nothing else of what
	// a session prints: not the notification's words either.
	attention bool
}

// asks is whether the session wears the Attention mark: its agent raised
// its hand, it rang or sent a notification, or its agent ended, and it
// has not been looked at since.
func (s *session) asks() bool { return s.attention || s.agentEnded }

// pane is one Pane of a running sandbox's page: a place that shows one
// Terminal Session.
type pane struct {
	// key tells the Pane from the others of its sandbox for as long as
	// it is there, wherever it stands among them.
	key int
	// session is what the Pane shows, nil while its shell is being
	// started and after its shell ended.
	session *session
	// opening is whether a shell is being started for the Pane, and note
	// what the Pane says while it shows no session: that its shell
	// ended, or why none was started.
	opening bool
	note    string
	failed  bool
	// focus is the terminal's view, by which a session that just came
	// into the Pane is given the keyboard.
	focus ui.Handle
}

// What a Pane says while it shows no session.
const (
	paneOpening = "Starting a login shell…"
	paneEnded   = "The shell ended."
)

// What the controls of a Pane's strip that are not always there are
// called.
const (
	hideLabel      = "Hide"
	closePaneLabel = "Close Pane"
)

// newSessionLabel is what the control that starts a login shell in a
// Pane whose shell ended is called.
const newSessionLabel = "New Session"

// openFirstPane gives a sandbox whose workstation just started its
// first Pane, with a login shell in it.
func (v *sandboxView) openFirstPane() {
	if len(v.panes) == 0 {
		v.addPane()
	}
}

// addPane adds a Pane, with a login shell in it, where there is room
// for one.
func (v *sandboxView) addPane() {
	if len(v.panes) >= maxPanes || !running(v.status.State) {
		return
	}
	v.paneSeq++
	p := &pane{key: v.paneSeq}
	v.panes = append(v.panes, p)
	v.openShell(p)
}

// openShell starts a login shell in the sandbox's workstation, off the
// main thread, and shows it in a Pane: the backend starts the shell and
// connects it. What runs in it from there, its user types.
func (v *sandboxView) openShell(p *pane) {
	if p.opening || p.session != nil {
		return
	}
	d, id := v.d, v.id
	lived := v.lived
	p.opening, p.note, p.failed = true, "", false
	go func() {
		conn, err := d.backend.OpenTerminalSession(id)
		d.update(func() {
			p.opening = false
			if err != nil {
				p.note, p.failed = err.Error(), true
				return
			}
			// The sandbox stopped meanwhile, the Pane was closed, or a
			// session was shown in it: the shell that was started for it
			// is hung up.
			if v.lived != lived || !slices.Contains(v.panes, p) || p.session != nil {
				conn.Close()
				return
			}
			opts := terminal.Options{Conn: conn, Theme: terminalTheme}
			// Without the symbols' font a session still shows: the symbols
			// no font has are boxes then.
			if err := registerSymbolFont(); err == nil {
				opts.Font = terminalFont
			}
			// An agent that takes the whole screen paints it in a color of
			// its own: the room around the grid takes that color, so that
			// no frame of the window's stands around the agent's screen.
			opts.PaddingColor = terminal.PaddingExtend
			// The terminal says when its program rings or sends a
			// notification, on a goroutine of its own. What a notification
			// says is the program's words, and is not taken.
			var s *session
			asks := func() {
				d.update(func() {
					if s != nil {
						s.attention = true
					}
				})
			}
			opts.OnBell = asks
			opts.OnNotify = func(_, _ string) { asks() }
			term, err := d.newTerminal(opts)
			if err != nil {
				conn.Close()
				p.note, p.failed = err.Error(), true
				return
			}
			s = &session{id: conn.ID(), term: term}
			v.sessions = append(v.sessions, s)
			p.session = s
			// A session that just opened is where typing goes. The
			// request waits until the terminal's view is built.
			p.focus.Focus()
			// When its shell exits, the session is over: one that was
			// closed is gone by then.
			go func() {
				<-term.Done()
				d.update(func() { v.sessionEnded(s) })
			}()
		})
	}()
}

// sessionEnded takes note that a session's shell ended by itself. One
// that is Hidden stays, with its last screen and the Attention mark on
// its glyph, until it is looked at. One in a Pane goes: its Pane says
// so and offers another, and the sandbox asks for its user until it is
// looked at.
func (v *sandboxView) sessionEnded(s *session) {
	if !slices.Contains(v.sessions, s) {
		return // closed before, with its Pane or with its sandbox
	}
	if v.paneOf(s) == nil {
		s.over, s.agentEnded = true, true
		return
	}
	for _, p := range v.panes {
		if p.session == s {
			p.session, p.note = nil, paneEnded
		}
	}
	v.drop(s)
	v.ended = true
}

// paneOf is the Pane that shows a session, nil for one that is Hidden.
func (v *sandboxView) paneOf(s *session) *pane {
	for _, p := range v.panes {
		if p.session == s {
			return p
		}
	}
	return nil
}

// sessionFacts takes what the workstation says of its Terminal
// Sessions: which agent runs in each. A session whose agent ended while
// it was Hidden keeps its glyph until it is looked at.
func (v *sandboxView) sessionFacts(states []sandbox.SessionState) {
	said := map[string]string{}
	for _, state := range states {
		said[state.ID] = state.Agent
	}
	for _, s := range v.sessions {
		agent, reported := said[s.id]
		switch {
		case s.over || !reported:
			// Its shell ended, or the workstation has not seen it yet.
		case agent != "":
			s.agent, s.agentEnded = agent, false
		case s.agent != "" && v.paneOf(s) == nil:
			s.agentEnded = true
		default:
			s.agent, s.agentEnded = "", false
		}
	}
}

// raised takes note that the agent of a session raised its hand: the
// session asks for its user until it is looked at, as one that rang
// does. A hand of a session the dashboard does not hold is nobody's.
func (v *sandboxView) raised(sessionID string) {
	for _, s := range v.sessions {
		if s.id == sessionID {
			s.attention = true
		}
	}
}

// lose takes note that a session has no Pane any more: an Agent Session
// is Hidden, and goes on; a plain shell's ends, since nothing in the
// Header stands for it.
func (v *sandboxView) lose(s *session) {
	if s != nil && s.agent == "" {
		v.drop(s)
	}
}

// hide takes an Agent Session out of its Pane, which gets a login shell
// of its own: the session goes on, Hidden.
func (v *sandboxView) hide(p *pane) {
	if p.session == nil || p.session.agent == "" || p.session.over || !slices.Contains(v.panes, p) {
		return
	}
	p.session = nil
	v.openShell(p)
}

// show shows a session in a Pane. One that another Pane shows changes
// places with what this one shows. One that is Hidden takes the Pane,
// and what was there loses it.
func (v *sandboxView) show(s *session, target *pane) {
	if !slices.Contains(v.sessions, s) || !slices.Contains(v.panes, target) || target.session == s {
		return
	}
	if from := v.paneOf(s); from != nil {
		from.session, target.session = target.session, s
	} else {
		was := target.session
		target.session = s
		v.lose(was)
	}
	target.note, target.failed = "", false
	// The session that came is where typing goes.
	v.giveKeyboard(target)
}

// giveKeyboard has a Pane's terminal take the keyboard, once the Panes
// are built as they now stand: a session that just came into a Pane has
// its terminal's view there only from the next frame on.
func (v *sandboxView) giveKeyboard(p *pane) {
	v.d.update(func() { p.focus.Focus() })
}

// choose is a click on an Agent Session's glyph: one that is shown gets
// the keyboard, and one that is Hidden a Pane of its own, where there is
// room for another.
func (v *sandboxView) choose(s *session) {
	if !slices.Contains(v.sessions, s) {
		return
	}
	if p := v.paneOf(s); p != nil {
		v.giveKeyboard(p)
		return
	}
	if len(v.panes) >= maxPanes {
		return
	}
	v.paneSeq++
	p := &pane{key: v.paneSeq, session: s}
	v.panes = append(v.panes, p)
	v.giveKeyboard(p)
}

// newSession starts a login shell in a Pane that shows none, or whose
// session's shell ended.
func (v *sandboxView) newSession(p *pane) {
	if s := p.session; s != nil {
		if !s.over {
			return
		}
		p.session = nil
		v.drop(s)
	}
	v.openShell(p)
}

// drop ends a session and forgets it, where it is held.
func (v *sandboxView) drop(s *session) {
	at := slices.Index(v.sessions, s)
	if at < 0 {
		return
	}
	v.sessions = slices.Delete(v.sessions, at, at+1)
	s.term.Close()
}

// closePane takes a Pane away, where it is not the last: its session
// loses it.
func (v *sandboxView) closePane(p *pane) {
	at := slices.Index(v.panes, p)
	if at < 0 || len(v.panes) <= 1 {
		return
	}
	v.panes = slices.Delete(v.panes, at, at+1)
	v.lose(p.session)
}

// closeSessions ends every session of the sandbox and takes its Panes
// away, as when its workstation stops.
func (v *sandboxView) closeSessions() {
	for _, s := range v.sessions {
		s.term.Close()
	}
	v.sessions, v.panes = nil, nil
	v.lived++
}

// asks is whether a session of the sandbox rang or sent a notification
// and has not been looked at since, and agentEnded whether an agent
// ended out of sight.
func (v *sandboxView) asks() (rang, agentEnded bool) {
	for _, s := range v.sessions {
		rang = rang || s.attention
		agentEnded = agentEnded || s.agentEnded
	}
	return rang, agentEnded
}

// agentSessions are the sandbox's Agent Sessions, shown and Hidden, in
// the order they were opened.
func (v *sandboxView) agentSessions() []*session {
	var out []*session
	for _, s := range v.sessions {
		if s.agent != "" {
			out = append(out, s)
		}
	}
	return out
}

// panesView is the Panes of a running sandbox: one fills the page, two
// stand side by side, and from three on they stand in two rows, whose
// dividers are one line.
func (v *sandboxView) panesView(c *ui.Context) {
	// What a strip's controls ask is done once every Pane is built: a
	// Pane that closed would move the ones still to be built.
	var asked func()
	one := func(i int) func() {
		return func() {
			if ask := v.paneView(c, i); ask != nil {
				asked = ask
			}
		}
	}
	row := func(i, j int) func() {
		return func() { ui.SplitPercent(c, &v.columnShare, one(i), one(j)).SplitReset(50).Fill() }
	}
	switch len(v.panes) {
	case 0:
		v.note(c, paneOpening)
	case 1:
		one(0)()
	case 2:
		row(0, 1)()
	case 3:
		ui.SplitVerticalPercent(c, &v.rowShare, row(0, 1), one(2)).SplitReset(50).Fill()
	default:
		ui.SplitVerticalPercent(c, &v.rowShare, row(0, 1), row(2, 3)).SplitReset(50).Fill()
	}
	if asked != nil {
		asked()
	}
}

// paneName is what a Pane is called, by where it stands.
func paneName(i int) string { return fmt.Sprintf("Pane %d", i+1) }

// paneView is one Pane: a strip that names it, says which agent runs in
// its session, and holds its controls, over the terminal of its
// session. A Pane whose shell ended says so, and its strip offers
// another. A glyph dragged from the Header is dropped on a Pane, which
// shows a wash of the accent color while one is over it. It returns
// what was asked of it, to be done once the Panes are built, or nil.
func (v *sandboxView) paneView(c *ui.Context, i int) func() {
	t := c.Theme()
	p := v.panes[i]
	name := paneName(i)
	n := fmt.Sprint(i + 1)
	// Its label says which sandbox it is of, for whoever does not see
	// where it stands.
	of := name
	if cfg := v.d.config(v.id); cfg != nil {
		of = name + " of " + cfg.Name
	}
	var closing, opening, hiding bool
	container := ui.Column(c).Key(fmt.Sprintf("pane-%d", p.key)).Fill().Label(of)
	_, over := ui.DragOver[*session](container)
	dropped, took := ui.Drop[*session](container)
	container.Children(func() {
		ui.Row(c).Shrink(0).FillWidth().Height(stripHeight).Gap(t.Space(0.5)).Padding(0, t.Space(1.5), 0, t.Space(3)).AlignItems(ui.Center).
			Font(chromeFont).Background(panelTheme.Background).BorderWidth(0, 0, 1, 0).BorderColor(t.Border).Children(func() {
			ui.Text(c, name).MaxLines(1).TextColor(t.TextMuted).Shrink(0)
			// The agent that runs in the Pane's session, in the app's own
			// words: its name as the app knows it.
			if s := p.session; s != nil && s.agent != "" {
				ui.Text(c, agentTitle(s.agent)).MaxLines(1).Shrink(1).Margin(0, 0, 0, t.Space(2))
			}
			ui.Spacer(c)
			if (p.session == nil && !p.opening) || (p.session != nil && p.session.over) {
				opening = glyphButtonIn(c, "new-session-"+n, glyphAdd, newSessionLabel+" in "+name, newSessionLabel, t.Text).Clicked()
			}
			if s := p.session; s != nil && s.agent != "" && !s.over {
				hiding = glyphButtonIn(c, "hide-"+n, glyphHide, hideLabel+" the session of "+name, hideLabel+": its session goes on, behind its glyph in the Header", t.Text).Clicked()
			}
			if len(v.panes) > 1 {
				closing = glyphButtonIn(c, "close-pane-"+n, glyphClose, "Close "+name, closePaneLabel, t.Text).Clicked()
			}
		})
		switch {
		case p.session != nil:
			c.Key("session-" + p.session.id)
			terminal.View(c, p.session.term).Grow(1).FillWidth().Bind(&p.focus)
		case p.opening:
			v.note(c, paneOpening)
		default:
			color := t.TextMuted
			if p.failed {
				color = t.Danger
			}
			ui.Column(c).Grow(1).FillWidth().Center().Padding(t.Space(3)).Font(chromeFont).Children(func() {
				ui.Text(c, p.note).FontSize(textSmall).TextColor(color)
			})
		}
	})
	if over {
		// A glyph is over the Pane, which would take it: a wash of the
		// accent color, over its terminal.
		wash := t.Accent.Alpha(0.22)
		container.DrawOver(func(painter *ui.Painter, r ui.Rect) { painter.Fill(r, wash, 0) })
	}
	switch {
	case took:
		return func() { v.show(dropped, p) }
	case closing:
		return func() { v.closePane(p) }
	case hiding:
		return func() { v.hide(p) }
	case opening:
		return func() { v.newSession(p) }
	}
	return nil
}
