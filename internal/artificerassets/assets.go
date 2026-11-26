// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

// Package artificerassets embeds the vendored Artificer stylesheet and its
// woff2 web fonts with zero external dependencies, so local HTTP surfaces such
// as `forgectl desk serve` can share the Artificer look without linking
// internal/docs (and its chroma/mermaid/katex/fsnotify dependency graph).
package artificerassets

import (
	"embed"
	"io/fs"
	"strings"
)

//go:embed artificer.css
var CSS []byte

//go:embed assets/fonts/*.woff2
var Fonts embed.FS

// Assets returns the vendored Artificer stylesheet and a lookup for its woff2
// fonts by file name. font returns false for any name that is not one of the
// embedded woff2 files.
func Assets() (css []byte, font func(name string) ([]byte, bool)) {
	return CSS, func(name string) ([]byte, bool) {
		if !strings.HasSuffix(name, ".woff2") || strings.ContainsAny(name, "/\\") {
			return nil, false
		}
		b, err := fs.ReadFile(Fonts, "assets/fonts/"+name)
		if err != nil {
			return nil, false
		}
		return b, true
	}
}
