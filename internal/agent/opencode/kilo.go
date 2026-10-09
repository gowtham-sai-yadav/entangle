package opencode

import "github.com/gowtham-sai-yadav/entangle/internal/agent"

// Kilo Code's CLI is a fork of opencode that kept the commands and the SQLite
// schema this package relies on (`kilo db`, `kilo export`, `kilo import`,
// `kilo --session`), moving only the names and the data directory. So it is the
// same provider with a different variant rather than a second implementation to
// keep in step.
//
// Kilo's VS Code extension, which descends from Cline and Roo and keeps task
// folders instead of a database, is a different format and is not covered here.
var kiloVariant = variant{
	id:      agent.Kilo,
	display: "Kilo Code",
	binary:  "kilo",
	dataDir: []string{".local", "share", "kilo"},
	dbFile:  "kilo.db",
	prefix:  "agents/kilo/",
}

func init() { agent.Register(Provider{v: &kiloVariant}) }
