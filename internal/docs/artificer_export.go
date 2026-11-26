package docs

import "github.com/cameronsjo/forgectl/internal/artificerassets"

// ArtificerAssets returns the vendored Artificer stylesheet and a lookup for
// its woff2 fonts by file name, delegating to internal/artificerassets so
// non-docs callers (such as `forgectl desk serve`) do not need to link
// internal/docs.
func ArtificerAssets() (css []byte, font func(name string) ([]byte, bool)) {
	return artificerassets.Assets()
}
