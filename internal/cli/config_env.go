// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Sources an envResolvedEntry can report besides the name of the environment
// variable that set it.
const (
	envSourceConfig  = "config"  // the config file sets it
	envSourceDefault = "default" // a built-in default applies
	envSourceUnset   = "unset"   // nothing sets it, and nothing is used
	envSourceError   = "error"   // it could not be resolved; value is the reason
)

// envResolvedEntry is one setting an environment variable can supply, as the
// verbs that read it resolve it (ADR-0008 rule 4, forgectl#1150). The entries
// above show what the file says; with FORGECTL_SESSIONS_DSN, HEARTH_DIR or
// DESK_DIR set they still read "" (default), so this is where the value in
// effect is shown, and which source supplied it.
type envResolvedEntry struct {
	Key string `json:"key"`
	// Value is what the verb uses: "" when nothing sets it, the reason when
	// Source is "error", and "(redacted)" for a set sessions.dsn.
	Value string `json:"value"`
	// Source is the environment variable that supplied Value, or "config",
	// "default", "unset" or "error".
	Source string `json:"source"`
}

// resolveEnvView resolves every setting an environment variable can supply,
// with the same precedence the verb that reads it uses.
func resolveEnvView(cfg config.Config, getenv func(string) string) []envResolvedEntry {
	return []envResolvedEntry{
		resolveDSNEntry(cfg.Sessions, getenv),
		resolveBenchDirEntry("bench.hearth_dir", cfg.Bench.HearthDir, "HEARTH_DIR", cfg.Bench.ResolveHearthDir, getenv),
		resolveBenchDirEntry("bench.chronicle_dir", cfg.Bench.ChronicleDir, "CHRONICLE_DIR", cfg.Bench.ResolveChronicleDir, getenv),
		resolveDeskDirEntry(getenv),
	}
}

// resolveDSNEntry is resolveDSN's precedence without the flag: the
// environment over the file. The value is withheld, as in the entries above.
func resolveDSNEntry(sc config.SessionsConfig, getenv func(string) string) envResolvedEntry {
	e := envResolvedEntry{Key: "sessions.dsn", Source: envSourceUnset}
	switch {
	case getenv("FORGECTL_SESSIONS_DSN") != "":
		e.Source = "FORGECTL_SESSIONS_DSN"
	case sc.DSN != "":
		e.Source = envSourceConfig
	default:
		return e
	}
	e.Value = redactedPlaceholder
	return e
}

// resolveBenchDirEntry reports a [bench] directory: the file's value, else
// the environment variable, else unset (config.resolveDir's order).
func resolveBenchDirEntry(key, configured, envVar string, resolve func() (string, error), getenv func(string) string) envResolvedEntry {
	e := envResolvedEntry{Key: key, Source: envSourceUnset}
	dir, err := resolve()
	switch {
	case err != nil:
		e.Source, e.Value = envSourceError, err.Error()
		return e
	case configured != "":
		e.Source = envSourceConfig
	case getenv(envVar) != "":
		e.Source = envVar
	}
	e.Value = dir
	return e
}

// resolveDeskDirEntry reports the desk directory every desk verb uses when
// --dir is not given (desk.ResolveDir's order).
func resolveDeskDirEntry(getenv func(string) string) envResolvedEntry {
	e := envResolvedEntry{Key: "desk.dir", Source: envSourceDefault}
	dir, err := resolveDeskDir("") // reads deskGetenv, which getenv mirrors
	if err != nil {
		e.Source, e.Value = envSourceError, err.Error()
		return e
	}
	switch {
	case getenv("DESK_DIR") != "":
		e.Source = "DESK_DIR"
	case getenv("CLAUDE_DESK_DIR") != "":
		e.Source = "CLAUDE_DESK_DIR"
	case filepath.IsAbs(getenv("XDG_STATE_HOME")):
		// A relative XDG_STATE_HOME is ignored, as the XDG spec requires.
		e.Source = "XDG_STATE_HOME"
	}
	e.Value = dir
	return e
}

// renderEnvResolvedText writes the resolved block of the human rendering.
// Values come from the environment and the file, so they are escaped here.
func renderEnvResolvedText(out io.Writer, entries []envResolvedEntry) {
	_, _ = fmt.Fprint(out, "\nresolved from the environment or the file (what the verbs use)\n")
	for _, e := range entries {
		value := e.Value
		if value == "" {
			value = `""`
		}
		_, _ = fmt.Fprintf(out, "  %-20s %-28s (%s)\n", e.Key, termsafe.SafeLineMax(value, labelMaxRunes*4), e.Source)
	}
}
