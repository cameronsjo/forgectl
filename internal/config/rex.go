// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package config

// RexConfig is the [surface.rex] section: where the Rex terminal multiplexer's `rex`
// CLI lives when it is not on PATH. A zero value means "look on PATH, then in
// the installed Rex app bundle".
type RexConfig struct {
	// BinaryPath is the absolute path of the `rex` binary. It outranks PATH and
	// the app bundle; a value that is not an executable file is refused, never
	// replaced by another binary.
	BinaryPath string `toml:"binary_path"`
}
