package config

import (
	"errors"
	"regexp"
)

// AttentionConfig is the [attention] section: which sessions are a chief of
// staff's workers. A zero value means the default.
type AttentionConfig struct {
	// WorkerPrefix starts a worker session's name: default "cos-".
	WorkerPrefix *string `toml:"worker_prefix"`
}

const attentionDefaultPrefix = "cos-"

var attentionPrefixRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,31}$`)

// Validate refuses a worker_prefix that is not 1 to 32 letters, digits, '.',
// '_' or '-' starting with a letter or digit. The error names the key.
func (c AttentionConfig) Validate() error {
	if c.WorkerPrefix != nil && !attentionPrefixRe.MatchString(*c.WorkerPrefix) {
		return errors.New("attention.worker_prefix must be 1 to 32 letters, digits, '.', '_' or '-', starting with a letter or digit")
	}
	return nil
}

// Prefix is worker_prefix, or the default "cos-".
func (c AttentionConfig) Prefix() string {
	if c.WorkerPrefix == nil {
		return attentionDefaultPrefix
	}
	return *c.WorkerPrefix
}
