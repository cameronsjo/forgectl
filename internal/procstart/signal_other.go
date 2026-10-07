//go:build !unix

package procstart

// signalZero has no portable form here; nothing is ever signalled.
func signalZero(int) bool { return false }
