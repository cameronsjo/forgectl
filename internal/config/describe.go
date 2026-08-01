package config

import (
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// Report carries the provenance of one config-file read: where the file was,
// whether it existed, whether it parsed, which keys the decoder bound, and
// which keys in the file bound to nothing.
//
// It exists because Config alone cannot answer the question `forgectl config`
// is opened to ask — "did my edit take effect?" Every section documents its
// zero value as "section absent, the owning package's built-in defaults
// apply", so a rendered value of "" is indistinguishable across four very
// different situations: the key was never set; the key is misspelled
// (probe_hostt); the key sits under the wrong section; or the key's value
// failed to decode and Load tolerated it with a warning. Report separates
// them.
//
// The BurntSushi toml.MetaData backing IsSet stays unexported deliberately:
// this package hides the decoder from its public surface everywhere else, and
// leaking it into a signature would be the first exception.
type Report struct {
	// Path is the resolved config file location; "" when PathErr is non-nil.
	Path string
	// PathErr is set when the config path itself could not be resolved (no
	// user config dir). Distinct from "the file is absent", which is the
	// normal defaults-only posture.
	PathErr error
	// Found reports whether a file exists at Path.
	Found bool
	// DecodeErr is the parse error from a file that exists but is malformed.
	// The Config returned alongside holds whatever decoded before the error —
	// the same partial value Load returns after logging its warning.
	DecodeErr error
	// Unrecognized lists dotted keys present in the file that bound to no
	// struct field: a misspelled key, or a key under the wrong section. It is
	// deliberately empty when DecodeErr is set — a decode aborts partway, so
	// every key after the failure point would be trivially "undecoded" and the
	// list would be noise rather than a signal.
	Unrecognized []string

	meta toml.MetaData
}

// IsSet reports whether a dotted key ("net.probe_host") was present in the
// config file AND bound to a struct field. It is the "set" half of the
// set/default marker: false means the value shown came from a built-in
// fallback, not from anything the file asked for.
func (r Report) IsSet(dotted string) bool {
	if dotted == "" {
		return false
	}
	return r.meta.IsDefined(strings.Split(dotted, ".")...)
}

// Describe reads the config file the same way Load does, but returns the
// provenance Load discards. Load's signature is deliberately untouched: every
// existing caller wants its tolerant "config or defaults, never an error"
// contract, and only the display path needs the rest.
//
// Describe decodes the file a second time (Load already decoded it at
// startup). That is fine for a display command, and keeping them separate
// avoids widening a hot path's return signature for one consumer.
func Describe() (Config, Report) {
	path, err := ConfigPath()
	if err != nil {
		return Config{}, Report{PathErr: err}
	}
	return describeFile(path)
}

// describeFile is Describe with the path supplied.
func describeFile(path string) (Config, Report) {
	rep := Report{Path: path}
	var cfg Config
	meta, err := toml.DecodeFile(path, &cfg)
	switch {
	case err == nil:
		rep.Found = true
	case os.IsNotExist(err):
		// Absent file is the normal defaults-only posture, not an error.
		return cfg, rep
	default:
		rep.Found = true
		rep.DecodeErr = err
	}
	rep.meta = meta
	if rep.DecodeErr == nil {
		for _, k := range meta.Undecoded() {
			rep.Unrecognized = append(rep.Unrecognized, k.String())
		}
	}
	return cfg, rep
}
