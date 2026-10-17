//go:build !unix

package project

import (
	"context"

	"github.com/cameronsjo/forgectl/internal/privdir"
)

func snapshotStore(context.Context, string) (Registry, error) {
	return Registry{}, privdir.ErrUnsupported
}

func updateRegistry(context.Context, string, bool, func(*Registry) error) (Registry, error) {
	return Registry{}, privdir.ErrUnsupported
}
