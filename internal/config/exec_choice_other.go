//go:build !unix

package config

// CheckExecChoiceFile passes: Rex is macOS-only, and the owner and mode checks
// need a Unix stat.
func CheckExecChoiceFile(string) error { return nil }
