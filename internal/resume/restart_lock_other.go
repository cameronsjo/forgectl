//go:build !unix

package resume

import "errors"

// lockRestart refuses off unix, where a restart cannot signal a session
// anyway (terminateProcess refuses too).
func lockRestart(string) (func(), error) {
	return nil, errors.New("restarting sessions is supported only on unix")
}
