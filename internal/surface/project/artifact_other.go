//go:build !unix

package project

import (
	"context"

	"github.com/cameronsjo/forgectl/internal/privdir"
)

func verifyArtifact(context.Context, string, string, string) (ArtifactDigest, error) {
	return ArtifactDigest{}, privdir.ErrUnsupported
}
