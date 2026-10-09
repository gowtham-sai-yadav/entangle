// Package cline implements agent.Provider and agent.Sharer for Cline (3.x), both
// its CLI and its VS Code extension, which share one store.
//
// A session is a folder under ~/.cline/data/sessions/<id>/ holding two JSON
// files: <id>.json, the metadata (cwd, workspace root, model, title, status), and
// <id>.messages.json, the conversation. There is no database to drive, and the
// index Cline keeps in db/sessions.db is rebuilt from these files: a session
// placed on disk shows up in `cline history` without any registration. That was
// confirmed against a scratch HOME rather than assumed.
//
// The older Cline extension layout (globalStorage/saoudrizwan.claude-dev/tasks)
// is a different format and is not read here.
//
// Importing this package registers the provider.
package cline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gowtham-sai-yadav/entangle/internal/agent"
)

// KeySessionsDir is where the session folders live, carried in agent.Roots.Extra.
const KeySessionsDir = "clineSessionsDir"

const (
	titleMaxLen = 60
	shortIDMin  = 8
)

// Provider is the Cline implementation of agent.Provider.
type Provider struct{}

func init() { agent.Register(Provider{}) }

func (Provider) ID() agent.ID { return agent.Cline }

func (Provider) DisplayName() string { return "Cline" }

// Locate resolves the Cline data root (default ~/.cline). Presence is judged by
// the sessions directory, so a machine that only ever configured Cline reads as
// absent. An explicit override is always treated as present.
func (Provider) Locate(override string) (agent.Roots, bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return agent.Roots{}, false, err
	}
	root := override
	if root == "" {
		root = filepath.Join(home, ".cline")
	}
	sessions := filepath.Join(root, "data", "sessions")
	roots := agent.Roots{
		Home:      home,
		ConfigDir: root,
		Extra:     map[string]string{KeySessionsDir: sessions},
	}
	if override != "" {
		return roots, true, nil
	}
	fi, statErr := os.Stat(sessions)
	return roots, statErr == nil && fi.IsDir(), nil
}

// Ref is the provider-private handle to a session folder.
type Ref struct {
	Dir string // absolute path of <sessions>/<id>
}

// meta is the part of <id>.json that listing needs. Everything else is left alone
// so a field Cline adds later cannot break reading.
type meta struct {
	SessionID     string `json:"session_id"`
	StartedAt     string `json:"started_at"`
	EndedAt       string `json:"ended_at"`
	CWD           string `json:"cwd"`
	WorkspaceRoot string `json:"workspace_root"`
	Prompt        string `json:"prompt"`
	IsSubagent    bool   `json:"is_subagent"`
	ParentSession string `json:"parent_session_id"`
	Metadata      struct {
		Title string `json:"title"`
	} `json:"metadata"`
}

// ListSessions reads every session folder under the sessions directory.
//
// A folder that cannot be read is skipped rather than failing the listing: one
// half-written session must not hide the rest.
func (Provider) ListSessions(r agent.Roots) ([]agent.Session, error) {
	dir := r.Get(KeySessionsDir)
	if dir == "" {
		return nil, nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []agent.Session
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		sdir := filepath.Join(dir, id)
		metaPath, msgPath := filePaths(sdir, id)
		raw, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var m meta
		if json.Unmarshal(raw, &m) != nil || m.IsSubagent || m.ParentSession != "" {
			continue
		}
		project := m.WorkspaceRoot
		if project == "" {
			project = m.CWD
		}
		size := int64(len(raw))
		mod := parseTime(m.EndedAt)
		if fi, err := os.Stat(msgPath); err == nil {
			size += fi.Size()
			if fi.ModTime().After(mod) {
				mod = fi.ModTime()
			}
		}
		if mod.IsZero() {
			mod = parseTime(m.StartedAt)
		}
		out = append(out, agent.Session{
			Provider:    agent.Cline,
			ID:          id,
			ShortID:     id,
			ProjectPath: project,
			Title:       titleOf(m),
			Messages:    countMessages(msgPath),
			ModTime:     mod,
			Size:        size,
			Ref:         Ref{Dir: sdir},
		})
	}
	agent.SortSessions(out)
	// Ids are "<epoch ms>_<random>", so siblings share a long prefix.
	agent.AssignShortIDs(out, shortIDMin)
	return out, nil
}

// filePaths names the two files inside a session folder.
func filePaths(dir, id string) (meta, messages string) {
	return filepath.Join(dir, id+".json"), filepath.Join(dir, id+".messages.json")
}

func countMessages(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var doc struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return 0
	}
	return len(doc.Messages)
}

// titleOf prefers Cline's own title and falls back to the first prompt, which the
// CLI wraps in a <user_input> tag that is noise in a listing.
func titleOf(m meta) string {
	t := strings.TrimSpace(m.Metadata.Title)
	if t == "" {
		t = stripUserInput(m.Prompt)
	}
	for _, line := range strings.Split(t, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return truncate(line, titleMaxLen)
		}
	}
	return ""
}

func stripUserInput(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "<user_input") {
		if i := strings.IndexByte(s, '>'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "</user_input>")
	}
	return s
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimSpace(string(r[:max])) + "…"
}

var _ agent.Provider = Provider{}
