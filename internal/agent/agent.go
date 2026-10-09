// Package agent is the seam between entangle and the coding tools whose
// sessions it moves. Each supported tool implements Provider: it says where it
// keeps its data on this machine and what sessions are there, in one shared
// vocabulary, so the CLI, the TUI, and the web GUI never learn a second tool's
// storage layout.
//
// Two rules govern everything here.
//
// An ID that has shipped never changes. It is printed in `sessions --json`, which
// other programs parse, and later it will be recorded inside bundles on other
// people's disks.
//
// A tool that is not installed is not an error. Locate reports absence with
// ok=false, so a machine with only one coding tool behaves exactly as it did
// before this package existed - no warnings, no empty sections.
package agent

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// ID identifies a coding tool. These strings are a public contract.
type ID string

const (
	// ClaudeCode is the original and the default. Its value is also what
	// `sessions --json` emitted before any other tool was supported, so it must
	// stay exactly this.
	ClaudeCode ID = "claude-code"
	Codex      ID = "codex"
	OpenCode   ID = "opencode"
	Kilo       ID = "kilo"
	Cline      ID = "cline"
)

// Roots is where one tool keeps its data on this machine. Tool-specific
// locations live in Extra so adding a provider never changes this struct - and
// so nothing outside a provider is tempted to depend on them.
type Roots struct {
	Home      string // the user's home directory
	ConfigDir string // the tool's own config/data root
	Extra     map[string]string
}

// Get returns an Extra value, or "" when absent.
func (r Roots) Get(key string) string { return r.Extra[key] }

// Session is one recorded conversation, described the same way for every tool.
// It carries no tool-specific fields on purpose: anything a provider needs to
// find the underlying bytes again belongs in Ref.
type Session struct {
	Provider ID

	// ID is opaque and only meaningful to its provider. Claude Code uses a UUID;
	// Codex uses a rollout filename stem. Nothing outside a provider may parse it.
	ID string

	// ShortID is a display handle, supplied by the provider rather than derived
	// by slicing ID, because not every tool names sessions in a way where the
	// first few characters distinguish anything.
	ShortID string

	// ProjectPath is the true absolute path of the project this session belongs
	// to, or "" when the tool did not record one recoverably.
	ProjectPath string

	// GroupKey is the provider's own on-disk bucket for the session (for Claude
	// Code, the encoded folder name). Empty when a tool does not bucket by path.
	GroupKey string

	Title    string
	Messages int
	ModTime  time.Time
	Size     int64

	// Ref is a provider-private handle to the underlying storage. It is never
	// serialised and callers must treat it as opaque.
	Ref any
}

// Provider is what a supported coding tool implements.
//
// It is deliberately small: it covers finding and listing sessions, which is
// what every caller needs today. Packing and unpacking are added when there is a
// second implementation to design them against, rather than guessed at now.
type Provider interface {
	// ID is the stable identifier for this tool.
	ID() ID

	// DisplayName is how the tool is named to a person ("Claude Code").
	DisplayName() string

	// Locate resolves this tool's roots, honouring an explicit override, then the
	// tool's own environment variable, then its default location. ok=false means
	// the tool is not present on this machine, which is not an error. An error is
	// reserved for a genuine problem, such as an unreadable home directory.
	Locate(override string) (r Roots, ok bool, err error)

	// ListSessions returns every session under r, newest first.
	ListSessions(r Roots) ([]Session, error)
}

// Entry is one file inside a bundle, named relative to its provider's prefix.
type Entry struct {
	Name string
	Data []byte
}

// Preview is what the user is shown before a session leaves the machine. Sharing
// is irreversible once a code is read out, so this exists to make the decision an
// informed one rather than a prompt to click through.
type Preview struct {
	Title         string
	ShortID       string
	ProjectPath   string
	Messages      int
	Bytes         int64 // uncompressed size of what will be sent
	SecretsMasked int
	AgentVersion  string // the tool's version on this machine, recorded for diagnosis
}

// Packed is one session captured in memory, ready to be written to a file or
// streamed over a transfer. Nothing has left the machine yet.
type Packed struct {
	Entries []Entry
	Preview Preview
}

// PackOptions carries the policy a provider must apply while packing.
type PackOptions struct {
	// Scrub masks likely secrets and reports how many it found. It is supplied by
	// the caller rather than chosen by the provider, so that "what gets redacted"
	// stays one decision in one place for every tool.
	Scrub func([]byte) ([]byte, int)
}

// Unpacked reports what landed after an unpack.
type Unpacked struct {
	SessionID string // the id the session now has on this machine
	Written   int
	// ResumeHint is the exact thing to type to carry on, e.g.
	// "codex resume 019f...". Different tools resume differently and a user should
	// not have to know which.
	ResumeHint string
}

// Sharer is implemented by providers whose individual sessions can be packed up
// and put back somewhere else.
//
// It is separate from Provider because Claude Code deliberately does not
// implement it: its bundle layout is frozen for compatibility with every released
// binary and is produced by the original exporter. Keeping that path untouched is
// worth more than making the interface look uniform.
type Sharer interface {
	// Pack captures one session in memory, scrubbed, without writing anything.
	Pack(r Roots, s Session, opts PackOptions) (*Packed, error)

	// Unpack writes a packed session into this machine's copy of the tool and
	// binds it to targetDir, so it resumes against the project the receiver is
	// actually standing in rather than the sender's path.
	Unpack(r Roots, entries []Entry, targetDir string) (*Unpacked, error)

	// BundlePrefix is where this tool's files live inside a bundle. It must never
	// collide with the Claude layout: a binary released before multi-tool support
	// routes purely on these prefixes, and would otherwise unpack a foreign
	// session into a user's Claude directory.
	BundlePrefix() string
}

// SharerFor returns the Sharer for a provider, if it has one.
func SharerFor(p Provider) (Sharer, bool) {
	s, ok := p.(Sharer)
	return s, ok
}

var (
	mu        sync.RWMutex
	providers = map[ID]Provider{}
)

// Register adds a provider. Providers register from an init function in their own
// package, so importing a provider is what enables it and nothing has to maintain
// a central list. Registering the same ID twice is a programming error.
func Register(p Provider) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := providers[p.ID()]; dup {
		panic(fmt.Sprintf("agent: provider %q registered twice", p.ID()))
	}
	providers[p.ID()] = p
}

// Unregister removes a provider. It exists so a test can install a stand-in and
// clean up after itself; nothing in the running program unregisters anything.
func Unregister(id ID) {
	mu.Lock()
	defer mu.Unlock()
	delete(providers, id)
}

// Get returns the provider with this ID.
func Get(id ID) (Provider, bool) {
	mu.RLock()
	defer mu.RUnlock()
	p, ok := providers[id]
	return p, ok
}

// All returns every registered provider, ordered so Claude Code comes first and
// the rest follow alphabetically. Output order is a user-visible detail, so it is
// defined here rather than left to map iteration.
func All() []Provider {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Provider, 0, len(providers))
	for _, p := range providers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].ID() == ClaudeCode) != (out[j].ID() == ClaudeCode) {
			return out[i].ID() == ClaudeCode
		}
		return out[i].ID() < out[j].ID()
	})
	return out
}

// IDs lists the registered identifiers, in All's order, for help text and errors.
func IDs() []string {
	ps := All()
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, string(p.ID()))
	}
	return out
}

// Bound is a provider together with the roots it resolved to on this machine.
type Bound struct {
	Provider Provider
	Roots    Roots
}

// Installed returns the providers actually present on this machine, in All's
// order. override is passed through to each Locate; it is only meaningful when
// resolving a single provider, so callers pass "" when scanning them all.
//
// A provider whose Locate fails outright is skipped rather than failing the whole
// scan: one broken tool must not stop the others being listed.
func Installed(override string) []Bound {
	var out []Bound
	for _, p := range All() {
		r, ok, err := p.Locate(override)
		if err != nil || !ok {
			continue
		}
		out = append(out, Bound{Provider: p, Roots: r})
	}
	return out
}

// Resolve looks up one provider by ID and locates it, with errors phrased for a
// user who typed --tool. An unknown name lists what is available; a known tool
// that is not installed says so plainly rather than returning an empty list.
func Resolve(id ID, override string) (Bound, error) {
	p, ok := Get(id)
	if !ok {
		return Bound{}, fmt.Errorf("unknown tool %q (available: %v)", id, IDs())
	}
	r, present, err := p.Locate(override)
	if err != nil {
		return Bound{}, fmt.Errorf("locate %s: %w", p.DisplayName(), err)
	}
	if !present {
		return Bound{}, fmt.Errorf("%s does not appear to be set up on this machine", p.DisplayName())
	}
	return Bound{Provider: p, Roots: r}, nil
}

// ShortIDMin is the narrowest handle a listing shows. Claude Code sessions have
// always displayed at eight characters, so starting every tool there keeps a
// mixed list visually even, and prefixes grow only where ids actually collide.
//
// Anything merging sessions from more than one tool must re-run AssignShortIDs
// over the combined slice: each provider only sees its own sessions, so handles
// that are unique per tool can still collide once the lists are joined.
const ShortIDMin = 8

// AssignShortIDs sets each ShortID to the shortest prefix of ID that is unique
// within s, never shorter than min. This is the treatment git gives commit
// hashes, and it is necessary for any tool whose ids are time-ordered: Codex uses
// UUIDv7, whose leading characters encode the timestamp, so two sessions started
// moments apart share a long prefix and a fixed-width handle would name both.
//
// Keeping the handle a genuine prefix matters: a user copies it from a listing and
// passes it back as an id, so it has to still match.
func AssignShortIDs(s []Session, min int) {
	if min < 1 {
		min = 1
	}
	for i := range s {
		n := min
		if n > len(s[i].ID) {
			n = len(s[i].ID)
		}
		// Grow only while growing helps. Tools do reuse an id across projects - a
		// Claude session copied into a second folder keeps its uuid - and for those
		// no prefix is ever unique, so lengthening to the full id buys nothing and
		// costs a listing its whole column width. Leave those at the short handle and
		// let --project separate them.
		if uniquePrefix(s, i, len(s[i].ID)) {
			for n < len(s[i].ID) && !uniquePrefix(s, i, n) {
				n++
			}
		}
		s[i].ShortID = s[i].ID[:n]
	}
}

// uniquePrefix reports whether s[i].ID[:n] matches no other session's id.
func uniquePrefix(s []Session, i, n int) bool {
	if n > len(s[i].ID) {
		return false
	}
	p := s[i].ID[:n]
	for j := range s {
		if j == i {
			continue
		}
		if len(s[j].ID) >= n && s[j].ID[:n] == p {
			return false
		}
	}
	return true
}

// SortSessions orders sessions newest first, which is how every caller shows
// them. Ties break on ID so the order is stable rather than arbitrary.
func SortSessions(s []Session) {
	sort.SliceStable(s, func(i, j int) bool {
		if !s[i].ModTime.Equal(s[j].ModTime) {
			return s[i].ModTime.After(s[j].ModTime)
		}
		return s[i].ID < s[j].ID
	})
}
