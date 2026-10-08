package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SurfaceConfig is the [surface] section: [surface.drain] and
// [surface.profiles]. A surface launch still names its backend on every call.
type SurfaceConfig struct {
	Drain SurfaceDrainConfig `toml:"drain"`
	// Profiles are the named Claude config directories a worker can run
	// under (`--profile <name>`), keyed by name.
	Profiles map[string]SurfaceProfile `toml:"profiles"`
}

// SurfaceProfile is one [surface.profiles.<name>] table.
type SurfaceProfile struct {
	// ConfigDir is the worker's CLAUDE_CONFIG_DIR: an absolute path, or one
	// starting with "~/", expanded to the home directory at use.
	ConfigDir string `toml:"config_dir"`
}

// MainProfile is the profile name that means no profile: the worker keeps
// the launcher's CLAUDE_CONFIG_DIR, as a launch with no --profile does. It
// cannot be defined in [surface.profiles].
const MainProfile = "main"

// maxProfileNameLen bounds a profile name; it is stored on queue rows.
const maxProfileNameLen = 32

var (
	// ErrUnknownProfile reports a profile name [surface.profiles] does not
	// define.
	ErrUnknownProfile = errors.New("no such [surface.profiles] entry")
	// ErrInvalidProfileName reports a profile name outside the allowed shape.
	ErrInvalidProfileName = errors.New("a profile name is 1-32 characters of a-z, 0-9, '-' and '_', starting with a letter or digit")
	// ErrInvalidModel reports a --model value that is not a plain token.
	ErrInvalidModel = errors.New("a model is 1-64 characters of letters, digits, '.', '-', '_', '[' and ']', not starting with '-'")
)

// CheckProfileName refuses a profile name outside the allowed shape.
func CheckProfileName(name string) error {
	if name == "" || len(name) > maxProfileNameLen {
		return ErrInvalidProfileName
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '_') && i > 0:
		default:
			return ErrInvalidProfileName
		}
	}
	return nil
}

// maxModelLen bounds a --model value.
const maxModelLen = 64

// CheckModelName refuses a model value that is not a plain token: it goes
// into a harness argv as the value of --model, so it may not start with '-'
// (where it would read as a flag) or carry anything but letters, digits, '.',
// '-', '_', '[' and ']' (the "[1m]" context suffix).
func CheckModelName(model string) error {
	if model == "" || len(model) > maxModelLen || model[0] == '-' {
		return ErrInvalidModel
	}
	for _, r := range model {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_', r == '[', r == ']':
		default:
			return ErrInvalidModel
		}
	}
	return nil
}

// ValidateProfiles checks every [surface.profiles] entry: its name, and a
// config_dir that is absolute or starts with "~/". A name of "main" is
// refused, since it means no profile.
func (c SurfaceConfig) ValidateProfiles() error {
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := CheckProfileName(name); err != nil {
			return fmt.Errorf("[surface.profiles.%s]: %w", quoteConfigValue(name), err)
		}
		if name == MainProfile {
			return fmt.Errorf("[surface.profiles.%s]: the name %q is reserved for the launcher's own CLAUDE_CONFIG_DIR and cannot be defined", name, MainProfile)
		}
		dir := c.Profiles[name].ConfigDir
		if dir != "~" && !strings.HasPrefix(dir, "~/") && !filepath.IsAbs(dir) {
			return fmt.Errorf("[surface.profiles.%s] config_dir: want an absolute path or one starting with \"~/\", got %s", name, quoteConfigValue(dir))
		}
	}
	return nil
}

// ProfileConfigDir resolves a profile name to its config directory, with a
// leading "~" expanded by home. An empty name and "main" resolve to "": the
// worker keeps the launcher's CLAUDE_CONFIG_DIR. A name [surface.profiles]
// does not define is ErrUnknownProfile.
func (c SurfaceConfig) ProfileConfigDir(name string, home func() (string, error)) (string, error) {
	if name == "" || name == MainProfile {
		return "", nil
	}
	p, ok := c.Profiles[name]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownProfile, quoteConfigValue(name))
	}
	dir := p.ConfigDir
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		h, err := home()
		if err != nil {
			return "", fmt.Errorf("[surface.profiles.%s] config_dir: home directory: %w", name, err)
		}
		dir = expandTilde(dir, h)
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("[surface.profiles.%s] config_dir: want an absolute path after expanding \"~\", got %s", name, quoteConfigValue(dir))
	}
	return filepath.Clean(dir), nil
}

// SurfaceDrainConfig is [surface.drain]: how `forgectl surface drain` paces
// and caps the workers it launches from the queue. Every field is optional;
// an absent one takes its default. A present one that is out of range is an
// error, never a default: the drain pauses claiming on it (Resolve).
type SurfaceDrainConfig struct {
	// Interval is the time between ticks, as a Go duration ("15s").
	Interval string `toml:"interval"`
	// Cap is how many workers may hold a slot at once, machine-wide.
	Cap *int `toml:"cap"`
	// PerRepo is how many of those may be in one repository.
	PerRepo *int `toml:"per_repo"`
	// Notify turns off the needs-you notification when set to false.
	Notify *bool `toml:"notify"`
	// IdleMinutes is how long a worker may sit at its prompt with no report
	// before its row goes to needs-you.
	IdleMinutes *int `toml:"idle_minutes"`
}

// Drain defaults and limits.
const (
	DefaultDrainInterval    = 15 * time.Second
	MinDrainInterval        = 5 * time.Second
	MaxDrainInterval        = time.Hour
	DefaultDrainCap         = 3
	MaxDrainCap             = 10
	DefaultDrainPerRepo     = 1
	DefaultDrainIdleMinutes = 10
	MaxDrainIdleMinutes     = 24 * 60
)

// DrainSettings is [surface.drain] resolved: every value present and in range.
type DrainSettings struct {
	Interval time.Duration
	Cap      int
	PerRepo  int
	Notify   bool
	Idle     time.Duration
}

// DefaultDrainSettings is what an absent [surface.drain] section resolves to.
func DefaultDrainSettings() DrainSettings {
	return DrainSettings{
		Interval: DefaultDrainInterval,
		Cap:      DefaultDrainCap,
		PerRepo:  DefaultDrainPerRepo,
		Notify:   true,
		Idle:     DefaultDrainIdleMinutes * time.Minute,
	}
}

// Resolve fills absent fields with their defaults and refuses a present one
// out of range, naming the key, the range, and the value seen.
func (c SurfaceDrainConfig) Resolve() (DrainSettings, error) {
	s := DefaultDrainSettings()
	if c.Interval != "" {
		d, err := time.ParseDuration(c.Interval)
		if err != nil {
			return DrainSettings{}, fmt.Errorf("[surface.drain] interval: want a duration such as \"15s\", got %s", quoteConfigValue(c.Interval))
		}
		if d < MinDrainInterval || d > MaxDrainInterval {
			return DrainSettings{}, fmt.Errorf("[surface.drain] interval: want %s to %s, got %s", MinDrainInterval, MaxDrainInterval, d)
		}
		s.Interval = d
	}
	if c.Cap != nil {
		if *c.Cap < 1 || *c.Cap > MaxDrainCap {
			return DrainSettings{}, fmt.Errorf("[surface.drain] cap: want 1 to %d, got %d", MaxDrainCap, *c.Cap)
		}
		s.Cap = *c.Cap
	}
	if c.PerRepo != nil {
		if *c.PerRepo < 1 || *c.PerRepo > MaxDrainCap {
			return DrainSettings{}, fmt.Errorf("[surface.drain] per_repo: want 1 to %d, got %d", MaxDrainCap, *c.PerRepo)
		}
		s.PerRepo = *c.PerRepo
	}
	if c.Notify != nil {
		s.Notify = *c.Notify
	}
	if c.IdleMinutes != nil {
		if *c.IdleMinutes < 1 || *c.IdleMinutes > MaxDrainIdleMinutes {
			return DrainSettings{}, fmt.Errorf("[surface.drain] idle_minutes: want 1 to %d, got %d", MaxDrainIdleMinutes, *c.IdleMinutes)
		}
		s.Idle = time.Duration(*c.IdleMinutes) * time.Minute
	}
	return s, nil
}

// Validate reports the first out-of-range [surface.drain] value.
func (c SurfaceDrainConfig) Validate() error {
	_, err := c.Resolve()
	return err
}
