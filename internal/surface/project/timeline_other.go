//go:build !unix

package project

import (
	"context"

	"github.com/cameronsjo/forgectl/internal/privdir"
)

func (store Store) MailTimeline(context.Context, string) (MailTimeline, error) {
	return MailTimeline{}, privdir.ErrUnsupported
}
