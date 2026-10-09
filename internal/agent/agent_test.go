package agent

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fake is a stand-in provider. The registry is package-global, so these tests
// register their own fakes here where no real provider is imported (importing one
// would be a cycle), which keeps them independent of what ships.
type fake struct {
	id        ID
	name      string
	present   bool
	locateErr error
	sessions  []Session
}

func (f fake) ID() ID              { return f.id }
func (f fake) DisplayName() string { return f.name }
func (f fake) Locate(override string) (Roots, bool, error) {
	if f.locateErr != nil {
		return Roots{}, false, f.locateErr
	}
	return Roots{Home: "/home/x", ConfigDir: override}, f.present, nil
}
func (f fake) ListSessions(Roots) ([]Session, error) { return f.sessions, nil }

func TestRegistryOrdersClaudeFirst(t *testing.T) {
	// Registered deliberately out of order.
	Register(fake{id: "zeta", name: "Zeta", present: true})
	Register(fake{id: ClaudeCode, name: "Claude Code", present: true})
	Register(fake{id: "alpha", name: "Alpha", present: true})
	t.Cleanup(func() { reset() })

	got := IDs()
	if len(got) < 3 || got[0] != string(ClaudeCode) {
		t.Fatalf("Claude Code must sort first (it is the default), got %v", got)
	}
	// The rest alphabetical, so listings are stable rather than map-random.
	rest := got[1:]
	for i := 1; i < len(rest); i++ {
		if rest[i-1] > rest[i] {
			t.Errorf("non-default providers should be alphabetical, got %v", rest)
			break
		}
	}
}

func TestRegisterRejectsDuplicates(t *testing.T) {
	Register(fake{id: "dup", name: "Dup"})
	t.Cleanup(func() { reset() })
	defer func() {
		if recover() == nil {
			t.Error("registering the same provider ID twice should panic; it is a programming error, not a runtime condition")
		}
	}()
	Register(fake{id: "dup", name: "Dup Again"})
}

func TestResolveErrors(t *testing.T) {
	Register(fake{id: "here", name: "Here", present: true})
	Register(fake{id: "gone", name: "Gone", present: false})
	Register(fake{id: "broken", name: "Broken", locateErr: errors.New("disk on fire")})
	t.Cleanup(func() { reset() })

	if _, err := Resolve("here", ""); err != nil {
		t.Errorf("an installed provider should resolve: %v", err)
	}

	// An unknown name should say what IS available, so a typo is self-correcting.
	_, err := Resolve("nope", "")
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("want an unknown-tool error, got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "here") {
		t.Errorf("the error should list available tools, got %v", err)
	}

	// A real tool that simply is not set up should say so, not return silence.
	_, err = Resolve("gone", "")
	if err == nil || !strings.Contains(err.Error(), "does not appear to be set up") {
		t.Errorf("want a not-installed error, got %v", err)
	}

	if _, err = Resolve("broken", ""); err == nil {
		t.Error("a Locate failure should surface, not be swallowed")
	}
}

// TestInstalledSkipsAbsentAndBroken: one unusable tool must never stop the others
// from being listed.
func TestInstalledSkipsAbsentAndBroken(t *testing.T) {
	Register(fake{id: ClaudeCode, name: "Claude Code", present: true})
	Register(fake{id: "absent", name: "Absent", present: false})
	Register(fake{id: "broken", name: "Broken", locateErr: errors.New("nope")})
	t.Cleanup(func() { reset() })

	got := Installed("")
	if len(got) != 1 || got[0].Provider.ID() != ClaudeCode {
		var ids []ID
		for _, b := range got {
			ids = append(ids, b.Provider.ID())
		}
		t.Fatalf("Installed() = %v, want only claude-code", ids)
	}
}

func TestSortSessionsNewestFirstAndStable(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	in := []Session{
		{ID: "old", ModTime: t0},
		{ID: "new", ModTime: t0.Add(time.Hour)},
		{ID: "b-tie", ModTime: t0.Add(30 * time.Minute)},
		{ID: "a-tie", ModTime: t0.Add(30 * time.Minute)},
	}
	SortSessions(in)
	want := []string{"new", "a-tie", "b-tie", "old"}
	for i, w := range want {
		if in[i].ID != w {
			var got []string
			for _, s := range in {
				got = append(got, s.ID)
			}
			t.Fatalf("SortSessions = %v, want %v", got, want)
		}
	}
}

func TestRootsGetMissingKey(t *testing.T) {
	if got := (Roots{}).Get("nothing"); got != "" {
		t.Errorf("Get on a nil Extra should be empty, got %q", got)
	}
	r := Roots{Extra: map[string]string{"k": "v"}}
	if r.Get("k") != "v" {
		t.Errorf("Get = %q, want v", r.Get("k"))
	}
}

// TestProviderIDsAreStable pins the wire values. They appear in `sessions --json`,
// which other programs parse, and will later be recorded inside bundles on other
// people's disks - so renaming one is a breaking change, not a rename.
func TestProviderIDsAreStable(t *testing.T) {
	for _, c := range []struct {
		got  ID
		want string
	}{
		{ClaudeCode, "claude-code"},
		{Codex, "codex"},
		{OpenCode, "opencode"},
		{Kilo, "kilo"},
		{Cline, "cline"},
	} {
		if string(c.got) != c.want {
			t.Errorf("provider ID = %q, want %q (these are a public contract)", c.got, c.want)
		}
	}
}

// reset clears the registry between tests.
func reset() {
	mu.Lock()
	defer mu.Unlock()
	providers = map[ID]Provider{}
}

// TestShortIDsGrowOnlyUntilUnique: the handle is what a user copies out of a
// listing and types back, so it has to identify one session - but no longer than
// it takes to do that.
func TestShortIDsGrowOnlyUntilUnique(t *testing.T) {
	// UUIDv7 ids, whose leading characters encode the timestamp: two sessions
	// started seconds apart share a long prefix, which is exactly the case a
	// fixed-width handle gets wrong.
	s := []Session{
		{ID: "019fa4443f2a7c1b8d01"},
		{ID: "019fa4443f2a7c1b8d99"},
		{ID: "8e3d21d1-aa11-4f00"},
	}
	AssignShortIDs(s, ShortIDMin)

	if s[0].ShortID == s[1].ShortID {
		t.Fatalf("time-ordered ids collided: both are %q", s[0].ShortID)
	}
	for i, x := range s {
		if !strings.HasPrefix(x.ID, x.ShortID) {
			t.Errorf("session %d: handle %q is not a prefix of id %q, so pasting it back would not match", i, x.ShortID, x.ID)
		}
	}
	if got := len(s[2].ShortID); got != ShortIDMin {
		t.Errorf("an id that needs no extra characters got %d of them, want %d", got, ShortIDMin)
	}
}

// TestShortIDsStayShortWhenTheFullIDIsAmbiguous: a session copied into a second
// project keeps its id, so no prefix can ever separate the two. Growing to the
// full 36-character uuid disambiguates nothing and widens every listing that
// contains it, so the handle must stay short and let --project decide.
func TestShortIDsStayShortWhenTheFullIDIsAmbiguous(t *testing.T) {
	dup := "9a16a391-a0d3-4eba-b71e-56c38c253c51"
	s := []Session{
		{ID: dup, ProjectPath: "/w/one"},
		{ID: dup, ProjectPath: "/w/two"},
		{ID: "1f9c77a2-bb22-4e11-8d1a-defdefdefdef"},
	}
	AssignShortIDs(s, ShortIDMin)

	for i := 0; i < 2; i++ {
		if got := len(s[i].ShortID); got != ShortIDMin {
			t.Errorf("duplicate id %d has a %d-character handle, want %d - lengthening it cannot disambiguate", i, got, ShortIDMin)
		}
	}
}

// TestShortIDsMustBeReassignedAcrossTools: each provider numbers its own sessions
// in isolation, so anything merging two lists has to redo the handles. Without it
// a listing shows one handle for two different sessions.
func TestShortIDsMustBeReassignedAcrossTools(t *testing.T) {
	claude := []Session{{Provider: ClaudeCode, ID: "abcdef1234-claude"}}
	other := []Session{{Provider: Codex, ID: "abcdef1234-codex"}}
	AssignShortIDs(claude, ShortIDMin)
	AssignShortIDs(other, ShortIDMin)
	if claude[0].ShortID != other[0].ShortID {
		t.Skip("ids no longer share a prefix; the merge hazard this guards is gone")
	}

	merged := append(append([]Session{}, claude...), other...)
	AssignShortIDs(merged, ShortIDMin)
	if merged[0].ShortID == merged[1].ShortID {
		t.Errorf("after merging, two sessions still share the handle %q", merged[0].ShortID)
	}
}

// TestShortIDsHandleAnIDShorterThanTheMinimum: a provider is free to use short
// ids, and asking for eight characters of a five-character id must not panic.
func TestShortIDsHandleAnIDShorterThanTheMinimum(t *testing.T) {
	s := []Session{{ID: "ab12"}, {ID: "cd34"}}
	AssignShortIDs(s, ShortIDMin)
	if s[0].ShortID != "ab12" || s[1].ShortID != "cd34" {
		t.Errorf("short ids were mangled: %q, %q", s[0].ShortID, s[1].ShortID)
	}
}
