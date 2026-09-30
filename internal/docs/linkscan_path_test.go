package docs

import (
	"os"
	"path/filepath"
)

// scanDoc scans the file at absPath as a docs root. It and scanDocFor are
// test conveniences over scanDocFrom: production code never scans by path,
// only through a file walkRoot or indexFileRoot opened through the root's
// os.Root.
func scanDoc(absPath, relPath string) (docMeta, error) {
	return scanDocFor(RootDocs, absPath, relPath)
}

// scanDocFor scans the file at absPath as a root of the given kind.
func scanDocFor(kind RootKind, absPath, relPath string) (docMeta, error) {
	f, err := os.Open(filepath.Clean(absPath))
	if err != nil {
		return docMeta{}, err
	}
	defer func() { _ = f.Close() }()
	return scanDocFrom(kind, f, relPath)
}
