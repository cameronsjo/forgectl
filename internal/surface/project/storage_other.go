//go:build !unix

package project

import (
	"context"

	"github.com/cameronsjo/forgectl/internal/privdir"
)

func snapshotStore(context.Context, string) (Registry, error) {
	return Registry{}, privdir.ErrUnsupported
}

func updateRegistry(context.Context, string, bool, func(*Registry, int) error) (Registry, error) {
	return Registry{}, privdir.ErrUnsupported
}

func inspectStore(context.Context, string, string) (Registry, Project, []Observation, error) {
	return Registry{}, Project{}, nil, privdir.ErrUnsupported
}
