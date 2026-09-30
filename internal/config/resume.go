package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

// ResumeConfig is the [resume] section. Only [[resume.on_update]] exists so
// far: the hooks `forgectl resume hooks run` fires when a harness's installed
// version changes.
type ResumeConfig struct {
	OnUpdate []OnUpdateHook `toml:"on_update"`
}

// OnUpdateHook is one [[resume.on_update]] block. Exactly one of Action and
// Command is set. Command is an argv array, never a shell string: it runs
// without a shell, and the versions reach it through its environment, never
// through its arguments.
type OnUpdateHook struct {
	Harness string   `toml:"harness"`
	Action  string   `toml:"action"`
	Command []string `toml:"command"`
	// TimeoutSeconds bounds one run of a command hook (its process group is
	// killed at the deadline). For action = "restart" it bounds only the
	// waiting for sessions to go idle: a restart already signalled is still
	// relaunched and confirmed after it passes. 0 takes the built-in default
	// for the kind. Named in seconds like [net] ttl_seconds.
	TimeoutSeconds int `toml:"timeout_seconds"`
}

// OnUpdateActionRestart is the one built-in action: `forgectl resume restart
// --outdated` for the harness that updated.
const OnUpdateActionRestart = "restart"

// onUpdateMaxTimeoutSeconds caps timeout_seconds at a day. A larger value is
// far more likely a unit mistake (milliseconds) than a hook that should run
// for a week, and the cap keeps the value well inside time.Duration.
const onUpdateMaxTimeoutSeconds = 24 * 60 * 60

// Validate reports the first invalid [[resume.on_update]] entry, or an
// unknown key anywhere under [resume]. unknown is the decoder's undecoded key
// list for the section (Config.resumeUnknown); a misspelled key such as
// `comand` would otherwise decode to a hook with nothing to run, or silently
// drop a timeout.
func (rc ResumeConfig) Validate(unknown []string) error {
	for _, k := range unknown {
		if k == "on_update" || strings.HasPrefix(k, "on_update.") {
			return errors.New("a top-level [[on_update]] table is not read: update hooks live under [[resume.on_update]]; rename the table header")
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("[resume]: unknown key %s", quoteConfigValue(unknown[0]))
	}
	for i, h := range rc.OnUpdate {
		if err := h.validate(); err != nil {
			return fmt.Errorf("[[resume.on_update]] #%d: %w", i+1, err)
		}
	}
	return nil
}

func (h OnUpdateHook) validate() error {
	switch h.Harness {
	case "claude":
	case "codex", "pi":
		return fmt.Errorf("harness %q is not supported yet: only \"claude\" has update detection", h.Harness)
	case "":
		return errors.New("harness is missing: set harness = \"claude\"")
	default:
		return fmt.Errorf("harness %s is unknown: only \"claude\" is supported", quoteConfigValue(h.Harness))
	}
	hasAction, hasCommand := h.Action != "", h.Command != nil
	switch {
	case hasAction && hasCommand:
		return errors.New("set exactly one of action and command, not both")
	case !hasAction && !hasCommand:
		return errors.New("set exactly one of action = \"restart\" or command = [\"argv\", ...]")
	case hasAction && h.Action != OnUpdateActionRestart:
		return fmt.Errorf("action %s is unknown: the only action is \"restart\"", quoteConfigValue(h.Action))
	}
	if hasCommand {
		if len(h.Command) == 0 || strings.TrimSpace(h.Command[0]) == "" {
			return errors.New("command is empty: name the program as its first element")
		}
		for j, arg := range h.Command {
			// A NUL cannot reach an exec argv at all, and any other control
			// character in a config argv is a paste accident, not intent.
			if strings.IndexFunc(arg, unicode.IsControl) >= 0 {
				return fmt.Errorf("command element %d holds a control character", j+1)
			}
		}
	}
	if h.TimeoutSeconds < 0 || h.TimeoutSeconds > onUpdateMaxTimeoutSeconds {
		return fmt.Errorf("timeout_seconds %d is out of range: use 0 (the default) up to %d", h.TimeoutSeconds, onUpdateMaxTimeoutSeconds)
	}
	return nil
}

// ResumeHooks returns the [[resume.on_update]] entries once the section
// validates, so a caller can never act on a half-valid list.
func (c Config) ResumeHooks() ([]OnUpdateHook, error) {
	if err := c.Resume.Validate(c.resumeUnknown); err != nil {
		return nil, err
	}
	return c.Resume.OnUpdate, nil
}

// ResumeHooksDir is forgectl's state directory for update hooks: the last
// version seen per harness, the run audit trail, and the watcher's log. It
// sits beside the resume snapshot store, never inside a harness's own files.
func ResumeHooksDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "resume-hooks"), nil
}
