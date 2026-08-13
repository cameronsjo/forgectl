//go:build !unix

package cli

func normalWriterAllowsUnsupportedLegacyMutation() bool { return true }
