package attention

import (
	"errors"
	"path/filepath"
)

// StateBase is the directory sweep.json lives under: $XDG_STATE_HOME when
// it is absolute (a relative one is ignored, as the XDG spec requires), else
// <home>/.local/state.
func StateBase(getenv func(string) string, home string) (string, error) {
	if v := getenv("XDG_STATE_HOME"); v != "" && filepath.IsAbs(v) {
		return filepath.Clean(v), nil
	}
	if home == "" || !filepath.IsAbs(home) {
		return "", errors.New("attention: no home directory; set XDG_STATE_HOME")
	}
	return filepath.Join(home, ".local", "state"), nil
}

// SweepPath is where `forgectl sweep` writes its last result.
func SweepPath(stateBase string) string {
	return filepath.Join(stateBase, "cadence", "sweep.json")
}
