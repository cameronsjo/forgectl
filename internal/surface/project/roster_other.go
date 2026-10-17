//go:build !unix

package project

import (
	"github.com/cameronsjo/forgectl/internal/privdir"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
)

type rosterSnapshot struct {
	present bool
	byName  map[string]mail.Worker
}

func readRoster(int, string) (rosterSnapshot, error) {
	return rosterSnapshot{}, privdir.ErrUnsupported
}
