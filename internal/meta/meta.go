// Package meta carries build-time identity for the binary.
//
// AppName is the single source of truth for the tool's name: a rename is a
// one-liner here (plus the go.mod path and the goreleaser binary name).
package meta

// AppName is the binary name and the root command's Use string.
const AppName = "forgectl"

// Tagline is the one-line description shown in help and the TUI header.
const Tagline = "a personal dev-experience CLI for the headless workbench"

// Build-time variables, injected via -ldflags by goreleaser. Defaults keep
// `go run` and local builds honest about being unreleased.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)
