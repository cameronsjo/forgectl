//go:build !unix

package pr

import (
	"context"
	"errors"
)

const lifecycleLockName = ".pr-session-lifecycle.lock"

var errLifecycleLockUnsupported = errors.New(
	"the pr session lifecycle lock has no implementation on this platform; pr session verbs require a unix build")

// withLifecycleLock REFUSES off Unix rather than passing through. The config
// and env locks fail open here because they serialize a statistics or config
// write; this one bounds an admission cap, and a cap with no cross-process
// exclusion is not a cap. No shipped binary runs here (goreleaser builds
// linux and darwin only).
func (c *Client) withLifecycleLock(_ context.Context, _ string, _ func() error) error {
	return errLifecycleLockUnsupported
}
