package config

import (
	"fmt"
	"regexp"
	"time"
)

// AttentionConfig is the [attention] section: how long `forgectl attention`
// keeps a record no herdr pane vouches for, and which sessions are a chief
// of staff's workers. A zero value means both defaults.
type AttentionConfig struct {
	// TTLHours is how long a record whose pane the caller's herdr server
	// cannot see is kept: 1 to 720, default 24.
	TTLHours *int `toml:"ttl_hours"`
	// WorkerPrefix starts a worker session's name: default "cos-".
	WorkerPrefix *string `toml:"worker_prefix"`
}

const (
	attentionDefaultTTLHours = 24
	attentionMaxTTLHours     = 720
	attentionDefaultPrefix   = "cos-"
)

var attentionPrefixRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,31}$`)

// Validate refuses a ttl_hours outside 1..720 or a worker_prefix that is not
// 1 to 32 letters, digits, '.', '_' or '-' starting with a letter or digit.
// The error names the key.
func (c AttentionConfig) Validate() error {
	if c.TTLHours != nil && (*c.TTLHours < 1 || *c.TTLHours > attentionMaxTTLHours) {
		return fmt.Errorf("attention.ttl_hours must be 1 to %d, got %d", attentionMaxTTLHours, *c.TTLHours)
	}
	if c.WorkerPrefix != nil && !attentionPrefixRe.MatchString(*c.WorkerPrefix) {
		return fmt.Errorf("attention.worker_prefix must be 1 to 32 letters, digits, '.', '_' or '-', starting with a letter or digit")
	}
	return nil
}

// TTL is ttl_hours as a duration, or the default 24h.
func (c AttentionConfig) TTL() time.Duration {
	if c.TTLHours == nil {
		return attentionDefaultTTLHours * time.Hour
	}
	return time.Duration(*c.TTLHours) * time.Hour
}

// Prefix is worker_prefix, or the default "cos-".
func (c AttentionConfig) Prefix() string {
	if c.WorkerPrefix == nil {
		return attentionDefaultPrefix
	}
	return *c.WorkerPrefix
}
