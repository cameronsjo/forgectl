package config

import (
	"fmt"
	"time"
)

// SurfaceConfig is the [surface] section. Only [surface.drain] exists so far;
// a surface launch still names its backend on every call.
type SurfaceConfig struct {
	Drain SurfaceDrainConfig `toml:"drain"`
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
