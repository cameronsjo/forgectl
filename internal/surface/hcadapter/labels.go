// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package hcadapter

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Canonical agent identity label keys stamped on herdr-core (hc) terminals at
// launch and read by consumers such as `forgectl attention`.
//
// Labels are not authenticated: any hc client can write one. Consumers must
// validate every value's shape on read and treat labels as corroboration and
// display metadata, never as proof of authorization.
const (
	LabelAgentKind    = "agent.kind"
	LabelAgentSession = "agent.session"
	LabelAgentWorker  = "agent.worker"
	LabelAgentRole    = "agent.role"
)

// ErrBadAgentIdentity reports an agent.* label value forgectl will not pass to hc.
var ErrBadAgentIdentity = errors.New("hc: invalid agent identity label")

// maxAgentLabelLen bounds agent.session, agent.worker, and agent.role values.
const maxAgentLabelLen = 64

// AgentIdentity holds the canonical agent.* labels stamped on an hc terminal.
// Empty fields are omitted when stamping.
type AgentIdentity struct {
	// Kind is the harness kind: "claude", "codex", or "pi".
	Kind string
	// Session is the agent session id, when known at launch.
	Session string
	// Worker is the worker name, when launched as a worker.
	Worker string
	// Role is the agent role, when specified.
	Role string
}

// ValidAgentKind reports whether s is one of the canonical agent.kind values.
func ValidAgentKind(s string) bool {
	switch s {
	case "claude", "codex", "pi":
		return true
	}
	return false
}

// ValidAgentSession reports whether s has the shape of an agent.session id:
// 1-64 letters, digits, '_' or '-', starting with a letter or digit.
func ValidAgentSession(s string) bool {
	return validAgentToken(s, false)
}

// ValidAgentWorker reports whether s has the shape of an agent.worker name:
// 1-64 letters, digits, '.', '_' or '-', starting with a letter or digit.
func ValidAgentWorker(s string) bool {
	return validAgentToken(s, true)
}

// ValidAgentRole reports whether s has the shape of an agent.role value:
// 1-64 letters, digits, '.', '_' or '-', starting with a letter or digit.
func ValidAgentRole(s string) bool {
	return validAgentToken(s, true)
}

func validAgentToken(s string, allowDot bool) bool {
	if s == "" || len(s) > maxAgentLabelLen {
		return false
	}
	if !isAlnum(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if isAlnum(c) || c == '-' || c == '_' || (allowDot && c == '.') {
			continue
		}
		return false
	}
	return true
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// Validate checks every non-empty field of id before it can reach hc's argv.
func (id AgentIdentity) Validate() error {
	if id.Kind != "" && !ValidAgentKind(id.Kind) {
		return fmt.Errorf("%w: %s must be claude, codex, or pi", ErrBadAgentIdentity, LabelAgentKind)
	}
	if id.Session != "" && !ValidAgentSession(id.Session) {
		return fmt.Errorf("%w: %s must be 1-%d letters, digits, '_' or '-'", ErrBadAgentIdentity, LabelAgentSession, maxAgentLabelLen)
	}
	if id.Worker != "" && !ValidAgentWorker(id.Worker) {
		return fmt.Errorf("%w: %s must be 1-%d letters, digits, '.', '_' or '-'", ErrBadAgentIdentity, LabelAgentWorker, maxAgentLabelLen)
	}
	if id.Role != "" && !ValidAgentRole(id.Role) {
		return fmt.Errorf("%w: %s must be 1-%d letters, digits, '.', '_' or '-'", ErrBadAgentIdentity, LabelAgentRole, maxAgentLabelLen)
	}
	return nil
}

// pairs returns the non-empty label key=value pairs in deterministic order.
func (id AgentIdentity) pairs() []string {
	var out []string
	if id.Kind != "" {
		out = append(out, LabelAgentKind+"="+id.Kind)
	}
	if id.Session != "" {
		out = append(out, LabelAgentSession+"="+id.Session)
	}
	if id.Worker != "" {
		out = append(out, LabelAgentWorker+"="+id.Worker)
	}
	if id.Role != "" {
		out = append(out, LabelAgentRole+"="+id.Role)
	}
	return out
}

// ParseAgentLabels extracts and validates the canonical agent.* labels from an
// hc terminal's label map. Invalid or unrecognized values are dropped so a
// malformed label on one terminal never breaks a caller.
func ParseAgentLabels(labels map[string]string) AgentIdentity {
	if len(labels) == 0 {
		return AgentIdentity{}
	}
	var out AgentIdentity
	if v := labels[LabelAgentKind]; ValidAgentKind(v) {
		out.Kind = v
	}
	if v := labels[LabelAgentSession]; ValidAgentSession(v) {
		out.Session = v
	}
	if v := labels[LabelAgentWorker]; ValidAgentWorker(v) {
		out.Worker = v
	}
	if v := labels[LabelAgentRole]; ValidAgentRole(v) {
		out.Role = v
	}
	return out
}

// SocketCandidates returns the candidate Unix socket paths for the hc core
// selected by getenv, without running hc. It returns nil if HC_SESSION is
// invalid.
func SocketCandidates(getenv func(string) string) []string {
	if sock := strings.TrimSpace(getenv("HC_SOCKET")); sock != "" {
		return []string{sock}
	}
	session := defaultSession
	if s := getenv("HC_SESSION"); s != "" && s != defaultSession {
		if !validSession(s) {
			return nil
		}
		session = s
	}
	name := session + ".sock"
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		for _, prev := range out {
			if prev == p {
				return
			}
		}
		out = append(out, p)
	}
	if sock := strings.TrimSpace(getenv("HC_TERMINAL_SOCKET")); sock != "" {
		add(sock)
	}
	if dir := getenv("XDG_RUNTIME_DIR"); dir != "" {
		add(filepath.Join(dir, "herdr-core", name))
	}
	if dir := getenv("XDG_STATE_HOME"); dir != "" {
		add(filepath.Join(dir, "herdr-core", name))
	}
	if home := getenv("HOME"); home != "" {
		add(filepath.Join(home, ".local", "state", "herdr-core", name))
		add(filepath.Join(home, "Library", "Application Support", "herdr-core", name))
		add(filepath.Join(home, "Library", "Caches", "herdr-core", name))
	}
	return out
}

// CoreSocketExists reports whether a Unix socket exists at any candidate path
// for the hc core selected by getenv. Read-only consumers such as `forgectl
// attention` check this before running `hc ls` so they do not auto-start a
// stray hc server when no core is running.
func CoreSocketExists(getenv func(string) string) bool {
	for _, p := range SocketCandidates(getenv) {
		if fi, err := os.Stat(p); err == nil && fi.Mode().Type() == fs.ModeSocket {
			return true
		}
	}
	return false
}
