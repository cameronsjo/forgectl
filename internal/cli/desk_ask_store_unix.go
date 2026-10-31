//go:build unix

// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"context"

	"github.com/cameronsjo/forgectl/internal/desk/ask"
)

// hcNotifier tells an asking agent about a response with `hc send`. hc is
// looked up at the first notify, not when the store opens, so a desk with no
// hc on PATH still records responses; the notify then fails and is logged.
type hcNotifier struct{}

func (hcNotifier) Notify(ctx context.Context, addr, text string) error {
	a, err := newHcClient()
	if err != nil {
		return err
	}
	return a.SendLine(ctx, addr, text)
}

// deskAskNotifier is the notifier every front end's store uses. Tests swap it
// for a stub.
var deskAskNotifier ask.Notifier = hcNotifier{}

// openAskStore resolves the desk directory the way every desk verb does and
// opens its asks store for writing, with the shared notifier and clock.
func openAskStore(dirFlag string) (*ask.Store, string, error) {
	dir, err := resolveDeskDir(dirFlag)
	if err != nil {
		return nil, "", err
	}
	s, err := ask.Open(dir, ask.WithNotifier(deskAskNotifier), ask.WithClock(deskNow))
	if err != nil {
		return nil, dir, err
	}
	return s, dir, nil
}
