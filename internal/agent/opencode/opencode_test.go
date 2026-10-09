package opencode

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gowtham-sai-yadav/entangle/internal/agent"
)

// The fixtures below mirror what a real `opencode db "<SQL>" --format json`
// returned on a live 1.18.5 install: a JSON array of rows, ids prefixed "ses_",
// directory holding the project path, and times as epoch milliseconds (confirmed
// against real data, not assumed).

// stub replaces the CLI for one test. Substituting the runner rather than planting
// an executable on PATH is what keeps these tests working on Windows.
func stub(t *testing.T, binary string, fn func(bin string, args ...string) ([]byte, error)) {
	t.Helper()
	oldLook, oldRun := lookPath, runCmd
	lookPath = func(string) (string, error) {
		if binary == "" {
			return "", errors.New("not found")
		}
		return binary, nil
	}
	runCmd = fn
	t.Cleanup(func() { lookPath, runCmd = oldLook, oldRun })
}

func rowsJSON(rows ...string) []byte {
	return []byte("[\n" + strings.Join(rows, ",\n") + "\n]\n")
}

func row(id, dir, title string, created, updated int64, messages int) string {
	return `{"id":"` + id + `","directory":"` + dir + `","title":"` + title +
		`","time_created":` + strconv.FormatInt(created, 10) +
		`,"time_updated":` + strconv.FormatInt(updated, 10) +
		`,"messages":` + strconv.Itoa(messages) + `}`
}

func TestRegisteredAndIdentity(t *testing.T) {
	p, ok := agent.Get(agent.OpenCode)
	if !ok {
		t.Fatal("importing this package should register the opencode provider")
	}
	if p.DisplayName() != "opencode" {
		t.Errorf("DisplayName = %q", p.DisplayName())
	}
}

// TestLocateAbsentWhenBinaryMissing: opencode not installed is a normal state, not
// an error, and must not be reported as present.
func TestLocateAbsentWhenBinaryMissing(t *testing.T) {
	stub(t, "", nil)
	r, ok, err := Provider{}.Locate("")
	if err != nil {
		t.Fatalf("a missing binary must not be an error: %v", err)
	}
	if ok {
		t.Error("should report not-present without the opencode binary")
	}
	if r.Get(KeyBinary) != "" {
		t.Errorf("binary should be empty, got %q", r.Get(KeyBinary))
	}
}

// TestLocateFastPath: the conventional database location is found with a stat, so
// a presence check does not have to launch a process.
func TestLocateFastPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	dataDir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dataDir, "opencode.db")
	if err := os.WriteFile(db, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	called := false
	stub(t, "/usr/local/bin/opencode", func(string, ...string) ([]byte, error) {
		called = true
		return nil, errors.New("should not be called")
	})

	r, ok, err := Provider{}.Locate("")
	if err != nil || !ok {
		t.Fatalf("want present, got ok=%v err=%v", ok, err)
	}
	if r.Get(KeyDBPath) != db {
		t.Errorf("db path = %q, want %q", r.Get(KeyDBPath), db)
	}
	if called {
		t.Error("the conventional path should be found by stat, without running the binary")
	}
}

// TestLocateAsksBinaryWhenPathUnconventional covers channel installs and an
// OPENCODE_DB override, where the database is not where we would guess.
func TestLocateAsksBinaryWhenPathUnconventional(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	elsewhere := filepath.Join(t.TempDir(), "opencode-dev.db")
	if err := os.WriteFile(elsewhere, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub(t, "/usr/local/bin/opencode", func(bin string, args ...string) ([]byte, error) {
		if len(args) == 2 && args[0] == "db" && args[1] == "path" {
			// The real CLI prints a banner before its answer.
			return []byte("opencode banner\n" + elsewhere + "\n"), nil
		}
		return nil, errors.New("unexpected args")
	})

	r, ok, err := Provider{}.Locate("")
	if err != nil || !ok {
		t.Fatalf("want present via the binary, got ok=%v err=%v", ok, err)
	}
	if r.Get(KeyDBPath) != elsewhere {
		t.Errorf("db path = %q, want %q (last non-empty line, past the banner)", r.Get(KeyDBPath), elsewhere)
	}
}

func TestListSessionsParsesRows(t *testing.T) {
	const created, updated = int64(1769500000000), int64(1769509999000)
	stub(t, "/usr/local/bin/opencode", func(bin string, args ...string) ([]byte, error) {
		if len(args) < 4 || args[0] != "db" || args[2] != "--format" || args[3] != "json" {
			return nil, errors.New("unexpected args: " + strings.Join(args, " "))
		}
		if !strings.Contains(args[1], "FROM session") {
			return nil, errors.New("expected a session query")
		}
		return rowsJSON(
			row("ses_05c823c9cffeagrVmgc2WkvY7o", "/Users/dev/api", "refactor the auth layer", created, updated, 8),
		), nil
	})

	r := agent.Roots{Extra: map[string]string{KeyBinary: "/usr/local/bin/opencode", KeyDBPath: "/db"}}
	got, err := Provider{}.ListSessions(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 session, got %d", len(got))
	}
	s := got[0]
	if s.Provider != agent.OpenCode {
		t.Errorf("Provider = %q", s.Provider)
	}
	if s.ID != "ses_05c823c9cffeagrVmgc2WkvY7o" {
		t.Errorf("ID = %q", s.ID)
	}
	if s.ProjectPath != "/Users/dev/api" {
		t.Errorf("ProjectPath = %q", s.ProjectPath)
	}
	if s.Title != "refactor the auth layer" {
		t.Errorf("Title = %q", s.Title)
	}
	if s.Messages != 8 {
		t.Errorf("Messages = %d, want 8", s.Messages)
	}
	// Times are epoch milliseconds; treating them as seconds would date sessions
	// to 1970 and sort the whole list wrongly.
	if want := time.UnixMilli(updated); !s.ModTime.Equal(want) {
		t.Errorf("ModTime = %v, want %v (times are epoch ms)", s.ModTime, want)
	}
	if s.GroupKey != "" {
		t.Errorf("GroupKey = %q; opencode has no path-derived on-disk bucket, so it must stay empty", s.GroupKey)
	}
	if s.Size != 0 {
		t.Errorf("Size = %d; a session is rows in a shared database and has no file size", s.Size)
	}
	ref, ok := s.Ref.(Ref)
	if !ok {
		t.Fatalf("Ref = %T, want opencode.Ref", s.Ref)
	}
	if ref.Binary == "" || ref.DBPath == "" {
		t.Errorf("Ref should carry how to reach the session again: %+v", ref)
	}
	if want := time.UnixMilli(created); !ref.Created.Equal(want) {
		t.Errorf("Ref.Created = %v, want %v", ref.Created, want)
	}
}

// TestShortIDsUniqueAndPrefixes: opencode ids embed a descending timestamp so
// sessions sort newest-first lexically, which makes siblings share a long prefix -
// the same hazard Codex has.
func TestShortIDsUniqueAndPrefixes(t *testing.T) {
	stub(t, "/bin/opencode", func(string, ...string) ([]byte, error) {
		return rowsJSON(
			row("ses_05c823c9cffeagrVmgc2WkvY7o", "/w", "a", 1, 3000, 1),
			row("ses_05c823c9cffeZZZZZZZZZZZZZZ", "/w", "b", 1, 2000, 1),
			row("ses_05c899066ffeEepPxa5nyy4Wqm", "/w", "c", 1, 1000, 1),
		), nil
	})
	got, err := Provider{}.ListSessions(agent.Roots{Extra: map[string]string{KeyBinary: "/bin/opencode"}})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, s := range got {
		if seen[s.ShortID] {
			t.Errorf("duplicate handle %q", s.ShortID)
		}
		seen[s.ShortID] = true
		if !strings.HasPrefix(s.ID, s.ShortID) {
			t.Errorf("ShortID %q is not a prefix of ID %q", s.ShortID, s.ID)
		}
	}
}

// TestSchemaChangeIsReportedNotSwallowed: if opencode changes its storage (a v2
// message model already exists in their tree), the user must be told the listing
// failed - not shown an empty list implying they have no sessions.
func TestSchemaChangeIsReportedNotSwallowed(t *testing.T) {
	stub(t, "/bin/opencode", func(string, ...string) ([]byte, error) {
		return []byte(`{"unexpected":"shape"}`), nil
	})
	_, err := Provider{}.ListSessions(agent.Roots{Extra: map[string]string{KeyBinary: "/bin/opencode"}})
	if err == nil {
		t.Fatal("an unparseable response must be an error, not an empty list")
	}
	if !strings.Contains(err.Error(), "format may have changed") {
		t.Errorf("the message should hint at the cause, got: %v", err)
	}
}

// TestQueryFailureSurfacesStderr: the CLI explains a bad query on stderr, so that
// text is what the user needs to see.
func TestQueryFailureSurfacesStderr(t *testing.T) {
	stub(t, "/bin/opencode", func(string, ...string) ([]byte, error) {
		return nil, errors.New("no such table: session")
	})
	_, err := Provider{}.ListSessions(agent.Roots{Extra: map[string]string{KeyBinary: "/bin/opencode"}})
	if err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("want the CLI's own explanation, got %v", err)
	}
}

// TestEmptyOutputIsNoSessions: a fresh install with no history is quiet.
func TestEmptyOutputIsNoSessions(t *testing.T) {
	stub(t, "/bin/opencode", func(string, ...string) ([]byte, error) { return []byte("\n"), nil })
	got, err := Provider{}.ListSessions(agent.Roots{Extra: map[string]string{KeyBinary: "/bin/opencode"}})
	if err != nil {
		t.Fatalf("no sessions should not be an error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 sessions, got %d", len(got))
	}
}

// TestNoBinaryListsNothing guards the path where Roots came from a machine without
// opencode: no subprocess, no error.
func TestNoBinaryListsNothing(t *testing.T) {
	got, err := Provider{}.ListSessions(agent.Roots{})
	if err != nil {
		t.Errorf("want no error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 sessions, got %d", len(got))
	}
}

func TestLastLineSkipsBanner(t *testing.T) {
	cases := map[string]string{
		"banner\n/path/to.db\n": "/path/to.db",
		"/only\n":               "/only",
		"a\nb\n\n\n":            "b",
		"":                      "",
	}
	for in, want := range cases {
		if got := lastLine(in); got != want {
			t.Errorf("lastLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanTitle(t *testing.T) {
	if got := cleanTitle("  hello \n world "); got != "hello" {
		t.Errorf("cleanTitle should take the first non-empty line, got %q", got)
	}
	long := strings.Repeat("x", 100)
	got := cleanTitle(long)
	if len([]rune(got)) > titleMaxLen+1 { // +1 for the ellipsis
		t.Errorf("title not truncated: %d runes", len([]rune(got)))
	}
}

func TestMsToTime(t *testing.T) {
	if !msToTime(0).IsZero() {
		t.Error("0 should mean unknown, not 1970")
	}
	if !msToTime(-5).IsZero() {
		t.Error("a negative timestamp should mean unknown")
	}
	if got := msToTime(1769500000000); got.IsZero() {
		t.Error("a real timestamp should convert")
	}
}

// TestKiloIsAVariant: Kilo Code's CLI reuses this implementation, so it must
// carry its own identity, binary, data directory and bundle prefix - a shared
// prefix would let one tool's bundle unpack into the other.
func TestKiloIsAVariant(t *testing.T) {
	p, ok := agent.Get(agent.Kilo)
	if !ok {
		t.Fatal("kilo provider not registered")
	}
	if p.DisplayName() != "Kilo Code" {
		t.Errorf("display name = %q", p.DisplayName())
	}
	sh, _ := agent.SharerFor(p)
	if sh.BundlePrefix() != "agents/kilo/" || sh.BundlePrefix() == (Provider{}).BundlePrefix() {
		t.Errorf("bundle prefix = %q", sh.BundlePrefix())
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dataDir := filepath.Join(home, ".local", "share", "kilo")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dataDir, "kilo.db")
	if err := os.WriteFile(db, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var looked string
	stub(t, "/usr/bin/kilo", nil)
	old := lookPath
	lookPath = func(n string) (string, error) { looked = n; return old(n) }
	r, ok2, err := p.Locate("")
	if err != nil || !ok2 || r.Get(KeyDBPath) != db || looked != "kilo" {
		t.Fatalf("locate: ok=%v err=%v db=%q looked=%q", ok2, err, r.Get(KeyDBPath), looked)
	}
}
