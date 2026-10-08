package worker

import (
	"crypto/rand"
	"fmt"
	"path/filepath"
	"strings"
)

// NewSessionID returns a random version 4 UUID in lowercase, for a claude
// worker's --session-id.
func NewSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("worker: session id: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// TranscriptPath is where Claude Code writes the transcript of session id
// started in dir, given the environment the harness runs with:
// <config dir>/projects/<slug>/<id>.jsonl, where the config dir is
// CLAUDE_CONFIG_DIR or ~/.claude and the slug is dir with every character
// other than an ASCII letter or digit replaced by '-'. It returns "" when the
// environment names neither a config dir nor a home.
//
// debt: Claude Code shortens very long slugs with a hash suffix; a worktree
// path past 200 characters gets a path that does not exist. Upgrade when a
// worker's transcript is read by anything that fails on a missing file.
func TranscriptPath(env []string, dir, id string) string {
	base := envValue(env, "CLAUDE_CONFIG_DIR")
	if base == "" {
		home := envValue(env, "HOME")
		if home == "" {
			return ""
		}
		base = filepath.Join(home, ".claude")
	}
	return filepath.Join(base, "projects", projectSlug(dir), id+".jsonl")
}

// projectSlug is Claude Code's project-directory encoding: every character
// outside [A-Za-z0-9] becomes '-'. It copies resume.ProjectSlug because
// resume reaches internal/launch, which no surface package may import (see
// backend's barrier test); TestProjectSlugMatchesResume keeps the two equal.
func projectSlug(dir string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, dir)
}

// envValue returns the last value of key in env, as exec does.
func envValue(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}
