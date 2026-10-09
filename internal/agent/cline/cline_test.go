package cline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gowtham-sai-yadav/entangle/internal/agent"
)

const metaJSON = `{"session_id":"1780000000000_abcde","status":"running","cwd":"/old","workspace_root":"/old","prompt":"<user_input mode=\"act\">fix the bug</user_input>","messages_path":"/old/x","metadata":{"checkpoint":{"latest":{"ref":"deadbeef"}},"usage":{"inputTokens":9007199254740993}}}`
const msgsJSON = `{"version":1,"sessionId":"1780000000000_abcde","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"assistant","content":[{"type":"text","text":"hello"}]}]}`

func plant(t *testing.T, sessions, id string) {
	t.Helper()
	d := filepath.Join(sessions, id)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	m, g := filePaths(d, id)
	if err := os.WriteFile(m, []byte(metaJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(g, []byte(msgsJSON), 0o644); err != nil {
		t.Fatal(err)
	}
}

func roots(sessions string) agent.Roots {
	return agent.Roots{Extra: map[string]string{KeySessionsDir: sessions}}
}

func TestLocateAbsentWithoutSessionsDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if _, ok, err := (Provider{}).Locate(""); err != nil || ok {
		t.Fatalf("want absent without error, got ok=%v err=%v", ok, err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".cline", "data", "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := (Provider{}).Locate(""); !ok {
		t.Fatal("want present once the sessions directory exists")
	}
}

func TestListSessions(t *testing.T) {
	dir := t.TempDir()
	plant(t, dir, "1780000000000_abcde")
	got, err := Provider{}.ListSessions(roots(dir))
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v", got, err)
	}
	s := got[0]
	if s.Provider != agent.Cline || s.ProjectPath != "/old" || s.Title != "fix the bug" || s.Messages != 2 {
		t.Errorf("unexpected session: %+v", s)
	}
}

func TestRoundTripRebinds(t *testing.T) {
	src := t.TempDir()
	plant(t, src, "1780000000000_abcde")
	got, _ := Provider{}.ListSessions(roots(src))
	packed, err := Provider{}.Pack(roots(src), got[0], agent.PackOptions{})
	if err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	res, err := Provider{}.Unpack(roots(dst), packed.Entries, "/new/project")
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionID != "1780000000000_abcde" || res.Written != 2 {
		t.Errorf("unexpected result: %+v", res)
	}
	out, err := Provider{}.ListSessions(roots(dst))
	if err != nil || len(out) != 1 {
		t.Fatalf("imported session not listed: %v %v", out, err)
	}
	if out[0].ProjectPath != "/new/project" || out[0].Messages != 2 {
		t.Errorf("not rebound: %+v", out[0])
	}
	raw, _ := os.ReadFile(filepath.Join(dst, res.SessionID, res.SessionID+".json"))
	s := string(raw)
	for _, bad := range []string{"deadbeef", "/old", `"running"`} {
		if strings.Contains(s, bad) {
			t.Errorf("metadata still contains %q:\n%s", bad, s)
		}
	}
	if !strings.Contains(s, "9007199254740993") {
		t.Errorf("large number was not preserved exactly:\n%s", s)
	}
}

// An id that is already taken, or is not a safe directory name, must never
// overwrite or escape the store.
func TestUnpackMintsFreshID(t *testing.T) {
	src := t.TempDir()
	plant(t, src, "1780000000000_abcde")
	got, _ := Provider{}.ListSessions(roots(src))
	packed, _ := Provider{}.Pack(roots(src), got[0], agent.PackOptions{})

	dst := t.TempDir()
	plant(t, dst, "1780000000000_abcde")
	res, err := Provider{}.Unpack(roots(dst), packed.Entries, "/p")
	if err != nil || res.SessionID == "1780000000000_abcde" {
		t.Fatalf("collision must mint a new id, got %v %v", res, err)
	}

	for i := range packed.Entries {
		packed.Entries[i].Data = []byte(strings.ReplaceAll(string(packed.Entries[i].Data), "1780000000000_abcde", "../../evil"))
	}
	res, err = Provider{}.Unpack(roots(dst), packed.Entries, "/p")
	if err != nil || strings.Contains(res.SessionID, "..") {
		t.Fatalf("unsafe id must be replaced, got %v %v", res, err)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(dst), "evil")); statErr == nil {
		t.Error("wrote outside the sessions directory")
	}
}
