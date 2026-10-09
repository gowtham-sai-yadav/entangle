// Package tui is the interactive terminal cockpit for entangle. It is a
// thin Bubble Tea front-end over the same building blocks the CLI uses
// (exporter, transfer, importer, updater), so every feature is reachable
// without remembering a single flag: browse your sessions, hand one to a
// teammate over an encrypted code, share it to a file, and export/import/update
// - all from one screen.
//
// Nothing here talks to disk or the network directly except through those
// packages, and it never prints to stdout itself (that would tear the rendered
// screen). Slow or streaming work runs in a goroutine that pushes typed
// messages onto a channel; a small "listen" command feeds them back into the
// Elm-style update loop, so the UI stays responsive and there are no mutexes.
package tui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	figure "github.com/common-nighthawk/go-figure"

	"github.com/gowtham-sai-yadav/entangle/internal/agent"
	_ "github.com/gowtham-sai-yadav/entangle/internal/agent/claudecode" // registers the Claude Code provider
	_ "github.com/gowtham-sai-yadav/entangle/internal/agent/cline"      // registers the Cline provider
	_ "github.com/gowtham-sai-yadav/entangle/internal/agent/codex"      // registers the Codex CLI provider
	_ "github.com/gowtham-sai-yadav/entangle/internal/agent/opencode"   // registers the opencode provider
	"github.com/gowtham-sai-yadav/entangle/internal/agentshare"
	"github.com/gowtham-sai-yadav/entangle/internal/claudedir"
	"github.com/gowtham-sai-yadav/entangle/internal/exporter"
	"github.com/gowtham-sai-yadav/entangle/internal/handoff"
	"github.com/gowtham-sai-yadav/entangle/internal/importer"
	"github.com/gowtham-sai-yadav/entangle/internal/transfer"
	"github.com/gowtham-sai-yadav/entangle/internal/updater"
)

// ---- palette --------------------------------------------------------------
// The same cosmic/coral identity as the landing page, mapped to the terminal.

// writeClipboard is a variable so a test can read back what the copy key actually
// put on the clipboard. Calling the real one in tests would clobber whatever the
// person running them had copied, and fails outright on a machine with no
// clipboard at all.
var writeClipboard = clipboard.WriteAll

var (
	signal = lipgloss.Color("#ff5a36") // coral - the one accent
	paper  = lipgloss.Color("#ece7dd") // bright text
	dim    = lipgloss.Color("#9a94aa") // secondary text
	muted  = lipgloss.Color("#6b6676") // faint text / rules
	okGrn  = lipgloss.Color("#7ee0a8") // success
	errRed = lipgloss.Color("#ff6b6b") // failure
	warnAm = lipgloss.Color("#ffb454") // advisory
)

var (
	bannerStyle  = lipgloss.NewStyle().Foreground(signal).Bold(true)
	taglineStyle = lipgloss.NewStyle().Foreground(paper)
	accentStyle  = lipgloss.NewStyle().Foreground(signal)
	dimStyle     = lipgloss.NewStyle().Foreground(dim)
	mutedStyle   = lipgloss.NewStyle().Foreground(muted)
	okStyle      = lipgloss.NewStyle().Foreground(okGrn)
	errStyle     = lipgloss.NewStyle().Foreground(errRed).Bold(true)
	warnStyle    = lipgloss.NewStyle().Foreground(warnAm)
	cardStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(muted).Padding(1, 3)
	codeStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(signal).
			Foreground(signal).Bold(true).Padding(1, 4)
	labelStyle = lipgloss.NewStyle().Foreground(dim).Width(9)
)

// ---- modes ----------------------------------------------------------------

type mode int

const (
	modeList      mode = iota // home: banner + session list
	modeConfirm               // preview a session before send/share
	modeSend                  // streaming a session over a wormhole
	modeRecvCode              // typing the code to receive
	modeReceive               // pulling + importing an incoming session
	modeInputPath             // typing a bundle path to import
	modeBusy                  // export / import-from-file / update working
	modeUpdate                // an update is available; confirm install
	modeResult                // a finished operation's summary (any key = back)
	modeHelp                  // full explanation of every key (any key = back)
)

// ---- session list item ----------------------------------------------------

type sessionItem struct {
	s agent.Session
	// showTool is set when more than one coding tool was found, so someone using
	// only Claude Code sees exactly the row they saw before.
	showTool bool
}

func (i sessionItem) Title() string {
	t := strings.TrimSpace(i.s.Title)
	if t == "" {
		t = "(untitled session)"
	}
	return t
}

func (i sessionItem) Description() string {
	proj := i.s.ProjectPath
	if proj == "" {
		proj = "(unknown project)"
	} else {
		proj = filepath.Base(proj)
	}
	line := fmt.Sprintf("%s · %d msgs · %s · %s",
		i.s.ShortID, i.s.Messages, humanAgo(i.s.ModTime), proj)
	if i.showTool {
		line = string(i.s.Provider) + "  " + line
	}
	return line
}

func (i sessionItem) FilterValue() string {
	// The tool name is searchable, so "/codex" narrows the list to one tool.
	return i.s.Title + " " + i.s.ID + " " + i.s.ProjectPath + " " + string(i.s.Provider)
}

// ---- model ----------------------------------------------------------------

type model struct {
	mode          mode
	width, height int

	// roots holds where each installed tool keeps its data, filled in when the
	// session list loads. configDir is the --config-dir override, which has always
	// meant the Claude directory.
	roots     map[agent.ID]agent.Roots
	configDir string
	version   string
	tcfg      transfer.Config

	// newVersion is a newer release the update check already found, or "". It sits
	// in the header rather than in notice, because notice is cleared whenever an
	// operation starts and this is true for the whole session.
	newVersion string

	// The full listing is kept so the tool filter can be changed without going
	// back to disk: re-reading would cost a visible pause on every press of `t`,
	// and would let the list shift under the cursor between views.
	//
	// present is the tools actually found here, in registry order. It doubles as
	// the cycle order for `t` and as the answer to "is more than one tool in play",
	// which decides whether rows carry a tool name at all.
	allSessions []agent.Session
	present     []agent.ID
	filter      agent.ID // "" means every tool

	banner   string // pre-rendered ascii wordmark
	bannerOK bool   // banner fits the current width

	list     list.Model
	spinner  spinner.Model
	progress progress.Model
	input    textinput.Model

	withContext bool // include project memory in a share/send (off by default)

	// operation context
	prepped  *prepared
	forShare bool // the prepared bundle is for a file, not a wormhole

	// streaming async plumbing
	events chan tea.Msg
	cancel context.CancelFunc
	code   string
	copied string // transient result of a copy attempt, shown under the code
	status string
	notice string // sticky warning for the current operation (e.g. server fallback)
	done   int64
	total  int64

	// update flow
	updLatest string
	// restartTo is the binary to hand this terminal to on the way out, set once a
	// self-update has finished. Read by Run after the program exits, because the
	// handover can only happen with the terminal already given back.
	restartTo string

	// result screen
	resTitle string
	resBody  string
	resErr   bool

	loadErr error
}

// ---- messages -------------------------------------------------------------

type sessionsMsg struct {
	sessions []agent.Session
	present  []agent.ID
	roots    map[agent.ID]agent.Roots
	problems []string
	err      error
}
type preppedMsg struct {
	b   *prepared
	err error
}

// prepared is one session captured in memory, ready to be written to a file or
// streamed, with the tool it came from remembered. It exists so the confirm
// screen and both send paths do not have to know which tool produced it.
type prepared struct {
	provider agent.ID
	name     string
	preview  agent.Preview
	write    func(io.Writer) error
	// withContext is only meaningful for Claude Code, whose bundles can carry the
	// project's memory files.
	withContext bool
}
type codeMsg string
type statusMsg string
type noticeMsg string
type progressMsg struct{ done, total int64 }
type doneMsg struct {
	title string
	body  string
	err   error
	// restart, when set, is the path of a binary that should take over this
	// terminal once the user says so. Only a finished self-update sets it.
	restart string
}
type updateAvailMsg struct {
	latest string
	newer  bool
	err    error
}

// ---- entry point ----------------------------------------------------------

// Run launches the interactive cockpit. configDir mirrors the --config-dir flag
// the other commands accept; "" means the default (~/.claude).
//
// newVersion is a newer release already found by the update check, or "". It is
// passed in rather than looked up here so the cockpit never makes a network
// request of its own: the answer is whatever the last run cached.
func Run(configDir string, tcfg transfer.Config, version, newVersion string) error {
	m := newModel(configDir, tcfg, version, newVersion)
	prog := tea.NewProgram(m, tea.WithAltScreen())
	final, err := prog.Run()
	if err != nil {
		return err
	}
	// Out here rather than inside the model on purpose: handing the process over
	// while bubbletea still owns the tty leaves the new binary drawing into an alt
	// screen it did not open and a terminal still in raw mode. By this line the
	// terminal has been restored, so the new build starts as if launched by hand.
	if fm, ok := final.(model); ok && fm.restartTo != "" {
		return restartInto(fm.restartTo)
	}
	return nil
}

func newModel(configDir string, tcfg transfer.Config, version, newVersion string) model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = accentStyle

	pr := progress.New(progress.WithSolidFill(string(signal)), progress.WithoutPercentage())

	in := textinput.New()
	in.Prompt = accentStyle.Render("› ")
	in.Cursor.Style = accentStyle

	d := list.NewDefaultDelegate()
	d.Styles.SelectedTitle = d.Styles.SelectedTitle.Foreground(signal).BorderForeground(signal).Bold(true)
	d.Styles.SelectedDesc = d.Styles.SelectedDesc.Foreground(signal).BorderForeground(signal)
	d.Styles.NormalTitle = d.Styles.NormalTitle.Foreground(paper)
	d.Styles.NormalDesc = d.Styles.NormalDesc.Foreground(muted)

	l := list.New(nil, d, 0, 0)
	l.Title = "your sessions"
	l.Styles.Title = lipgloss.NewStyle().Foreground(paper).Bold(true)
	l.SetShowStatusBar(true)
	l.SetStatusBarItemName("session", "sessions")
	l.SetShowHelp(false) // we draw our own footer
	l.SetFilteringEnabled(true)
	l.FilterInput.Cursor.Style = accentStyle
	l.FilterInput.PromptStyle = accentStyle

	return model{
		mode:       modeList,
		configDir:  configDir,
		version:    version,
		newVersion: newVersion,
		tcfg:       tcfg,
		banner:     figure.NewFigure("entangle", "small", true).String(),
		bannerOK:   true,
		list:       l,
		spinner:    sp,
		progress:   pr,
		input:      in,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(loadSessions(m.configDir), m.spinner.Tick)
}

// ---- commands (side effects live here) ------------------------------------

// loadSessions gathers sessions from every coding tool installed on this machine.
//
// The sweep itself lives in agent.Scan because `entangle gui` shows the same list
// in a browser, and two copies of that loop would drift apart.
func loadSessions(configDir string) tea.Cmd {
	return func() tea.Msg {
		inv := agent.Scan(configDir)
		return sessionsMsg{
			sessions: inv.Sessions,
			present:  inv.Present,
			roots:    inv.Roots,
			problems: inv.Problems,
		}
	}
}

// prepareShare captures the selected session in memory, using whichever path
// belongs to its tool. Claude Code keeps its original exporter, whose bundle
// layout every released binary reads; every other tool goes through agentshare.
func prepareShare(m model, s agent.Session) tea.Cmd {
	withContext := m.withContext
	configDir, version := m.configDir, m.version
	roots := m.roots[s.Provider]

	return func() tea.Msg {
		if s.Provider == agent.ClaudeCode {
			b, err := exporter.PrepareShare(exporter.ShareOptions{
				ConfigDir:     configDir,
				Version:       version,
				SessionPrefix: s.ID,
				WithContext:   withContext,
				Redact:        true,
			})
			if err != nil {
				return preppedMsg{err: err}
			}
			return preppedMsg{b: &prepared{
				provider: agent.ClaudeCode,
				name:     b.Name,
				preview: agent.Preview{
					Title:         b.Preview.Title,
					ShortID:       b.Preview.ShortID,
					ProjectPath:   b.Preview.ProjectPath,
					Messages:      b.Preview.Messages,
					Bytes:         b.Preview.Bytes,
					SecretsMasked: b.Preview.SecretsMasked,
				},
				write:       b.WriteBundle,
				withContext: withContext,
			}}
		}

		p, ok := agent.Get(s.Provider)
		if !ok {
			return preppedMsg{err: fmt.Errorf("unknown tool %q", s.Provider)}
		}
		ab, err := agentshare.Pack(p, roots, s, agentshare.Options{ToolVersion: version, Redact: true})
		if err != nil {
			return preppedMsg{err: err}
		}
		return preppedMsg{b: &prepared{
			provider: s.Provider,
			name:     ab.Name,
			preview:  ab.Preview,
			write:    ab.WriteBundle,
		}}
	}
}

// waitForEvent yields the next message a running operation pushed onto the
// channel, so streamed progress flows back into Update one frame at a time.
func waitForEvent(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

// emit blocks; used for rare, must-not-drop messages (code, status, done).
func emit(ch chan tea.Msg, msg tea.Msg) { ch <- msg }

// Switching transfer servers is deliberately not surfaced. It needs nothing from
// the user - receiving tries both mailboxes at once, so the two sides meet either
// way - and an amber banner about infrastructure reads like something went wrong
// during the one moment the user is watching for a code. If both servers fail, the
// error says so; that is when it matters.

// emitProgress never blocks the transfer goroutine: if the UI is behind, we
// simply skip a frame rather than stalling the bytes.
func emitProgress(ch chan tea.Msg, done, total int64) {
	select {
	case ch <- progressMsg{done, total}:
	default:
	}
}

// startSend streams the prepared bundle over a wormhole, reporting the code and
// progress as they happen.
func startSend(ch chan tea.Msg, ctx context.Context, cfg transfer.Config, b *prepared) tea.Cmd {
	return func() tea.Msg {
		go func() {
			var buf bytes.Buffer
			if err := b.write(&buf); err != nil {
				emit(ch, doneMsg{err: err})
				return
			}
			err := transfer.Send(ctx, cfg, b.name, buf.Bytes(),
				func(code string) { emit(ch, codeMsg(code)) },
				func(done, total int64) { emitProgress(ch, done, total) },
			)
			if err != nil {
				emit(ch, doneMsg{err: err})
				return
			}
			emit(ch, doneMsg{
				title: "Sent.",
				body: "The session is on your teammate's machine.\nThey open " +
					toolName(b.provider) + " in their project and carry on.",
			})
		}()
		return nil
	}
}

// toolName is the human name of a tool, falling back to its id.
func toolName(id agent.ID) string {
	if p, ok := agent.Get(id); ok {
		return p.DisplayName()
	}
	return string(id)
}

// startReceive pulls an incoming bundle and imports it into the current
// directory's project, exactly as the receive command does.
func startReceive(ch chan tea.Msg, ctx context.Context, cfg transfer.Config, configDir, code string) tea.Cmd {
	return func() tea.Msg {
		go func() {
			emit(ch, statusMsg("Connecting…"))
			in, err := transfer.Receive(ctx, cfg, code)
			if err != nil {
				emit(ch, doneMsg{err: fmt.Errorf("receive: %w", err)})
				return
			}
			tmp, err := os.CreateTemp("", "entangle-recv-*.tgz")
			if err != nil {
				emit(ch, doneMsg{err: err})
				return
			}
			tmpPath := tmp.Name()
			defer os.Remove(tmpPath)

			emit(ch, statusMsg("Receiving…"))
			const maxBytes = 2 << 30
			if _, err := copyCapped(ch, tmp, in, in.Bytes, maxBytes); err != nil {
				tmp.Close()
				emit(ch, doneMsg{err: fmt.Errorf("receive: %w", err)})
				return
			}
			if err := tmp.Close(); err != nil {
				emit(ch, doneMsg{err: err})
				return
			}

			emit(ch, statusMsg("Importing…"))
			summary, err := runImport(configDir, tmpPath)
			if err != nil {
				emit(ch, doneMsg{err: err})
				return
			}
			emit(ch, doneMsg{title: "Received.", body: summary})
		}()
		return nil
	}
}

// startExport writes a full portable backup of every session.
func startExport(ch chan tea.Msg, configDir, version string) tea.Cmd {
	return func() tea.Msg {
		go func() {
			res, err := exporter.Run(exporter.Options{ConfigDir: configDir, Version: version})
			if err != nil {
				emit(ch, doneMsg{err: err})
				return
			}
			body := fmt.Sprintf("%d project(s), %d session(s) → %s\n(%s)",
				res.Projects, res.Sessions, res.Path, exporter.HumanSize(res.Bytes))
			emit(ch, doneMsg{title: "Exported.", body: body})
		}()
		return nil
	}
}

// startImportFile imports a bundle chosen by path.
func startImportFile(ch chan tea.Msg, configDir, path string) tea.Cmd {
	return func() tea.Msg {
		go func() {
			summary, err := runImport(configDir, path)
			if err != nil {
				emit(ch, doneMsg{err: err})
				return
			}
			emit(ch, doneMsg{title: "Imported.", body: summary})
		}()
		return nil
	}
}

// startWriteShare writes the prepared bundle to a file for a teammate.
func startWriteShare(ch chan tea.Msg, b *prepared) tea.Cmd {
	return func() tea.Msg {
		go func() {
			f, err := os.Create(b.name)
			if err != nil {
				emit(ch, doneMsg{err: err})
				return
			}
			if err := b.write(f); err != nil {
				f.Close()
				emit(ch, doneMsg{err: err})
				return
			}
			if err := f.Close(); err != nil {
				emit(ch, doneMsg{err: err})
				return
			}
			abs, _ := filepath.Abs(b.name)
			emit(ch, doneMsg{
				title: "Shared to a file.",
				body: fmt.Sprintf("Wrote %s\nSend it to your teammate; they run:\n  entangle import %s\n(they need %s installed)",
					abs, b.name, toolName(b.provider)),
			})
		}()
		return nil
	}
}

func checkUpdate(version string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		latest, err := updater.LatestVersion(ctx, updater.DefaultRepo)
		if err != nil {
			return updateAvailMsg{err: err}
		}
		return updateAvailMsg{latest: strings.TrimPrefix(latest, "v"), newer: updater.Newer(latest, version)}
	}
}

func startUpdate(ch chan tea.Msg, tag string) tea.Cmd {
	return func() tea.Msg {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			emit(ch, statusMsg("Downloading…"))
			// Resolved before the swap, not after. Apply renames the new build over
			// this path, and on Linux asking afterwards follows the old inode and can
			// hand back a path with "(deleted)" stuck on the end - which would then be
			// exec'd, putting the user back on the version they just replaced.
			exe, exeErr := os.Executable()
			err := updater.Apply(ctx, updater.DefaultRepo, "v"+strings.TrimPrefix(tag, "v"),
				func(done, total int64) { emitProgress(ch, done, total) })
			if err != nil {
				emit(ch, doneMsg{err: err})
				return
			}
			version := strings.TrimPrefix(tag, "v")
			if exeErr != nil {
				// Rare, but the update itself did land, so report that plainly rather
				// than offering a handover to a path that could not be resolved.
				emit(ch, doneMsg{title: "Updated.", body: "Now on " + version + ". Restart to use the new version."})
				return
			}
			emit(ch, doneMsg{title: "Updated.", body: "Now on " + version + ".", restart: exe})
		}()
		return nil
	}
}

// runImport applies a bundle and returns a human summary.
//
// The sender's tool is recorded in the manifest, so the receiver never has to say
// which one it was: a Claude bundle goes through the original importer, and any
// other tool's session goes to that tool's provider.
func runImport(configDir, bundlePath string) (string, error) {
	_, foreign, err := agentshare.PeekAgent(bundlePath)
	if err != nil {
		return "", err
	}
	if foreign {
		// A shared session attaches to the folder the user is standing in, the same
		// rule Claude shared sessions follow.
		targetDir, werr := os.Getwd()
		if werr != nil {
			return "", werr
		}
		res, uerr := agentshare.Unpack(bundlePath, targetDir, configDir)
		if uerr != nil {
			return "", uerr
		}
		body := fmt.Sprintf("Imported one %s session into\n  %s", res.DisplayName, targetDir)
		if res.ResumeHint != "" {
			body += "\n\nCarry on with:\n  " + res.ResumeHint
		}
		return body, nil
	}

	p, err := claudedir.Locate(configDir)
	if err != nil {
		return "", err
	}
	man, err := importer.LoadManifest(bundlePath)
	if err != nil {
		return "", err
	}
	// BuildPlan/Import take the already-located paths directly, so ConfigDir on
	// Options is irrelevant here (it only matters to the printing importer.Run).
	opts := importer.Options{Bundle: bundlePath, AssumeYes: true}
	plan := importer.BuildPlan(man, p, opts)
	res, err := importer.Import(p, plan, opts)
	if err != nil {
		return "", err
	}
	ok := 0
	for _, v := range res.Verify {
		if v.OK {
			ok++
		}
	}
	return fmt.Sprintf("%d file(s) written, %d skipped, %d project(s) merged.\n%d/%d project(s) resume-ready.\nOpen Claude Code in this folder to continue.",
		res.Written, res.Skipped, res.MergedProjects, ok, len(res.Verify)), nil
}

// copyCapped streams src→dst, refusing to write more than limit bytes and
// pushing progress onto ch.
func copyCapped(ch chan tea.Msg, dst io.Writer, src io.Reader, total, limit int64) (int64, error) {
	buf := make([]byte, 32*1024)
	var done int64
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if done+int64(n) > limit {
				return done, fmt.Errorf("incoming bundle exceeds the %s safety cap", exporter.HumanSize(limit))
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return done, werr
			}
			done += int64(n)
			emitProgress(ch, done, total)
		}
		if rerr == io.EOF {
			return done, nil
		}
		if rerr != nil {
			return done, rerr
		}
	}
}

// ---- update loop ----------------------------------------------------------

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.bannerOK = lipgloss.Width(m.banner) <= m.width
		m.layout()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case sessionsMsg:
		if msg.err != nil {
			m.loadErr = msg.err
			return m, nil
		}
		m.roots, m.present, m.allSessions = msg.roots, msg.present, msg.sessions
		m.rebuildList()
		if len(msg.problems) > 0 {
			// Say which tool could not be read rather than quietly showing a short
			// list the user has no way to question.
			m.notice = "Could not read some sessions:\n  " + strings.Join(msg.problems, "\n  ")
		}
		return m, nil

	case preppedMsg:
		if msg.err != nil {
			return m.toResult("Could not prepare that session.", msg.err.Error(), true), nil
		}
		m.prepped = msg.b
		m.mode = modeConfirm
		return m, nil

	case codeMsg:
		m.code = string(msg)
		return m, waitForEvent(m.events)

	case statusMsg:
		m.status = string(msg)
		return m, waitForEvent(m.events)

	case noticeMsg:
		m.notice = string(msg)
		return m, waitForEvent(m.events)

	case progressMsg:
		m.done, m.total = msg.done, msg.total
		return m, waitForEvent(m.events)

	case updateAvailMsg:
		if msg.err != nil {
			return m.toResult("Could not check for updates.", msg.err.Error(), true), nil
		}
		if !msg.newer {
			return m.toResult("Up to date.", "You are on the latest version ("+m.version+").", false), nil
		}
		m.updLatest = msg.latest
		m.mode = modeUpdate
		return m, nil

	case doneMsg:
		m.clearOp()
		if msg.err != nil {
			return m.toResult("That didn't work.", msg.err.Error(), true), nil
		}
		nm := m.toResult(msg.title, msg.body, false)
		nm.restartTo = msg.restart
		return nm, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Route anything else to whichever child owns the current screen.
	return m.routeToChild(msg)
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Ctrl-C always quits (cancelling any transfer in flight first).
	if key == "ctrl+c" {
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit
	}

	switch m.mode {

	case modeList:
		// While typing in the filter, the list owns every key.
		if m.list.FilterState() == list.Filtering {
			return m.routeToChild(msg)
		}
		switch key {
		case "q", "esc":
			return m, tea.Quit
		case "enter":
			if it, ok := m.selected(); ok {
				m.forShare = false
				return m, prepareShare(m, it.s)
			}
			return m, nil
		case "s":
			if it, ok := m.selected(); ok {
				m.forShare = true
				return m, prepareShare(m, it.s)
			}
			return m, nil
		case "r":
			return m.startInput(modeRecvCode), nil
		case "i":
			return m.startInput(modeInputPath), nil
		case "e":
			nm := m.enterBusy("Exporting every session…")
			return nm, tea.Batch(startExport(nm.events, nm.configDir, nm.version), waitForEvent(nm.events), m.spinner.Tick)
		case "u":
			m.mode = modeBusy
			m.status = "Checking for updates…"
			return m, tea.Batch(checkUpdate(m.version), m.spinner.Tick)
		case "c":
			m.withContext = !m.withContext
			return m, nil
		case "t":
			m.cycleTool()
			m.layout() // the header line may have grown or shrunk
			return m, nil
		case "?":
			m.mode = modeHelp
			return m, nil
		}
		return m.routeToChild(msg)

	case modeConfirm:
		switch key {
		case "enter":
			if m.forShare {
				nm := m.enterBusy("Writing the file…")
				return nm, tea.Batch(startWriteShare(nm.events, m.prepped), waitForEvent(nm.events), m.spinner.Tick)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			m.cancel = cancel
			m.events = make(chan tea.Msg, 64)
			m.mode, m.status, m.code, m.copied, m.notice, m.done, m.total = modeSend, "Opening a secure channel…", "", "", "", 0, 0
			return m, tea.Batch(startSend(m.events, ctx, m.tcfg, m.prepped), waitForEvent(m.events), m.spinner.Tick)
		case "esc", "q":
			m.prepped = nil
			m.mode = modeList
			return m, nil
		}
		return m, nil

	case modeSend, modeReceive, modeBusy:
		// Copies the whole invite, not just the code. Reading three words aloud is
		// the nice case; the common one is pasting into chat, and the person on the
		// other end may not have entangle at all. A bare code leaves them to be
		// talked through an install by hand, so the clipboard carries the install
		// line and where to run it.
		if key == "c" && m.code != "" {
			if err := writeClipboard(handoff.Invite(m.code)); err != nil {
				// No clipboard is normal over SSH or in a bare container, so say what
				// to do instead rather than presenting it as a failure.
				m.copied = "no clipboard here - select the code above to copy it"
			} else {
				m.copied = "copied - paste it to them"
			}
			return m, nil
		}
		if key == "esc" {
			if m.cancel != nil {
				m.cancel()
			}
			return m, nil // the goroutine will report the cancellation as a doneMsg
		}
		return m, nil

	case modeRecvCode, modeInputPath:
		switch key {
		case "esc":
			m.mode = modeList
			m.input.Blur()
			return m, nil
		case "enter":
			val := strings.TrimSpace(m.input.Value())
			if val == "" {
				return m, nil
			}
			m.input.Blur()
			if m.mode == modeRecvCode {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
				m.cancel = cancel
				m.events = make(chan tea.Msg, 64)
				m.mode, m.status, m.notice, m.done, m.total = modeReceive, "Connecting…", "", 0, 0
				return m, tea.Batch(startReceive(m.events, ctx, m.tcfg, m.configDir, val), waitForEvent(m.events), m.spinner.Tick)
			}
			nm := m.enterBusy("Importing " + filepath.Base(val) + "…")
			return nm, tea.Batch(startImportFile(nm.events, nm.configDir, val), waitForEvent(nm.events), m.spinner.Tick)
		}
		return m.routeToChild(msg)

	case modeUpdate:
		switch key {
		case "enter":
			nm := m.enterBusy("Downloading " + m.updLatest + "…")
			return nm, tea.Batch(startUpdate(nm.events, m.updLatest), waitForEvent(nm.events), m.spinner.Tick)
		case "esc", "q":
			m.mode = modeList
			return m, nil
		}
		return m, nil

	case modeResult:
		// A finished update is the one result worth acting on. The new build is
		// already on disk, but this process is still the old one, so without this the
		// only way to see the version you just installed is to quit and come back -
		// which is where updates quietly don't happen.
		//
		// Quit rather than exec here: the handover has to wait until bubbletea has
		// put the terminal back, so Run does it once this returns.
		if m.restartTo != "" && key == "enter" {
			return m, tea.Quit
		}
		// Any other key returns home, still running the old build. Clearing restartTo
		// matters - leaving it set would exec on the next unrelated quit, long after
		// the person declined.
		m.mode = modeList
		m.resTitle, m.resBody, m.resErr = "", "", false
		m.restartTo = ""
		return m, nil

	case modeHelp:
		// Any key returns home.
		m.mode = modeList
		return m, nil
	}
	return m, nil
}

// routeToChild forwards a message to whatever component owns the active screen.
func (m model) routeToChild(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch m.mode {
	case modeList:
		m.list, cmd = m.list.Update(msg)
	case modeRecvCode, modeInputPath:
		m.input, cmd = m.input.Update(msg)
	}
	return m, cmd
}

// ---- small state helpers --------------------------------------------------

func (m model) selected() (sessionItem, bool) {
	it, ok := m.list.SelectedItem().(sessionItem)
	return it, ok
}

// rebuildList refills the list from allSessions, honouring the current tool
// filter. The list is rebuilt rather than hidden row by row so that the count in
// the title, the scrollbar, and the built-in `/` search all agree with what is on
// screen.
func (m *model) rebuildList() {
	items := make([]list.Item, 0, len(m.allSessions))
	for _, s := range m.allSessions {
		if m.filter != "" && s.Provider != m.filter {
			continue
		}
		// Naming the tool is only useful when the row could have come from more than
		// one: filtered to a single tool, the header already says which, and someone
		// who only has Claude Code sees exactly the row they always saw.
		items = append(items, sessionItem{s: s, showTool: len(m.present) > 1 && m.filter == ""})
	}
	m.list.SetItems(items)
	// Rows changed height and count, so an old cursor position can land past the
	// end. Start at the newest session, which is what the user came for anyway.
	m.list.Select(0)
}

// cycleTool advances the filter one step: all tools, then each installed tool in
// turn, then back to all. Cycling beats a picker here because there are only ever
// a handful of tools and the whole point is a glance at another one and back.
func (m *model) cycleTool() {
	if len(m.present) < 2 {
		return // nothing to cycle between
	}
	if m.filter == "" {
		m.filter = m.present[0]
	} else {
		at := len(m.present) // not found -> fall back to "all"
		for i, id := range m.present {
			if id == m.filter {
				at = i
				break
			}
		}
		if at+1 < len(m.present) {
			m.filter = m.present[at+1]
		} else {
			m.filter = ""
		}
	}
	m.rebuildList()
}

// filterLabel names the active filter for the header and the key hint.
//
// It uses the provider's id, not its display name, so the label matches what the
// rows underneath it already say. A list of rows reading `codex` sitting under a
// filter reading "OpenAI Codex CLI" looks like two different things, and the id
// is also what the --tool flag takes, so this is the name worth learning. Display
// names still belong on the detail cards, where there is room to be formal.
func (m model) filterLabel() string {
	if m.filter == "" {
		return "all tools"
	}
	return string(m.filter)
}

func (m *model) startInput(md mode) model {
	m.mode = md
	m.input.SetValue("")
	if md == modeRecvCode {
		m.input.Placeholder = "7-crossover-marbles"
	} else {
		m.input.Placeholder = "path/to/entangle-session.tgz"
	}
	m.input.Focus()
	return *m
}

// enterBusy switches to the spinner screen and opens a fresh event channel.
func (m *model) enterBusy(status string) model {
	m.mode = modeBusy
	m.status = status
	m.notice = ""
	m.done, m.total = 0, 0
	m.events = make(chan tea.Msg, 64)
	return *m
}

func (m *model) clearOp() {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.code, m.copied, m.status, m.notice, m.done, m.total = "", "", "", "", 0, 0
	m.prepped = nil
}

func (m model) toResult(title, body string, isErr bool) model {
	m.mode = modeResult
	m.resTitle, m.resBody, m.resErr = title, body, isErr
	return m
}

// layout keeps the list sized to whatever room the header and footer leave.
func (m *model) layout() {
	if m.width == 0 {
		return
	}
	headerH := lipgloss.Height(m.headerView())
	footerH := lipgloss.Height(m.footerView())
	h := m.height - headerH - footerH - 1
	if h < 3 {
		h = 3
	}
	m.list.SetSize(m.width, h)
}

// ---- views ----------------------------------------------------------------

func (m model) View() string {
	switch m.mode {
	case modeList:
		if m.loadErr != nil {
			return m.headerView() + "\n\n" + errStyle.Render("Could not read your sessions: ") + m.loadErr.Error() + "\n"
		}
		return lipgloss.JoinVertical(lipgloss.Left, m.headerView(), m.list.View(), m.footerView())
	case modeConfirm:
		return m.centered(m.confirmView())
	case modeSend:
		return m.centered(m.sendView())
	case modeRecvCode:
		return m.centered(m.inputView("Receive a session", "Enter the code your teammate read out:", "It imports into this folder's project."))
	case modeReceive:
		return m.centered(m.transferView("Receiving a session"))
	case modeInputPath:
		return m.centered(m.inputView("Import a bundle", "Path to a .tgz bundle:", "A file exported or shared with you."))
	case modeBusy:
		return m.centered(m.busyView())
	case modeUpdate:
		return m.centered(m.updateView())
	case modeResult:
		return m.centered(m.resultView())
	case modeHelp:
		return m.centered(m.helpView())
	}
	return ""
}

// helpView is the full legend behind the `?` key: what each action does, in
// plain words, grouped so the send/share vs export/import distinction is clear.
func (m model) helpView() string {
	ctx := "off"
	if m.withContext {
		ctx = "on"
	}
	head := func(s string) string { return accentStyle.Bold(true).Render(s) }
	kcol := lipgloss.NewStyle().Foreground(signal).Width(6)
	row := func(k, d string) string { return "  " + kcol.Render(k) + dimStyle.Render(d) }

	// Blank strings in this list are deliberate spacers, so a row that does not
	// apply has to be left out rather than emptied - hence the appends.
	rows := []string{
		accentStyle.Bold(true).Render("What everything does"),
		"",
		head("Browse"),
		row("↑ ↓", "move through your sessions"),
		row("/", "search by title, project, id, or tool name"),
		row("row", "tool · id · messages · last active · project"),
		row("", "every coding agent found on this machine is listed"),
	}
	// The tool switch is only worth explaining when there is a second tool to
	// switch to; on a one-tool machine it describes a distinction that does not
	// exist here.
	multi := len(m.present) > 1
	if multi {
		rows = append(rows, row("t", "show one tool at a time, or all together (now: "+m.filterLabel()+")"))
	}
	rows = append(rows,
		"",
		head("Hand a session to a teammate"),
		row("↵", "send: stream it over an encrypted code, no file"),
		row("s", "share: write it to a .tgz file to send yourself"),
		row("r", "receive: type their code; it lands in this folder"),
		"",
		head("Move your own history between machines"),
		row("e", "export: pack every session into one backup file"),
	)
	// Same reasoning, and the same honesty the CLI applies: whole-machine backup
	// reads the Claude Code layout only, so say so wherever several tools are in
	// play rather than letting it be discovered on the new machine.
	if multi {
		rows = append(rows, row("", "backup covers Claude Code only; use s or ↵ for the rest"))
	}
	rows = append(rows,
		row("i", "import: restore a backup or shared file here"),
		"",
		head("Other"),
		row("c", "context: include project memory in a share ("+ctx+" now)"),
		row("u", "update: install a newer entangle in place"),
		row("q", "quit"),
		"",
		mutedStyle.Render("Secrets are scrubbed before anything leaves. Everything runs"),
		mutedStyle.Render("on this machine. No account, no server in the middle."),
		"",
		keyHint("any key", "back"),
	)
	return cardStyle.Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

func (m model) headerView() string {
	var b strings.Builder
	if m.bannerOK {
		b.WriteString(bannerStyle.Render(strings.TrimRight(m.banner, "\n")))
	} else {
		b.WriteString(bannerStyle.Render("◈ entangle"))
	}
	b.WriteString("\n")
	b.WriteString(taglineStyle.Render("hand a live coding session to anyone."))
	b.WriteString("\n")
	b.WriteString(accentStyle.Render("private by construction") + dimStyle.Render(" · everything runs on this machine"))
	b.WriteString("\n")
	b.WriteString(mutedStyle.Render(fmt.Sprintf("entangle v%s", m.version)))
	// Which tools the list covers, but only once there is more than one to cover:
	// on a machine with just Claude Code the distinction does not exist, and saying
	// it anyway would imply something is being hidden.
	if len(m.present) > 1 {
		b.WriteString(mutedStyle.Render(" · showing ") + accentStyle.Render(m.filterLabel()))
	}
	// Someone who only ever types `entangle` lives in here, so this is the only
	// place they would ever learn a new version exists. It says which command to
	// run because the right one differs by how they installed it.
	if m.newVersion != "" {
		how := "press u"
		if updater.ManagedByHomebrew() {
			how = "brew upgrade entangle"
		}
		b.WriteString(mutedStyle.Render(" · ") +
			warnStyle.Render("v"+m.newVersion+" available") +
			mutedStyle.Render(" · "+how))
	}
	b.WriteString("\n")
	return b.String()
}

func (m model) footerView() string {
	ctx := "off"
	if m.withContext {
		ctx = "on"
	}
	line1 := strings.Join([]string{
		keyHint("↵", "send"), keyHint("s", "share to file"), keyHint("r", "receive"),
		keyHint("e", "export"), keyHint("i", "import"), keyHint("u", "update"),
	}, dimStyle.Render("   "))
	hints := []string{keyHint("/", "search"), keyHint("c", "context: "+ctx)}
	// The tool switch is only offered when there is a second tool to switch to.
	if len(m.present) > 1 {
		hints = append(hints, keyHint("t", "tool: "+m.filterLabel()))
	}
	hints = append(hints,
		keyHint("?", "help"), keyHint("↑↓", "move"), keyHint("q", "quit"))
	line2 := strings.Join(hints, dimStyle.Render("   "))
	rule := mutedStyle.Render(strings.Repeat("─", max(1, min(m.width, 80))))
	return rule + "\n" + line1 + "\n" + line2
}

func keyHint(k, label string) string {
	return accentStyle.Render(k) + " " + dimStyle.Render(label)
}

func (m model) confirmView() string {
	p := m.prepped.preview
	verb := "Send"
	transport := "This streams over an end-to-end-encrypted connection."
	if m.forShare {
		verb = "Share"
		transport = "This writes a file you hand to your teammate."
	}
	ctxLine := "conversation only (memory not included)"
	if m.prepped.withContext {
		ctxLine = "project memory INCLUDED"
	}
	secrets := fmt.Sprintf("%d likely secret(s) masked", p.SecretsMasked)

	body := lipgloss.JoinVertical(lipgloss.Left,
		accentStyle.Bold(true).Render(verb+" this session?"),
		"",
		row("session", fmt.Sprintf("%s  (%s)", orDefault(p.Title, "(untitled)"), p.ShortID)),
		row("tool", toolName(m.prepped.provider)),
		row("project", orUnknown(p.ProjectPath)),
		row("content", fmt.Sprintf("%d message(s), %s", p.Messages, exporter.HumanSize(p.Bytes))),
		row("context", ctxLine),
		row("secrets", secrets),
		"",
		dimStyle.Render(transport),
		"",
		keyHint("↵", "confirm")+"    "+keyHint("esc", "cancel"),
	)
	return cardStyle.Render(body)
}

// noticeBlock renders the sticky advisory, if any, for the transfer screens.
func (m model) noticeBlock() string {
	if m.notice == "" {
		return ""
	}
	return warnStyle.Render(m.notice) + "\n\n"
}

func (m model) sendView() string {
	var b strings.Builder
	b.WriteString(accentStyle.Bold(true).Render("Sending a session") + "\n\n")
	b.WriteString(m.noticeBlock())
	if m.code == "" {
		b.WriteString(m.spinner.View() + " " + dimStyle.Render(m.status))
	} else {
		b.WriteString(dimStyle.Render("Read this code to your teammate:") + "\n\n")
		b.WriteString(codeStyle.Render(m.code) + "\n\n")
		if m.copied != "" {
			b.WriteString(okStyle.Render("  "+m.copied) + "\n")
		} else {
			b.WriteString(mutedStyle.Render("  press c to copy this plus the install line") + "\n")
		}
		b.WriteString("\n" + dimStyle.Render("They run this from inside their copy of the project:") + "\n")
		b.WriteString(paperText("  "+handoff.Command(m.code)) + "\n\n")
		if m.total > 0 {
			b.WriteString(m.progress.ViewAs(ratio(m.done, m.total)) + " " + dimStyle.Render(pct(m.done, m.total)) + "\n")
		} else {
			b.WriteString(m.spinner.View() + " " + dimStyle.Render("waiting for them to connect…") + "\n")
		}
	}
	if m.code != "" {
		b.WriteString("\n" + keyHint("c", "copy invite") + "    " + keyHint("esc", "cancel"))
	} else {
		b.WriteString("\n" + keyHint("esc", "cancel"))
	}
	return cardStyle.Render(b.String())
}

func (m model) transferView(title string) string {
	var b strings.Builder
	b.WriteString(accentStyle.Bold(true).Render(title) + "\n\n")
	b.WriteString(m.noticeBlock())
	b.WriteString(m.spinner.View() + " " + dimStyle.Render(orDefault(m.status, "working…")) + "\n\n")
	if m.total > 0 {
		b.WriteString(m.progress.ViewAs(ratio(m.done, m.total)) + " " + dimStyle.Render(pct(m.done, m.total)) + "\n")
	}
	b.WriteString("\n" + keyHint("esc", "cancel"))
	return cardStyle.Render(b.String())
}

func (m model) busyView() string {
	var b strings.Builder
	b.WriteString(m.spinner.View() + " " + taglineStyle.Render(orDefault(m.status, "working…")) + "\n")
	if m.total > 0 {
		b.WriteString("\n" + m.progress.ViewAs(ratio(m.done, m.total)) + " " + dimStyle.Render(pct(m.done, m.total)) + "\n")
	}
	return cardStyle.Render(b.String())
}

func (m model) inputView(title, prompt, hint string) string {
	body := lipgloss.JoinVertical(lipgloss.Left,
		accentStyle.Bold(true).Render(title),
		"",
		dimStyle.Render(prompt),
		m.input.View(),
		"",
		mutedStyle.Render(hint),
		"",
		keyHint("↵", "go")+"    "+keyHint("esc", "cancel"),
	)
	return cardStyle.Render(body)
}

func (m model) updateView() string {
	rows := []string{
		accentStyle.Bold(true).Render("An update is available"),
		"",
		row("installed", m.version),
		row("latest", m.updLatest),
		"",
		dimStyle.Render("Downloads the signed release and verifies its checksum."),
	}
	// Installing in place inside a Homebrew keg works, and then leaves brew still
	// convinced the old version is there - so a later `brew upgrade` can put an
	// older build back over the top. Better said here than discovered later as a
	// mysterious downgrade.
	if updater.ManagedByHomebrew() {
		rows = append(rows,
			"",
			warnStyle.Render("Homebrew installed this copy. Installing here confuses brew's"),
			warnStyle.Render("records - quit and run `brew upgrade entangle` instead."),
		)
	}
	rows = append(rows,
		"",
		keyHint("↵", "install")+"    "+keyHint("esc", "not now"),
	)
	return cardStyle.Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

func (m model) resultView() string {
	titleStyle := okStyle
	glyph := "✓ "
	if m.resErr {
		titleStyle, glyph = errStyle, "✗ "
	}
	// After an update the footer is a real choice, not an acknowledgement, so it
	// names both outcomes. Saying which version each key leaves you on is the
	// point: the number on screen is the one they just installed, and the number
	// they are still running is the old one.
	footer := mutedStyle.Render("press any key to go back")
	if m.restartTo != "" {
		footer = keyHint("↵", "restart into "+m.updLatest) + "    " +
			mutedStyle.Render("any other key stays on "+m.version)
	}
	body := lipgloss.JoinVertical(lipgloss.Left,
		titleStyle.Bold(true).Render(glyph+m.resTitle),
		"",
		taglineStyle.Render(m.resBody),
		"",
		footer,
	)
	style := cardStyle
	if m.resErr {
		style = cardStyle.BorderForeground(errRed)
	} else {
		style = cardStyle.BorderForeground(okGrn)
	}
	return style.Render(body)
}

// centered places a card in the middle of the screen with the wordmark above.
func (m model) centered(card string) string {
	head := bannerStyle.Render("◈ entangle")
	block := lipgloss.JoinVertical(lipgloss.Center, head, "", card)
	if m.width == 0 || m.height == 0 {
		return block
	}
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, block)
}

// ---- tiny formatting helpers ----------------------------------------------

func row(label, val string) string {
	return labelStyle.Render(label) + dimStyle.Render(": ") + taglineStyle.Render(val)
}

func paperText(s string) string { return taglineStyle.Render(s) }

func orUnknown(s string) string {
	if s == "" {
		return "(unknown project)"
	}
	return s
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func ratio(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	r := float64(done) / float64(total)
	if r > 1 {
		return 1
	}
	return r
}

func pct(done, total int64) string {
	if total <= 0 {
		return exporter.HumanSize(done)
	}
	return fmt.Sprintf("%.0f%% (%s / %s)", ratio(done, total)*100, exporter.HumanSize(done), exporter.HumanSize(total))
}

func humanAgo(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
