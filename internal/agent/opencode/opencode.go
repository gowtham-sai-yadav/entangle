// Package opencode implements the agent.Provider contract for opencode.
//
// opencode keeps sessions in a SQLite database, not in per-session files, so
// there is nothing to read directly the way there is for Claude Code and Codex.
// This package therefore drives opencode's own CLI (`opencode db "<SQL>"
// --format json`) instead of opening the database file.
//
// That choice is deliberate and worth the subprocess cost:
//
//   - No SQLite driver. The pure-Go ones are large and the common one needs cgo,
//     which would cost this project its single static cross-compiled binary.
//   - No locking risk. opencode runs the database in WAL mode with foreign keys
//     on; a second process poking at the file while the TUI is live is a way to
//     corrupt someone's history, and reading through the owner avoids it.
//   - The binary resolves its own location, so channel installs and an OPENCODE_DB
//     override work without this package knowing the rules.
//
// The cost is that the SQL couples to opencode's table names. That is a real
// exposure: a v2 message model already exists in their tree. So a failed query is
// reported as a plain "could not read" rather than crashing, and the listing
// asks only for the columns it genuinely needs.
//
// Importing this package registers the provider.
package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gowtham-sai-yadav/entangle/internal/agent"
)

// Keys for opencode-specific locations carried in agent.Roots.Extra.
const (
	KeyBinary = "opencodeBinary" // absolute path to the opencode executable
	KeyDBPath = "opencodeDB"     // absolute path to opencode.db
)

const (
	// queryTimeout bounds the subprocess. A hung CLI must not hang a listing.
	queryTimeout = 20 * time.Second
	titleMaxLen  = 60
	shortIDMin   = 8
)

// listQuery asks for one row per top-level session.
//
// parent_id IS NULL keeps subagent sessions out, matching how Claude Code's
// subagent logs are part of their parent session rather than separate entries.
// Archived sessions are excluded because opencode hides them too, and a listing
// that offers something the owning tool considers gone is misleading.
const listQuery = `SELECT s.id, s.directory, s.title, s.time_created, s.time_updated,
  (SELECT COUNT(*) FROM message m WHERE m.session_id = s.id) AS messages
FROM session s
WHERE s.parent_id IS NULL AND s.time_archived IS NULL
ORDER BY s.time_updated DESC`

// Indirection for the two things this package does to the outside world, so
// tests can substitute them. A test that instead planted a stub executable on
// PATH would need a shell script, which does not run on Windows - and this code
// has to keep working there.
var (
	lookPath = exec.LookPath
	runCmd   = execRunner
)

// execRunner runs bin with args and returns stdout, folding stderr into the
// error because that is where the CLI explains a bad query.
func execRunner(bin string, args ...string) ([]byte, error) {
	return execRunnerIn("", bin, args...)
}

// execRunnerIn is execRunner with a working directory, which is how opencode is
// told which project an imported session belongs to.
func execRunnerIn(dir, bin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("opencode did not respond within %s", queryTimeout)
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, errors.New(firstLine(msg))
		}
		return nil, err
	}
	return out, nil
}

// variant is what differs between opencode and the forks that kept its CLI and
// storage layout. Kilo Code's CLI is one: same `db`, `export` and `import`
// commands, same schema, different names. Everything else is shared code.
type variant struct {
	id      agent.ID
	display string
	binary  string   // executable looked up on PATH
	dataDir []string // data directory, relative to home
	dbFile  string   // database file name inside dataDir
	prefix  string   // bundle prefix, which must be unique per tool
}

var opencodeVariant = variant{
	id:      agent.OpenCode,
	display: "opencode",
	binary:  "opencode",
	dataDir: []string{".local", "share", "opencode"},
	dbFile:  "opencode.db",
	prefix:  "agents/opencode/",
}

// Provider is the opencode implementation of agent.Provider. The zero value is
// opencode itself; a fork is a Provider carrying its own variant.
type Provider struct{ v *variant }

func init() { agent.Register(Provider{}) }

func (p Provider) variant() variant {
	if p.v == nil {
		return opencodeVariant
	}
	return *p.v
}

func (p Provider) ID() agent.ID { return p.variant().id }

func (p Provider) DisplayName() string { return p.variant().display }

// Locate finds the opencode binary and its database.
//
// Both are required to report present: the database is only readable by asking
// the binary, so one without the other is not a usable installation. An override
// names the database directly, for pointing at a copy.
func (p Provider) Locate(override string) (agent.Roots, bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return agent.Roots{}, false, err
	}
	v := p.variant()
	bin, lookErr := lookPath(v.binary)
	dataDir := filepath.Join(append([]string{home}, v.dataDir...)...)
	roots := agent.Roots{
		Home:      home,
		ConfigDir: dataDir,
		Extra:     map[string]string{KeyBinary: bin, KeyDBPath: ""},
	}
	if lookErr != nil {
		return roots, false, nil // not installed: not an error
	}

	if override != "" {
		roots.Extra[KeyDBPath] = override
		return roots, true, nil
	}

	// Fast path: the conventional location, so a presence check is a stat rather
	// than a process launch. Only when that misses is the binary asked, which
	// covers channel installs (opencode-dev.db) and an OPENCODE_DB override.
	db := filepath.Join(dataDir, v.dbFile)
	if fi, statErr := os.Stat(db); statErr == nil && !fi.IsDir() {
		roots.Extra[KeyDBPath] = db
		return roots, true, nil
	}
	if resolved, rerr := askDBPath(bin); rerr == nil && resolved != "" {
		if fi, statErr := os.Stat(resolved); statErr == nil && !fi.IsDir() {
			roots.Extra[KeyDBPath] = resolved
			return roots, true, nil
		}
	}
	return roots, false, nil
}

// askDBPath runs `opencode db path`.
func askDBPath(bin string) (string, error) {
	out, err := runCmd(bin, "db", "path")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(lastLine(string(out))), nil
}

// sessionRow is one row of listQuery. Times are epoch milliseconds, which was
// confirmed against a real database rather than assumed.
type sessionRow struct {
	ID          string `json:"id"`
	Directory   string `json:"directory"`
	Title       string `json:"title"`
	TimeCreated int64  `json:"time_created"`
	TimeUpdated int64  `json:"time_updated"`
	Messages    int    `json:"messages"`
}

// Ref is the provider-private handle to a session.
type Ref struct {
	Binary  string // the opencode executable that owns this session
	DBPath  string // the database it lives in
	Created time.Time
}

// ListSessions reads sessions by querying through the opencode binary.
func (p Provider) ListSessions(r agent.Roots) ([]agent.Session, error) {
	bin := r.Get(KeyBinary)
	if bin == "" {
		return nil, nil // opencode is not installed; nothing to list
	}
	raw, err := runQuery(bin, listQuery)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil // no sessions yet
	}
	var rows []sessionRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		// The schema changed under us, or the CLI printed something unexpected.
		// Say so plainly; do not pretend the user has no sessions.
		return nil, fmt.Errorf("could not read opencode's session list (its storage format may have changed; "+
			"try updating entangle): %w", err)
	}

	out := make([]agent.Session, 0, len(rows))
	for _, row := range rows {
		if row.ID == "" {
			continue
		}
		out = append(out, agent.Session{
			Provider:    p.ID(),
			ID:          row.ID,
			ShortID:     row.ID, // replaced below by a unique prefix
			ProjectPath: row.Directory,
			// opencode groups by project id internally, not by a path-derived
			// folder, and it re-homes a session on import. There is no on-disk
			// bucket to report, so this stays empty by design.
			GroupKey: "",
			Title:    cleanTitle(row.Title),
			Messages: row.Messages,
			ModTime:  msToTime(row.TimeUpdated),
			// Size is left zero: a session is rows in a shared database, so it has
			// no file size, and inventing one would be a lie in the listing.
			Ref: Ref{Binary: bin, DBPath: r.Get(KeyDBPath), Created: msToTime(row.TimeCreated)},
		})
	}
	agent.SortSessions(out)
	// opencode ids embed a descending timestamp so that sessions sort newest-first
	// lexically, which means siblings share a long prefix - the same hazard Codex
	// has. Grow each handle until it is unique.
	agent.AssignShortIDs(out, shortIDMin)
	return out, nil
}

// runQuery executes one SQL statement through the opencode CLI.
func runQuery(bin, sql string) ([]byte, error) {
	out, err := runCmd(bin, "db", sql, "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("opencode db failed: %w", err)
	}
	return out, nil
}

// msToTime converts epoch milliseconds, tolerating a zero as "unknown".
func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// cleanTitle reduces a stored title to a single display line.
func cleanTitle(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		return truncate(line, titleMaxLen)
	}
	return ""
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimSpace(string(r[:max])) + "…"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// lastLine returns the final non-empty line, because the CLI prints a banner
// before its answer.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

var _ agent.Provider = Provider{}
