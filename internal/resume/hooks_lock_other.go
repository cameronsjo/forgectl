//go:build !unix

package resume

import (
	"context"
	"errors"
	"os"
)

// Lock refuses off unix: the watcher is a launchd agent, and the restart
// action it runs is unix-only too.
func (s FileHookStore) Lock(context.Context) (func(), error) {
	return nil, errors.New("update hooks are supported only on unix")
}

func openAppendNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- fixed name under forgectl's own state dir
}
