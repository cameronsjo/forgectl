//go:build !unix

package config

import "errors"

// readMergeConfig refuses: the owner and mode checks need a Unix stat, so
// the merge policy resolves to off on this platform.
func readMergeConfig(string, MergeFileCheck) ([]byte, error) {
	return nil, errors.New("the merge policy's file checks need a Unix platform")
}

// LocalMergeFileCheck is the check for this process's user; on this
// platform readMergeConfig refuses whatever it says.
func LocalMergeFileCheck() MergeFileCheck { return MergeFileCheck{UID: -1} }
