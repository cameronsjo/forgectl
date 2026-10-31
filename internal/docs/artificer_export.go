package docs

import (
	"io/fs"
	"strings"
)

// ArtificerAssets returns the vendored Artificer stylesheet and a lookup for
// its woff2 fonts by file name, so another local page (`forgectl desk serve`)
// can wear the same look without a second vendored copy. font returns false
// for any name that is not one of the embedded woff2 files.
func ArtificerAssets() (css []byte, font func(name string) ([]byte, bool)) {
	return artificerCSS, func(name string) ([]byte, bool) {
		if !strings.HasSuffix(name, ".woff2") || strings.ContainsAny(name, "/\\") {
			return nil, false
		}
		b, err := fs.ReadFile(artificerFonts, "assets/artificer/assets/fonts/"+name)
		if err != nil {
			return nil, false
		}
		return b, true
	}
}
