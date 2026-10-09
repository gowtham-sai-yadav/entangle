// Package cli wires the export/import/inspect subcommands together.
package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gowtham-sai-yadav/entangle/internal/agent"
	_ "github.com/gowtham-sai-yadav/entangle/internal/agent/claudecode" // registers the Claude Code provider
	_ "github.com/gowtham-sai-yadav/entangle/internal/agent/cline"      // registers the Cline provider
	_ "github.com/gowtham-sai-yadav/entangle/internal/agent/codex"      // registers the Codex CLI provider
	_ "github.com/gowtham-sai-yadav/entangle/internal/agent/opencode"   // registers the opencode provider
	"github.com/gowtham-sai-yadav/entangle/internal/agentshare"
	"github.com/gowtham-sai-yadav/entangle/internal/bundle"
	"github.com/gowtham-sai-yadav/entangle/internal/claudedir"
	"github.com/gowtham-sai-yadav/entangle/internal/exporter"
	"github.com/gowtham-sai-yadav/entangle/internal/handoff"
	"github.com/gowtham-sai-yadav/entangle/internal/importer"
	"github.com/gowtham-sai-yadav/entangle/internal/manifest"
	"github.com/gowtham-sai-yadav/entangle/internal/paths"
	"github.com/gowtham-sai-yadav/entangle/internal/transfer"
	"github.com/gowtham-sai-yadav/entangle/internal/tui"
	"github.com/gowtham-sai-yadav/entangle/internal/updater"
	"github.com/gowtham-sai-yadav/entangle/internal/webui"
	"golang.org/x/term"
)

// version is stamped by the linker at release time via
// -X ...internal/cli.version=<tag> (see .goreleaser.yaml). For `go install`
// builds it stays empty and we fall back to the module version in the build
// info, so the reported version tracks the git tag with no manual bumping.
var version string

// Version returns the running version, without a leading "v".
func Version() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return strings.TrimPrefix(v, "v")
		}
	}
	return "dev"
}

// Run dispatches one command, bracketed by the update check: the cached answer
// is shown before the work, and the cache is refreshed after it. Doing the fetch
// afterwards is what keeps the check off the critical path - see
// internal/updater/notice.go.
func Run(args []string) error {
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	showUpdateNotice(cmd)
	err := dispatch(args)
	// Only refresh when a person is watching. A script piping `sessions --json`
	// will never be shown the notice, so the request would be pure cost - and it
	// keeps test and CI runs off the network.
	//
	// `update` is excluded because it has just asked GitHub the same question
	// itself, and a second request in the same run is waste against a limit of 60
	// an hour for the whole machine. The cockpit is deliberately NOT excluded: it
	// only checks live when someone presses `u`, so a user who does nothing but
	// type `entangle` would otherwise never populate the cache the header reads,
	// and the badge they are the entire audience for would never appear.
	if isTerminal() && cmd != "update" && cmd != "upgrade" {
		updater.RefreshCheck("", Version())
	}
	// `entangle send -h` asks a flag set for its usage, which prints it and hands
	// back flag.ErrHelp. That is the command doing exactly what was asked, so it
	// must not reach the caller as a failure: `cmd -h` returning a non-zero status
	// breaks any script that checks one, and printing "error: flag: help
	// requested" under the usage text reads like the help itself went wrong.
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

// showUpdateNotice prints the one-line "newer version available" note, or
// nothing.
//
// It goes to stderr, and only with a terminal attached, for the same reason the
// rename notice does: `entangle sessions --json` is parsed by the editor
// extension and must stay byte for byte what it was.
//
// The interactive cockpit is excluded because it takes over the screen the
// moment it starts - the line would be wiped before it could be read - so it
// shows the same information in its header instead.
func showUpdateNotice(cmd string) {
	switch cmd {
	case "update", "upgrade":
		return // this command does its own, live check
	case "", "tui", "ui":
		return // the cockpit shows it in the header
	}
	if !isTerminal() {
		return
	}
	if latest := updater.AvailableUpdate(Version()); latest != "" {
		fmt.Fprintf(os.Stderr, "note: entangle %s is available (you have %s). Update with: %s\n\n",
			latest, Version(), updater.UpgradeCommand())
	}
}

func isTerminal() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

func dispatch(args []string) error {
	noticeIfLegacyName()
	if len(args) == 0 {
		// With a terminal attached, the friendliest default is the interactive
		// cockpit; piped or redirected, fall back to plain help text.
		if isTerminal() {
			return runTUI(nil)
		}
		printHelp()
		return nil
	}
	switch args[0] {
	case "export":
		return runExport(args[1:])
	case "import":
		return runImport(args[1:])
	case "inspect":
		return runInspect(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "sessions":
		return runSessions(args[1:])
	case "share":
		return runShare(args[1:])
	case "send":
		return runSend(args[1:])
	case "receive":
		return runReceive(args[1:])
	case "update", "upgrade":
		return runUpdate(args[1:])
	case "gui":
		return runGUI(args[1:])
	case "tui", "ui":
		return runTUI(args[1:])
	case "version", "-v", "--version":
		fmt.Println("entangle", Version())
		return nil
	case "help", "-h", "--help":
		printHelp()
		return nil
	default:
		return fmt.Errorf("unknown command %q (try: tui, sessions, send, receive, share, export, import)", args[0])
	}
}

func printHelp() {
	fmt.Print("entangle " + Version() + " - hand a live coding session to anyone\n\n" +
		"USAGE:\n" +
		"  entangle                 open the interactive cockpit (default with a terminal)\n" +
		"  entangle tui             open the interactive cockpit explicitly\n" +
		"  entangle export  [--out FILE] [--config-dir DIR]\n" +
		"  entangle import  <bundle> [--dry-run] [--map OLD=NEW]... [--project P]... [--target-os OS] [--overwrite] [--deep] [--yes]\n" +
		"  entangle inspect <bundle>\n" +
		"  entangle verify  [--config-dir DIR]\n" +
		"  entangle sessions [--tool all|claude-code|codex|opencode|kilo|cline] [--project P] [--config-dir DIR] [--json]\n" +
		"  entangle share   <session-id-prefix | --last> [--tool T] [--project P] [--out FILE] [--with-context] [--no-redact] [--yes]\n" +
		"  entangle send    <session-id-prefix | --last> [--tool T] [--project P] [--with-context] [--no-redact] [--yes]\n" +
		"  entangle receive <code> [--config-dir DIR] [--map OLD=NEW]... [--yes]\n" +
		"  entangle update  [--check] [--yes]\n" +
		"  entangle gui     [bundle] [--port N]\n\n" +
		"SESSIONS, SHARE and SEND cover every coding agent on this machine by default -\n" +
		"Claude Code, Codex, opencode, Kilo Code and Cline together. Paste an id from the list into SHARE\n" +
		"or SEND and the right tool is worked out for you; --tool narrows the list when\n" +
		"you want only one. IMPORT and RECEIVE read whichever tool a bundle came from.\n" +
		"EXPORT and VERIFY are Claude Code only for now: they move and check a whole\n" +
		"machine's history at once, which needs a per-tool restore layout that does not\n" +
		"exist yet.\n\n" +
		"SESSIONS lists your conversations so you can find one to hand off. SHARE packs a\n" +
		"single session into a file for a teammate (secrets scrubbed first). SEND does the\n" +
		"same but streams it over an end-to-end-encrypted connection: you read out a short\n" +
		"code and they RECEIVE it, no file to move. GUI opens a point-and-click wizard.\n\n" +
		"EXPORT runs on the OLD machine and writes a portable bundle. IMPORT runs on the\n" +
		"NEW machine and restores it, translating paths for this OS (Linux, macOS, or\n" +
		"Windows - drive letters and backslashes handled). VERIFY then checks the\n" +
		"migrated sessions are resume-ready. Your login is never copied.\n")
}

func runExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	out := fs.String("out", "", "output bundle path")
	cfg := fs.String("config-dir", "", "override Claude config dir")
	if err := fs.Parse(args); err != nil {
		return err
	}
	res, err := exporter.Run(exporter.Options{Out: *out, ConfigDir: *cfg, Version: Version()})
	if err != nil {
		return err
	}
	fmt.Printf("Exported %d Claude Code project(s), %d session(s) -> %s (%s)\n",
		res.Projects, res.Sessions, res.Path, exporter.HumanSize(res.Bytes))
	if res.UnknownPaths > 0 {
		fmt.Printf("Note: %d folder(s) had no recoverable path; they import under their original name.\n", res.UnknownPaths)
	}
	// Say what was left out. A user with three agents installed would otherwise
	// read "exported" as "all of it" and find out on the new machine.
	if others := otherToolsPresent(); len(others) > 0 {
		fmt.Printf("Not included: %s. Whole-machine export covers Claude Code only so far;\n"+
			"move those one at a time with: entangle share <id>\n", strings.Join(others, " and "))
	}
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func runImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "show the plan without writing anything")
	overwrite := fs.Bool("overwrite", false, "overwrite existing files (backs up first)")
	deep := fs.Bool("deep", false, "rewrite old paths everywhere in transcripts, not just cwd")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	home := fs.String("target-home", "", "override the target home directory")
	tos := fs.String("target-os", "", "render paths for this OS: linux|darwin|windows (default: this machine)")
	cfg := fs.String("config-dir", "", "override the target Claude config dir")
	var maps multiFlag
	fs.Var(&maps, "map", "remap OLD=NEW path prefix (repeatable)")
	var projects multiFlag
	fs.Var(&projects, "project", "import only this project, by path or folder (repeatable; default: all)")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return fmt.Errorf("usage: entangle import <bundle> [flags]")
	}
	parsed, err := parseMaps(maps)
	if err != nil {
		return err
	}
	return applyBundle(pos[0], importer.Options{
		Bundle:     pos[0],
		TargetHome: *home,
		TargetOS:   *tos,
		ConfigDir:  *cfg,
		DryRun:     *dry,
		Overwrite:  *overwrite,
		Deep:       *deep,
		AssumeYes:  *yes,
		Maps:       parsed,
		Projects:   projects,
	})
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	cfg := fs.String("config-dir", "", "override the Claude config dir")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// verify inspects what is already installed, so a filename here means the user
	// expected it to check a bundle. Silently ignoring it reported healthy-looking
	// numbers about something they were not asking about, which reads as a pass.
	if fs.NArg() > 0 {
		return fmt.Errorf("verify checks the sessions already on this machine and takes no file argument.\n"+
			"To look inside a bundle before importing it, run:\n  entangle inspect %s", fs.Arg(0))
	}
	tp, err := claudedir.Locate(*cfg)
	if err != nil {
		return err
	}
	results := importer.VerifyDir(tp)
	if len(results) == 0 {
		fmt.Println("No projects found under", tp.ProjectsDir)
		return nil
	}
	ok := 0
	for _, v := range results {
		status := "ok"
		if !v.OK {
			status = "FAIL: " + v.Detail
		} else {
			ok++
		}
		fmt.Printf("  [%s] %s  (%d session(s))\n", status, v.Folder, v.Sessions)
	}
	fmt.Printf("\n%d/%d Claude Code project(s) resume-ready.\n", ok, len(results))
	if others := otherToolsPresent(); len(others) > 0 {
		fmt.Printf("Not checked: %s. Verify reads the Claude Code transcript format only.\n", strings.Join(others, " and "))
	}
	return nil
}

// otherToolsPresent names the installed tools that the whole-machine commands do
// not cover, for a user standing in front of a report that looks complete.
func otherToolsPresent() []string {
	var out []string
	for _, b := range agent.Installed("") {
		if b.Provider.ID() != agent.ClaudeCode {
			out = append(out, b.Provider.DisplayName())
		}
	}
	return out
}

// ProviderClaudeCode names the coding tool a session belongs to. Codex and
// opencode are emitted too; the identifiers are stable because they travel in
// output other programs parse.
const ProviderClaudeCode = "claude-code"

// SessionJSON is the shape `sessions --json` emits, one element per session.
//
// PUBLIC CONTRACT. The editor extension and anything else scripting this command
// parses it, and a released extension keeps running against newer CLIs, so:
// fields may be ADDED (unknown keys are ignored by consumers), but no existing
// key may be renamed, removed, or change meaning. Adding a field is safe;
// changing one is a breaking release.
type SessionJSON struct {
	// Provider is which coding tool recorded the session. Emitted from the
	// start so consumers can branch on it before more tools are supported,
	// rather than needing an update at that point.
	Provider  string `json:"provider"`
	ID        string `json:"id"`
	ShortID   string `json:"shortId"`
	Project   string `json:"project"`
	Folder    string `json:"folder"`
	Messages  int    `json:"messages"`
	Modified  string `json:"modified"`
	SizeBytes int64  `json:"sizeBytes"`
	Title     string `json:"title"`
}

func runSessions(args []string) error {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	cfg := fs.String("config-dir", "", "override the config dir of the selected tool")
	project := fs.String("project", "", "only sessions whose project path or folder contains this")
	asJSON := fs.Bool("json", false, "output the session list as JSON (for tooling, e.g. the editor extension)")
	tool := fs.String("tool", "", toolFlagUsage("list"))
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Listing goes through the provider registry, so this command never learns how
	// any particular tool stores its sessions.
	sessions, multi, err := listFor(toolSelection(*tool, *cfg), *cfg)
	if err != nil {
		return err
	}
	agent.SortSessions(sessions)
	sessions = filterSessions(sessions, *project)

	if *asJSON {
		list := make([]SessionJSON, 0, len(sessions))
		for _, s := range sessions {
			list = append(list, SessionJSON{
				Provider: string(s.Provider),
				ID:       s.ID, ShortID: s.ShortID, Project: s.ProjectPath, Folder: s.GroupKey,
				Messages: s.Messages, Modified: s.ModTime.Format(time.RFC3339), SizeBytes: s.Size, Title: s.Title,
			})
		}
		b, err := json.MarshalIndent(list, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}

	if len(sessions) == 0 {
		explainNoSessions(toolSelection(*tool, *cfg), *cfg)
		return nil
	}
	// The TOOL column appears only when more than one tool is in play, so someone
	// using a single tool sees exactly the table they saw before.
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	if multi {
		fmt.Fprintln(tw, "TOOL\tID\tLAST ACTIVE\tMSGS\tPROJECT\tTITLE")
	} else {
		fmt.Fprintln(tw, "ID\tLAST ACTIVE\tMSGS\tPROJECT\tTITLE")
	}
	for _, s := range sessions {
		proj := elideLeft(s.ProjectPath, projectColWidth)
		if proj == "" {
			proj = "(unknown)"
		}
		if multi {
			fmt.Fprintf(tw, "%s\t", s.Provider)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n",
			s.ShortID, s.ModTime.Format("2006-01-02 15:04"), s.Messages, proj, s.Title)
	}
	tw.Flush()
	fmt.Printf("\n%d session(s). Share one with: entangle share <ID>\n", len(sessions))
	return nil
}

// explainNoSessions says what was searched instead of leaving an empty list to
// speak for itself.
//
// "No sessions found." cannot be told apart from three different situations: no
// coding tool is set up here at all, one is set up somewhere this did not look, or
// the tools really are empty. Someone who installed entangle a minute ago hits the
// first two far more often than the third, and four words send them away believing
// it does not work. Naming the directories that were read turns it into a fact
// they can act on.
//
// This is the same promise `export` already keeps by printing the tools it skipped.
//
// Mirrors Scan's override rule: --config-dir has only ever named the Claude Code
// directory, so it is not handed to another provider.
func explainNoSessions(sel, configDir string) {
	if sel != toolAll {
		// A tool named with --tool that is not installed never reaches here;
		// agent.Resolve fails earlier with its own message. So it is present and
		// simply has nothing recorded yet.
		fmt.Println("No sessions found.")
		if b, err := agent.Resolve(agent.ID(sel), configDir); err == nil {
			fmt.Printf("%s is set up at %s and has no sessions recorded yet.\n",
				b.Provider.DisplayName(), b.Roots.ConfigDir)
		}
		return
	}

	var missing []string
	found := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	any := false
	for _, p := range agent.All() {
		r, ok, err := p.Locate("")
		if err != nil || !ok {
			missing = append(missing, p.DisplayName())
			continue
		}
		fmt.Fprintf(found, "  %s\t%s\n", p.DisplayName(), r.ConfigDir)
		any = true
	}

	if !any {
		fmt.Println("No sessions found: none of these coding tools are set up on this machine.")
		fmt.Println()
		for _, name := range missing {
			fmt.Println("  " + name)
		}
		fmt.Println()
		fmt.Println("Using one of them from somewhere unusual? Name the directory:")
		fmt.Println("  entangle sessions --tool claude-code --config-dir <dir>")
		return
	}

	fmt.Println("No sessions found. Looked in:")
	fmt.Println()
	found.Flush()
	if len(missing) > 0 {
		fmt.Println()
		fmt.Println("Not set up on this machine: " + strings.Join(missing, ", "))
	}
}

// projectColWidth caps the project column. A single deeply nested path (an
// editor's cache directory, say) otherwise pads the whole table out past a
// terminal's width via tabwriter, pushing the titles off screen.
const projectColWidth = 46

// elideLeft shortens a path to at most max characters by dropping the front,
// because the end of a path - the project's own name - is the part that
// identifies it. Short paths are returned untouched.
func elideLeft(s string, max int) string {
	r := []rune(s)
	if len(r) <= max || max < 2 {
		return s
	}
	return "…" + string(r[len(r)-(max-1):])
}

// toolAll is the --tool value meaning "every coding agent installed here", and
// the default for every command that resolves a session.
//
// entangle is not a Claude Code tool with other agents bolted on. Defaulting to
// one vendor means a Codex user runs `entangle sessions`, sees an empty list, and
// concludes the tool does not support them - the opposite of what it does.
const toolAll = "all"

func toolFlagUsage(verb string) string {
	return "which coding tool to " + verb + ": " + strings.Join(agent.IDs(), ", ") + ", or all (default all)"
}

// toolSelection turns an unset --tool into the right default for the flags the
// user did pass.
//
// --config-dir has always named the Claude Code directory and nothing else, so a
// command given one but no --tool means Claude Code. Without this, every existing
// script that passes --config-dir would start failing on the "all" path, which
// cannot honour a single tool's override. Naming both --tool all and --config-dir
// is still an error, because then the user has asked for two contradictory things
// rather than left one unsaid.
func toolSelection(tool, configDir string) string {
	if tool != "" {
		return tool
	}
	if configDir != "" {
		return string(agent.ClaudeCode)
	}
	return toolAll
}

// resolveTool decides which tool owns the session the user named.
//
// An explicit --tool is taken at its word. Otherwise the id is looked up across
// every installed tool, so `entangle share <id>` works with an id copied out of a
// listing without the user having to know, or type, which agent produced it.
func resolveTool(tool, configDir, prefix, project string, last bool) (string, error) {
	if tool != toolAll {
		return tool, nil
	}
	sessions, _, err := listFor(toolAll, configDir)
	if err != nil {
		return "", err
	}
	agent.SortSessions(sessions)
	sessions = filterSessions(sessions, project)
	s, err := pickSession(sessions, prefix, last, "")
	if err != nil {
		return "", err
	}
	return string(s.Provider), nil
}

// listFor gathers sessions for a --tool selection. multi reports whether more than
// one tool could contribute, which decides whether the table shows a TOOL column.
func listFor(tool, configDir string) (sessions []agent.Session, multi bool, err error) {
	if tool == "all" {
		// --config-dir overrides one tool's location, so pairing it with "all"
		// has no coherent meaning. Refuse rather than silently apply it to one.
		if configDir != "" {
			return nil, false, fmt.Errorf("--config-dir applies to a single tool; use it with --tool <name>")
		}
		bounds := agent.Installed("")
		if len(bounds) == 0 {
			return nil, false, nil
		}
		for _, b := range bounds {
			s, lerr := b.Provider.ListSessions(b.Roots)
			if lerr != nil {
				// One unreadable tool must not hide the others.
				fmt.Fprintf(os.Stderr, "warning: could not list %s sessions: %v\n", b.Provider.DisplayName(), lerr)
				continue
			}
			sessions = append(sessions, s...)
		}
		// Each provider numbered its own sessions in isolation, so two tools can
		// hand back the same handle. Re-derive them over the joined list: a handle
		// printed here is one the user pastes straight back into `share`, so it has
		// to be unique across everything shown, not just within one tool.
		agent.AssignShortIDs(sessions, agent.ShortIDMin)
		return sessions, len(bounds) > 1, nil
	}

	bound, err := agent.Resolve(agent.ID(tool), configDir)
	if err != nil {
		return nil, false, err
	}
	sessions, err = bound.Provider.ListSessions(bound.Roots)
	return sessions, false, err
}

// filterSessions narrows a listing by a substring of the project path or the
// provider's on-disk bucket name.
func filterSessions(in []agent.Session, needle string) []agent.Session {
	if needle == "" {
		return in
	}
	needle = strings.ToLower(needle)
	var out []agent.Session
	for _, s := range in {
		if strings.Contains(strings.ToLower(s.ProjectPath), needle) ||
			strings.Contains(strings.ToLower(s.GroupKey), needle) {
			out = append(out, s)
		}
	}
	return out
}

func runShare(args []string) error {
	fs := flag.NewFlagSet("share", flag.ContinueOnError)
	out := fs.String("out", "", "output file path")
	cfg := fs.String("config-dir", "", "override the selected tool's config dir")
	last := fs.Bool("last", false, "share your most recent session")
	project := fs.String("project", "", "disambiguate by project when the same id exists in more than one")
	withContext := fs.Bool("with-context", false, "also include the project's memory/context files (Claude Code only)")
	noRedact := fs.Bool("no-redact", false, "do NOT scrub secrets before packing (not recommended)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	tool := fs.String("tool", "", toolFlagUsage("share from"))
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	prefix := ""
	if len(pos) > 0 {
		prefix = pos[0]
	}
	if prefix == "" && !*last {
		return fmt.Errorf("usage: entangle share <session-id-prefix | --last>")
	}

	owner, err := resolveTool(toolSelection(*tool, *cfg), *cfg, prefix, *project, *last)
	if err != nil {
		return err
	}

	// Claude Code keeps its own long-standing path: its bundle layout is what every
	// released binary reads, so it is deliberately left alone.
	if agent.ID(owner) == agent.ClaudeCode {
		return exporter.RunShare(exporter.ShareOptions{
			ConfigDir:     *cfg,
			Version:       Version(),
			Out:           *out,
			SessionPrefix: prefix,
			Project:       *project,
			Last:          *last,
			WithContext:   *withContext,
			Redact:        !*noRedact,
			AssumeYes:     *yes,
			Confirm:       confirmShare,
		})
	}

	if *withContext {
		fmt.Println("Note: --with-context only applies to Claude Code; ignoring it.")
	}
	b, err := packForeign(owner, *cfg, prefix, *project, *last, !*noRedact)
	if err != nil {
		return err
	}
	if !*yes && !confirmAgentShare(b, false) {
		fmt.Println("Aborted - nothing was written.")
		return nil
	}
	dest := *out
	if dest == "" {
		dest = b.Name
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	if err := b.WriteBundle(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Printf("\nWrote %s\n", dest)
	fmt.Printf("Your teammate imports it with: entangle import %s\n", dest)
	fmt.Printf("They need %s installed, and should run it from the project folder they want the session attached to.\n",
		displayNameOf(owner))
	return nil
}

// packForeign resolves a session belonging to a non-Claude tool and packs it.
func packForeign(tool, configDir, prefix, project string, last, redactOn bool) (*agentshare.Bundle, error) {
	bound, err := agent.Resolve(agent.ID(tool), configDir)
	if err != nil {
		return nil, err
	}
	sessions, err := bound.Provider.ListSessions(bound.Roots)
	if err != nil {
		return nil, err
	}
	agent.SortSessions(sessions)
	sessions = filterSessions(sessions, project)
	s, err := pickSession(sessions, prefix, last, bound.Provider.DisplayName())
	if err != nil {
		return nil, err
	}
	return agentshare.Pack(bound.Provider, bound.Roots, s, agentshare.Options{
		ToolVersion: Version(),
		Redact:      redactOn,
	})
}

// pickSession resolves an id prefix, or the most recent session with --last,
// erroring clearly on nothing-matched and on an ambiguous prefix.
//
// toolName scopes the messages to one tool; empty means the search ran across all
// of them, and the wording drops the vendor so it does not claim a scope it
// did not have.
func pickSession(sessions []agent.Session, prefix string, last bool, toolName string) (agent.Session, error) {
	scope, seeAlso := "", "entangle sessions"
	if toolName != "" {
		scope, seeAlso = toolName+" ", "entangle sessions --tool "+toolName
	}
	if len(sessions) == 0 {
		return agent.Session{}, fmt.Errorf("no %ssessions found on this machine", scope)
	}
	if last {
		return sessions[0], nil // already newest-first
	}
	var hits []agent.Session
	for _, s := range sessions {
		if strings.HasPrefix(s.ID, prefix) {
			hits = append(hits, s)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return agent.Session{}, fmt.Errorf("no %ssession matches %q (see: %s)", scope, prefix, seeAlso)
	}

	// A tool can hold one session id under more than one project - Claude Code
	// does it when the same conversation is resumed from a different folder. The
	// ids are then identical, so "use a longer id" is advice that cannot be
	// followed. Name the projects and point at the flag that can separate them.
	sameID := true
	for _, h := range hits[1:] {
		if h.ID != hits[0].ID {
			sameID = false
			break
		}
	}
	if sameID {
		var where []string
		for _, h := range hits {
			p := h.ProjectPath
			if p == "" {
				p = h.GroupKey
			}
			where = append(where, p)
		}
		return agent.Session{}, fmt.Errorf("id %s exists in %d projects (%s); pick one with --project <substring>",
			hits[0].ShortID, len(hits), strings.Join(where, ", "))
	}

	tools := map[agent.ID]bool{}
	for _, h := range hits {
		tools[h.Provider] = true
	}
	var ids []string
	for _, h := range hits {
		label := h.ShortID
		if len(tools) > 1 {
			label += " (" + string(h.Provider) + ")"
		}
		ids = append(ids, label)
	}
	hint := "use a longer id"
	if len(tools) > 1 {
		hint = "use a longer id, or name the tool with --tool"
	}
	return agent.Session{}, fmt.Errorf("%q matches %d sessions (%s); %s", prefix, len(hits), strings.Join(ids, ", "), hint)
}

func displayNameOf(tool string) string {
	if p, ok := agent.Get(agent.ID(tool)); ok {
		return p.DisplayName()
	}
	return tool
}

// confirmAgentShare shows what is about to leave the machine for a non-Claude
// session. It mirrors confirmShare so the decision looks the same for every tool.
func confirmAgentShare(b *agentshare.Bundle, overNetwork bool) bool {
	where := "This writes a file you hand to your teammate."
	if overNetwork {
		where = "This streams over an end-to-end-encrypted connection."
	}
	fmt.Printf("About to share ONE %s session. This leaves your machine, so read it:\n", displayNameOf(string(b.Provider)))
	fmt.Printf("  session : %s  (%s)\n", orUnknown(b.Preview.Title, "(untitled)"), b.Preview.ShortID)
	fmt.Printf("  project : %s\n", orUnknown(b.Preview.ProjectPath, "(unknown)"))
	fmt.Printf("  content : %d message(s), %s\n", b.Preview.Messages, exporter.HumanSize(b.Preview.Bytes))
	fmt.Printf("  secrets : %d likely secret(s) masked (best effort, not a guarantee)\n", b.Preview.SecretsMasked)
	fmt.Println("  " + where)
	q := "Write this file?"
	if overNetwork {
		q = "Send it?"
	}
	return confirm(q)
}

func orUnknown(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// confirmShare is passed into the exporter so the summary and prompt live in
// one place (the CLI) while the packing logic stays in the exporter.
func confirmShare(preview exporter.SharePreview) bool {
	fmt.Println("About to share ONE session. This leaves your machine, so read it:")
	fmt.Printf("  session : %s  (%s)\n", preview.Title, preview.ShortID)
	fmt.Printf("  project : %s\n", preview.ProjectPath)
	fmt.Printf("  content : %d message(s), %s\n", preview.Messages, exporter.HumanSize(preview.Bytes))
	if preview.WithContext {
		fmt.Println("  context : project memory INCLUDED (--with-context)")
	} else {
		fmt.Println("  context : conversation only (memory not included)")
	}
	if preview.Redact {
		fmt.Printf("  secrets : %d likely secret(s) masked (best effort, not a guarantee)\n", preview.SecretsMasked)
	} else {
		fmt.Println("  secrets : NOT scrubbed (--no-redact) - the raw transcript will be shared")
	}
	return confirm("Write this file?")
}

// runSend builds a single-session bundle and streams it to a teammate over an
// end-to-end-encrypted wormhole, identified by a short spoken code. No file
// changes hands.
func runSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	cfg := fs.String("config-dir", "", "override the selected tool's config dir")
	last := fs.Bool("last", false, "send your most recent session")
	project := fs.String("project", "", "disambiguate by project when the same id exists in more than one")
	withContext := fs.Bool("with-context", false, "also include the project's memory/context files")
	noRedact := fs.Bool("no-redact", false, "do NOT scrub secrets before sending (not recommended)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	rendezvous := fs.String("rendezvous", envOr("ENTANGLE_RENDEZVOUS", ""), "rendezvous server URL (default: public magic-wormhole)")
	relay := fs.String("relay", envOr("ENTANGLE_RELAY", ""), "transit relay host:port (default: public magic-wormhole)")
	words := fs.Int("code-words", 2, "number of words in the transfer code")
	timeout := fs.Duration("timeout", 15*time.Minute, "give up if the peer does not connect within this time")
	tool := fs.String("tool", "", toolFlagUsage("send from"))
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	prefix := ""
	if len(pos) > 0 {
		prefix = pos[0]
	}
	if prefix == "" && !*last {
		return fmt.Errorf("usage: entangle send <session-id-prefix | --last>")
	}

	owner, err := resolveTool(toolSelection(*tool, *cfg), *cfg, prefix, *project, *last)
	if err != nil {
		return err
	}

	// The transfer itself does not care what is inside the bundle, so the only
	// difference between tools is how the bundle gets built.
	var (
		buf      bytes.Buffer
		sendName string
	)
	if agent.ID(owner) == agent.ClaudeCode {
		b, perr := exporter.PrepareShare(exporter.ShareOptions{
			ConfigDir:     *cfg,
			Version:       Version(),
			SessionPrefix: prefix,
			Project:       *project,
			Last:          *last,
			WithContext:   *withContext,
			Redact:        !*noRedact,
		})
		if perr != nil {
			return perr
		}
		if !*yes && !confirmSend(b.Preview) {
			fmt.Println("Aborted - nothing was sent.")
			return nil
		}
		if err := b.WriteBundle(&buf); err != nil {
			return err
		}
		sendName = b.Name
	} else {
		if *withContext {
			fmt.Println("Note: --with-context only applies to Claude Code; ignoring it.")
		}
		b, perr := packForeign(owner, *cfg, prefix, *project, *last, !*noRedact)
		if perr != nil {
			return perr
		}
		if !*yes && !confirmAgentShare(b, true) {
			fmt.Println("Aborted - nothing was sent.")
			return nil
		}
		if err := b.WriteBundle(&buf); err != nil {
			return err
		}
		sendName = b.Name
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	tcfg := transfer.Config{RendezvousURL: *rendezvous, TransitRelay: *relay, CodeWords: *words}
	fmt.Println("Preparing a secure transfer...")
	err = transfer.Send(ctx, tcfg, sendName, buf.Bytes(),
		printSendCode,
		progressPrinter("Sending"),
	)
	if err != nil {
		return fmt.Errorf("send failed: %w", err)
	}
	fmt.Println("\nDone. The session is on your teammate's machine.")
	return nil
}

// printSendCode is what a sender reads off the screen the moment a code exists.
// Split out of runSend so it can be tested without opening a real wormhole.
//
// It says the same thing twice on purpose, because two different people are being
// served. Someone whose teammate already has entangle wants one short command.
// Someone whose teammate does not needs a block they can paste as-is, or they end
// up explaining what entangle is over chat while this transfer waits to time out.
func printSendCode(code string) {
	fmt.Printf("\nGive your teammate this code:\n\n    %s\n\n", code)
	fmt.Println("They run this from inside their copy of the project:")
	fmt.Printf("    %s\n\n", handoff.Command(code))
	fmt.Printf("If they do not have entangle yet, paste them this instead:\n\n")
	fmt.Println(handoff.Invite(code))
	fmt.Println("\nWaiting for them to connect... (press Ctrl-C to cancel)")
}

// runReceive pulls a session bundle over a wormhole using the code, then hands
// it to the normal importer (which attaches it to the current directory).
func runReceive(args []string) error {
	fs := flag.NewFlagSet("receive", flag.ContinueOnError)
	cfg := fs.String("config-dir", "", "override the target Claude config dir")
	overwrite := fs.Bool("overwrite", false, "overwrite existing files (backs up first)")
	deep := fs.Bool("deep", false, "rewrite old paths everywhere in transcripts, not just cwd")
	yes := fs.Bool("yes", false, "skip the import confirmation prompt")
	home := fs.String("target-home", "", "override the target home directory")
	tos := fs.String("target-os", "", "render paths for this OS: linux|darwin|windows")
	rendezvous := fs.String("rendezvous", envOr("ENTANGLE_RENDEZVOUS", ""), "rendezvous server URL (default: public magic-wormhole)")
	relay := fs.String("relay", envOr("ENTANGLE_RELAY", ""), "transit relay host:port (default: public magic-wormhole)")
	timeout := fs.Duration("timeout", 15*time.Minute, "give up if the transfer does not start within this time")
	var maps multiFlag
	fs.Var(&maps, "map", "remap OLD=NEW path prefix (repeatable)")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return fmt.Errorf("usage: entangle receive <code>")
	}
	code := pos[0]
	parsedMaps, err := parseMaps(maps)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	tcfg := transfer.Config{RendezvousURL: *rendezvous, TransitRelay: *relay}
	fmt.Println("Connecting...")
	in, err := transfer.Receive(ctx, tcfg, code)
	if err != nil {
		return fmt.Errorf("receive failed: %w", err)
	}

	// Stream to a temp bundle, capped so a peer that lies about the size cannot
	// fill the disk. The importer then treats it exactly like a shared file.
	tmp, err := os.CreateTemp("", "entangle-recv-*.tgz")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	const maxBytes = 2 << 30 // 2 GiB hard ceiling
	if _, err := copyCapped(tmp, in, in.Bytes, maxBytes, progressPrinter("Receiving")); err != nil {
		tmp.Close()
		return fmt.Errorf("receive failed: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	fmt.Println()

	return applyBundle(tmpPath, importer.Options{
		Bundle:     tmpPath,
		TargetHome: *home,
		TargetOS:   *tos,
		ConfigDir:  *cfg,
		Overwrite:  *overwrite,
		Deep:       *deep,
		AssumeYes:  *yes,
		Maps:       parsedMaps,
	})
}

// applyBundle routes a bundle to whichever tool owns it. The sender's tool is
// recorded in the manifest, so a receiver never has to be told which flag to pass:
// `receive` and `import` work the same regardless of where the session came from.
func applyBundle(bundlePath string, opts importer.Options) error {
	id, foreign, err := agentshare.PeekAgent(bundlePath)
	if err != nil {
		return err
	}
	if !foreign {
		return importer.Run(opts)
	}

	// A foreign session attaches to the directory the receiver is standing in,
	// which is the same rule Claude shared sessions follow.
	targetDir, err := os.Getwd()
	if err != nil {
		return err
	}
	name := displayNameOf(string(id))
	// Phrased to avoid "a"/"an", which no tool name can satisfy in general: it is
	// "a Claude Code session" but "an OpenAI Codex CLI session".
	fmt.Printf("This bundle holds one %s session. Attaching it to the current directory:\n  %s\n", name, targetDir)
	if !opts.AssumeYes && !confirm("Import it here?") {
		fmt.Println("Aborted - nothing was written.")
		return nil
	}
	res, err := agentshare.Unpack(bundlePath, targetDir, opts.ConfigDir)
	if err != nil {
		return err
	}
	fmt.Printf("\nDone. Imported one %s session.\n", res.DisplayName)
	if res.ResumeHint != "" {
		fmt.Printf("Carry on with:\n    %s\n", res.ResumeHint)
	}
	fmt.Println("\nIMPORTANT: your login was NOT transferred - credentials never are.")
	return nil
}

// confirmSend shows what is about to leave the machine over the network and
// asks to proceed. It mirrors confirmShare but names the transport.
func confirmSend(preview exporter.SharePreview) bool {
	fmt.Println("About to send ONE session over an end-to-end-encrypted connection. Read it first:")
	fmt.Printf("  session : %s  (%s)\n", preview.Title, preview.ShortID)
	fmt.Printf("  project : %s\n", preview.ProjectPath)
	fmt.Printf("  content : %d message(s), %s\n", preview.Messages, exporter.HumanSize(preview.Bytes))
	if preview.WithContext {
		fmt.Println("  context : project memory INCLUDED (--with-context)")
	} else {
		fmt.Println("  context : conversation only (memory not included)")
	}
	if preview.Redact {
		fmt.Printf("  secrets : %d likely secret(s) masked (best effort, not a guarantee)\n", preview.SecretsMasked)
	} else {
		fmt.Println("  secrets : NOT scrubbed (--no-redact) - the raw transcript will be sent")
	}
	return confirm("Send it?")
}

// Switching transfer servers is deliberately not announced. Nothing is required of
// either side - receiving tries both mailboxes at once - and a warning about
// infrastructure in the middle of a handoff reads as a problem when there is none.
// A failure of both servers still reports itself, which is when it matters.

// progressPrinter returns a progress callback that rewrites a single status
// line on stderr.
func progressPrinter(label string) transfer.Progress {
	return func(done, total int64) {
		if total <= 0 {
			fmt.Fprintf(os.Stderr, "\r%s %s   ", label, exporter.HumanSize(done))
			return
		}
		pct := float64(done) / float64(total) * 100
		fmt.Fprintf(os.Stderr, "\r%s %3.0f%% (%s / %s)   ", label, pct, exporter.HumanSize(done), exporter.HumanSize(total))
	}
}

// copyCapped copies src to dst, reporting progress and refusing to write more
// than limit bytes (a peer controls the offered size, so it cannot be trusted).
func copyCapped(dst io.Writer, src io.Reader, total, limit int64, prog transfer.Progress) (int64, error) {
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
			if prog != nil {
				prog(done, total)
			}
		}
		if rerr == io.EOF {
			return done, nil
		}
		if rerr != nil {
			return done, rerr
		}
	}
}

// envOr reads an ENTANGLE_* setting, falling back to the CLAUDE_TELEPORT_*
// spelling this project used before it was renamed.
//
// People put these in shell profiles and CI config, and a rename is no reason to
// silently stop honouring what they already wrote. The old names keep working
// until a release well after the rename has settled.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if legacy, ok := strings.CutPrefix(key, "ENTANGLE_"); ok {
		if v := os.Getenv("CLAUDE_TELEPORT_" + legacy); v != "" {
			return v
		}
	}
	return def
}

// runUpdate checks GitHub for a newer release and, unless --check, downloads it,
// verifies its checksum, and replaces this binary in place.
func runUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	check := fs.Bool("check", false, "only report whether a newer version exists")
	yes := fs.Bool("yes", false, "update without asking")
	repo := fs.String("repo", updater.DefaultRepo, "owner/repo to update from")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fmt.Println("Checking for updates...")
	latest, err := updater.LatestVersion(ctx, *repo)
	if err != nil {
		return fmt.Errorf("could not check the latest version: %w", err)
	}
	latestClean := strings.TrimPrefix(latest, "v")
	fmt.Printf("  installed : %s\n  latest    : %s\n", Version(), latestClean)

	if !updater.Newer(latest, Version()) {
		fmt.Println("You are already on the latest version.")
		return nil
	}
	if *check {
		fmt.Printf("A newer version is available (%s). Install it with: %s\n", latestClean, updater.UpgradeCommand())
		return nil
	}
	// Replacing the executable inside a Homebrew keg works, and then leaves brew
	// convinced the old version is still installed - so the next `brew upgrade`
	// can put an older build back over the top. Say so before doing it, rather
	// than letting the user discover it as a mysterious downgrade.
	if updater.ManagedByHomebrew() {
		fmt.Println("\nThis copy was installed by Homebrew. Updating in place here leaves brew's")
		fmt.Println("record out of step with the file on disk, and a later `brew upgrade` can put")
		fmt.Printf("an older build back. Use this instead:\n\n  brew upgrade entangle\n\n")
		if !*yes && !confirm("Replace this file anyway?") {
			fmt.Println("Aborted.")
			return nil
		}
	} else if !*yes && !confirm(fmt.Sprintf("Update to %s now?", latestClean)) {
		fmt.Println("Aborted.")
		return nil
	}
	if err := updater.Apply(ctx, *repo, latest, progressPrinter("Downloading")); err != nil {
		return fmt.Errorf("update failed: %w", err)
	}
	fmt.Printf("\nUpdated to %s.\n", latestClean)
	return nil
}

// confirm asks a yes/no question on the terminal (default no).
func confirm(q string) bool {
	fmt.Printf("%s [y/N]: ", q)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}

func runGUI(args []string) error {
	fs := flag.NewFlagSet("gui", flag.ContinueOnError)
	port := fs.Int("port", 0, "port to listen on (0 = pick a free one)")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	bundlePath := ""
	if len(pos) > 0 {
		bundlePath = pos[0]
	}
	return webui.Serve(*port, bundlePath, Version())
}

// runTUI launches the full-screen interactive cockpit, wiring in the same
// transfer configuration the send/receive commands accept so a self-hosted
// rendezvous/relay works here too.
func runTUI(args []string) error {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	cfg := fs.String("config-dir", "", "override the Claude config dir")
	rendezvous := fs.String("rendezvous", envOr("ENTANGLE_RENDEZVOUS", ""), "rendezvous server URL (default: public magic-wormhole)")
	relay := fs.String("relay", envOr("ENTANGLE_RELAY", ""), "transit relay host:port (default: public magic-wormhole)")
	words := fs.Int("code-words", 2, "number of words in a generated transfer code")
	if err := fs.Parse(args); err != nil {
		return err
	}
	tcfg := transfer.Config{RendezvousURL: *rendezvous, TransitRelay: *relay, CodeWords: *words}
	// The cockpit takes over the screen, so it is handed the update note to show
	// in its own header rather than having it printed underneath the alt-screen.
	return tui.Run(*cfg, tcfg, Version(), updater.AvailableUpdate(Version()))
}

// parseInterleaved lets flags and positional arguments appear in any order.
// Go's flag package stops at the first positional, so we consume one
// positional at a time and re-parse the remainder.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		positionals = append(positionals, rest[0])
		args = rest[1:]
	}
	return positionals, nil
}

func parseMaps(in []string) ([]paths.Mapping, error) {
	var out []paths.Mapping
	for _, s := range in {
		i := strings.IndexByte(s, '=')
		if i < 0 {
			return nil, fmt.Errorf("bad --map %q (want OLD=NEW)", s)
		}
		out = append(out, paths.Mapping{Old: s[:i], New: s[i+1:]})
	}
	return out, nil
}

func runInspect(args []string) error {
	// This command takes a path and no flags, so there is no flag set to catch -h
	// for it. Without this, `entangle inspect --help` treats the flag as a
	// filename and reports "open --help: no such file or directory", which reads
	// like a bug in the tool rather than an answer to the question asked.
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		fmt.Println("usage: entangle inspect <bundle>")
		fmt.Println()
		fmt.Println("Prints what a bundle contains: tool, kind, whether it was redacted,")
		fmt.Println("and the projects and session counts inside. Writes nothing.")
		return nil
	}
	if len(args) < 1 {
		return fmt.Errorf("usage: entangle inspect <bundle>")
	}
	mb, err := bundle.ReadManifest(args[0])
	if err != nil {
		return err
	}
	if len(mb) == 0 {
		return fmt.Errorf("no manifest.json found - is %q an entangle bundle?", args[0])
	}
	var man manifest.Manifest
	if err := json.Unmarshal(mb, &man); err != nil {
		return err
	}
	fmt.Printf("tool        : %s %s\n", man.Tool, man.ToolVersion)
	if man.IsSession() {
		fmt.Printf("kind        : single session (%s)\n", man.SessionID)
		fmt.Printf("redacted    : %v\n", man.Redacted)
	} else {
		fmt.Printf("kind        : full backup\n")
	}
	fmt.Printf("created     : %s\n", man.CreatedAt)
	fmt.Printf("source OS   : %s\n", man.Source.OS)
	fmt.Printf("source home : %s\n", man.Source.Home)
	fmt.Printf("includes    : %s\n", strings.Join(man.Includes, ", "))
	fmt.Printf("projects    : %d\n", len(man.Projects))
	for _, p := range man.Projects {
		path := p.OriginalPath
		if path == "" {
			path = "(unknown) " + p.EncodedFolder
		}
		fmt.Printf("  - %s  [%d session(s)]\n", path, p.Sessions)
	}
	return nil
}
