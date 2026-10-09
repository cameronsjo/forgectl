package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SurfaceConfig is the [surface] section: [surface.drain], [surface.intake],
// [surface.merge] and [surface.profiles]. A surface launch still names its backend on every
// call.
type SurfaceConfig struct {
	Drain SurfaceDrainConfig `toml:"drain"`
	// Intake is how `surface intake gh` picks GitHub issues to queue.
	Intake SurfaceIntakeConfig `toml:"intake"`
	// Merge is the worker PR merge policy (ADR-0011).
	Merge SurfaceMergeConfig `toml:"merge"`
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
// config_dir that is absolute or starts with "~/", and is neither the home
// directory nor the root. A name of "main" is refused, since it means no
// profile.
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
		if wholeTree(dir) {
			return fmt.Errorf("[surface.profiles.%s] config_dir: want a directory of its own, not the home directory or the root, got %s", name, quoteConfigValue(dir))
		}
		if !strings.HasPrefix(dir, "~/") && !filepath.IsAbs(dir) {
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
	if strings.HasPrefix(dir, "~/") {
		h, err := home()
		if err != nil {
			return "", fmt.Errorf("[surface.profiles.%s] config_dir: home directory: %w", name, err)
		}
		dir = expandTilde(dir, h)
		if filepath.Clean(dir) == filepath.Clean(h) {
			return "", fmt.Errorf("[surface.profiles.%s] config_dir: want a directory of its own, not the home directory, got %s", name, quoteConfigValue(p.ConfigDir))
		}
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("[surface.profiles.%s] config_dir: want an absolute path after expanding \"~\", got %s", name, quoteConfigValue(dir))
	}
	if filepath.Clean(dir) == string(filepath.Separator) {
		return "", fmt.Errorf("[surface.profiles.%s] config_dir: want a directory of its own, not the root, got %s", name, quoteConfigValue(p.ConfigDir))
	}
	return filepath.Clean(dir), nil
}

// wholeTree reports a config_dir that is, as written, the home directory
// ("~", "~/", "~/.") or the filesystem root: a worker's CLAUDE_CONFIG_DIR
// there would write Claude Code's state straight into it. ProfileConfigDir
// checks the expanded path again.
func wholeTree(dir string) bool {
	if dir == "~" || (strings.HasPrefix(dir, "~/") && filepath.Clean("/"+dir[2:]) == "/") {
		return true
	}
	return filepath.IsAbs(dir) && filepath.Clean(dir) == string(filepath.Separator)
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

// SurfaceIntakeConfig is [surface.intake]: which GitHub issues `forgectl
// surface intake gh` may turn into queue rows. Every field is optional; an
// absent one takes its default. A present one that is out of shape is an
// error, refused when the file loads and again by intake itself; it never
// falls back to a default, because a default here widens who can write a
// worker's brief.
type SurfaceIntakeConfig struct {
	// Authors are the GitHub logins whose issues, labeled by one of them,
	// intake takes. Empty means the repository owner's login for a
	// user-owned repository; intake refuses an organization-owned one.
	Authors []string `toml:"authors"`
	// Labels are the eligible label names. Absent means DefaultIntakeLabels.
	Labels []string `toml:"labels"`
	// MaxPerRun caps the rows one intake run adds.
	MaxPerRun *int `toml:"max_per_run"`
	// unknown lists the undecoded keys under [surface.intake], set by
	// DecodeStrict. A misspelled key (lables) would otherwise leave its
	// field absent and select the default, so Resolve refuses any.
	unknown []string
}

// Intake defaults and limits.
const (
	DefaultIntakeMaxPerRun = 5
	MaxIntakeMaxPerRun     = 50
	// maxIntakeLabelLen is GitHub's own limit on a label name.
	maxIntakeLabelLen = 50
	// maxIntakeLoginLen is GitHub's own limit on a login.
	maxIntakeLoginLen = 39
	maxIntakeListLen  = 32
)

// DefaultIntakeLabels are the eligible labels when [surface.intake] labels
// is absent. queue:drain is a label no triage sweep applies, so the labeler
// check means a person put it there; a bulk-applied label (the exec:* triage
// taxonomy) would only prove a script ran as the owner.
var DefaultIntakeLabels = []string{"queue:drain"}

// IntakeSettings is [surface.intake] resolved. Authors stays empty when none
// are configured: the default depends on the repository's owner, which only
// intake knows.
type IntakeSettings struct {
	Authors   []string
	Labels    []string
	MaxPerRun int
}

// CheckGitHubLogin refuses a value that is not a GitHub user login: 1-39
// characters of letters, digits, '-' and '_' (an Enterprise Managed User's
// login ends in _<shortcode>), not starting or ending with '-'. A bot's
// "[bot]" suffix is outside the charset, so no bot login passes.
func CheckGitHubLogin(login string) error {
	if login == "" || len(login) > maxIntakeLoginLen || login[0] == '-' || login[len(login)-1] == '-' {
		return errors.New("want a GitHub login: 1-39 characters of letters, digits, '-' and '_', not starting or ending with '-'")
	}
	for _, r := range login {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return errors.New("want a GitHub login: 1-39 characters of letters, digits, '-' and '_', not starting or ending with '-'")
		}
	}
	return nil
}

// checkIntakeLabel refuses a label name intake will not pass to GitHub: empty,
// longer than GitHub allows, padded with spaces, or holding anything but
// printable ASCII.
func checkIntakeLabel(label string) error {
	if label == "" || len(label) > maxIntakeLabelLen || strings.TrimSpace(label) != label {
		return fmt.Errorf("want a label name of 1-%d characters with no leading or trailing space", maxIntakeLabelLen)
	}
	for _, r := range label {
		if r < ' ' || r > '~' {
			return errors.New("want a label name of printable ASCII characters")
		}
	}
	return nil
}

// Resolve fills absent fields with their defaults and refuses a present one
// out of shape, naming the key and the value seen.
func (c SurfaceIntakeConfig) Resolve() (IntakeSettings, error) {
	s := IntakeSettings{Labels: DefaultIntakeLabels, MaxPerRun: DefaultIntakeMaxPerRun}
	if len(c.unknown) > 0 {
		return IntakeSettings{}, fmt.Errorf("[surface.intake]: unknown key %s; the keys are authors, labels and max_per_run", quoteConfigValue(c.unknown[0]))
	}
	if len(c.Authors) > maxIntakeListLen {
		return IntakeSettings{}, fmt.Errorf("[surface.intake] authors: at most %d entries, got %d", maxIntakeListLen, len(c.Authors))
	}
	for _, a := range c.Authors {
		if err := CheckGitHubLogin(a); err != nil {
			return IntakeSettings{}, fmt.Errorf("[surface.intake] authors: %w, got %s", err, quoteConfigValue(a))
		}
	}
	s.Authors = append([]string(nil), c.Authors...)
	if c.Labels != nil {
		if len(c.Labels) == 0 || len(c.Labels) > maxIntakeListLen {
			return IntakeSettings{}, fmt.Errorf("[surface.intake] labels: want 1 to %d label names, got %d", maxIntakeListLen, len(c.Labels))
		}
		for _, l := range c.Labels {
			if err := checkIntakeLabel(l); err != nil {
				return IntakeSettings{}, fmt.Errorf("[surface.intake] labels: %w, got %s", err, quoteConfigValue(l))
			}
		}
		s.Labels = append([]string(nil), c.Labels...)
	}
	if c.MaxPerRun != nil {
		if *c.MaxPerRun < 1 || *c.MaxPerRun > MaxIntakeMaxPerRun {
			return IntakeSettings{}, fmt.Errorf("[surface.intake] max_per_run: want 1 to %d, got %d", MaxIntakeMaxPerRun, *c.MaxPerRun)
		}
		s.MaxPerRun = *c.MaxPerRun
	}
	return s, nil
}

// Validate reports the first out-of-shape [surface.intake] value.
func (c SurfaceIntakeConfig) Validate() error {
	_, err := c.Resolve()
	return err
}
