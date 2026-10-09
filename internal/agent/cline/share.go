package cline

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gowtham-sai-yadav/entangle/internal/agent"
)

// Sharing a Cline session.
//
// Both files travel as they are, scrubbed, and on arrival three things are
// rewritten where Cline reads them structurally:
//
//   - cwd and workspace_root become the receiver's directory, so the session
//     resumes against the project they are standing in.
//   - messages_path and the session id inside both files follow the folder the
//     session lands in. The id is kept unless it is unusable or already taken
//     here, in which case a fresh one is minted rather than overwriting someone's
//     existing session.
//   - metadata.checkpoint is dropped. Its refs point into a shadow git repository
//     on the sender's machine, which does not exist on the receiver's.
//
// Prose inside the conversation is left alone.

const bundlePrefix = "agents/cline/"

const (
	metaEntry     = bundlePrefix + "session.json"
	messagesEntry = bundlePrefix + "messages.json"
)

// BundlePrefix implements agent.Sharer.
func (Provider) BundlePrefix() string { return bundlePrefix }

// safeID is what an id may look like. The bundle is untrusted input and the id
// becomes a directory name, so anything else is replaced, never joined to a path.
var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Pack reads both files of a session and scrubs them.
func (Provider) Pack(r agent.Roots, s agent.Session, opts agent.PackOptions) (*agent.Packed, error) {
	ref, ok := s.Ref.(Ref)
	if !ok || ref.Dir == "" {
		return nil, fmt.Errorf("session %s has no folder on this machine", s.ShortID)
	}
	id := filepath.Base(ref.Dir)
	metaPath, msgPath := filePaths(ref.Dir, id)

	var entries []agent.Entry
	var total int64
	masked := 0
	for _, f := range []struct{ path, entry string }{{metaPath, metaEntry}, {msgPath, messagesEntry}} {
		raw, err := os.ReadFile(f.path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", filepath.Base(f.path), err)
		}
		if !json.Valid(raw) {
			return nil, fmt.Errorf("%s is not valid JSON (Cline's format may have changed)", filepath.Base(f.path))
		}
		if opts.Scrub != nil {
			// Scrubbing edits bytes inside a JSON document, so confirm it still is one.
			if scrubbed, n := opts.Scrub(raw); json.Valid(scrubbed) {
				raw, masked = scrubbed, masked+n
			}
		}
		total += int64(len(raw))
		entries = append(entries, agent.Entry{Name: f.entry, Data: raw})
	}

	return &agent.Packed{
		Entries: entries,
		Preview: agent.Preview{
			Title:         s.Title,
			ShortID:       s.ShortID,
			ProjectPath:   s.ProjectPath,
			Messages:      s.Messages,
			Bytes:         total,
			SecretsMasked: masked,
		},
	}, nil
}

// Unpack writes the session into this machine's Cline store, bound to targetDir.
func (Provider) Unpack(r agent.Roots, entries []agent.Entry, targetDir string) (*agent.Unpacked, error) {
	sessions := r.Get(KeySessionsDir)
	if sessions == "" {
		return nil, errors.New("Cline does not appear to be set up on this machine")
	}
	var metaRaw, msgRaw []byte
	for _, e := range entries {
		switch e.Name {
		case metaEntry:
			metaRaw = e.Data
		case messagesEntry:
			msgRaw = e.Data
		}
	}
	if metaRaw == nil || msgRaw == nil {
		return nil, errors.New("this bundle has no complete Cline session in it")
	}
	meta, err := decodeObject(metaRaw)
	if err != nil {
		return nil, fmt.Errorf("session metadata: %w", err)
	}
	msgs, err := decodeObject(msgRaw)
	if err != nil {
		return nil, fmt.Errorf("session messages: %w", err)
	}

	id, _ := meta["session_id"].(string)
	if !safeID.MatchString(id) || exists(filepath.Join(sessions, id)) {
		id = newID()
		for exists(filepath.Join(sessions, id)) {
			id = newID()
		}
	}
	dir := filepath.Join(sessions, id)
	metaPath, msgPath := filePaths(dir, id)

	meta["session_id"] = id
	meta["cwd"] = targetDir
	meta["workspace_root"] = targetDir
	meta["messages_path"] = msgPath
	// A session captured mid-run has no process behind it here.
	if meta["status"] == "running" {
		meta["status"] = "completed"
	}
	if md, ok := meta["metadata"].(map[string]any); ok {
		delete(md, "checkpoint")
	}
	msgs["sessionId"] = id

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		path string
		doc  map[string]any
	}{{msgPath, msgs}, {metaPath, meta}} { // metadata last: it is what makes the folder a session
		b, err := json.MarshalIndent(f.doc, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(f.path, b, 0o644); err != nil {
			return nil, err
		}
	}
	return &agent.Unpacked{
		SessionID:  id,
		Written:    2,
		ResumeHint: "cline --id " + id + "   (from " + targetDir + ")",
	}, nil
}

// decodeObject parses a JSON object keeping numbers exact, so ids and timestamps
// survive the rewrite untouched.
func decodeObject(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("not a JSON object")
	}
	return m, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// newID mints an id in Cline's own shape, "<epoch ms>_<5 base36 chars>".
func newID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return strconv.FormatInt(time.Now().UnixMilli(), 10) + "_" + strings.ToLower(string(b))
}

var _ agent.Sharer = Provider{}
