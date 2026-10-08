package gitleaks

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// This file is the package's whole filesystem surface, and the only place
// TestGitleaksSource_FilesystemOnlyInTemp allows an os call: the private
// work dir, its config, and the report gitleaks writes there.

const (
	configName = "cfg.toml"
	reportName = "report.json"
)

// newWorkDir makes the private temp dir a pass runs in (os.MkdirTemp makes
// it 0700) and writes forgectl's config into it 0600. The cleanup removes
// the dir and everything gitleaks wrote there.
func newWorkDir() (string, func(), error) {
	dir, err := os.MkdirTemp("", "forgectl-gitleaks-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := os.WriteFile(filepath.Join(dir, configName), []byte(configTOML), 0o600); err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}

// removeReport deletes the previous repo's report so a scan that writes
// none is never read as having written the last one. gitleaks itself
// creates the report (it test-creates and removes the path first, so a
// pre-made 0600 file would not survive); the 0700 dir is what keeps it
// private.
func removeReport(dir string) {
	_ = os.Remove(filepath.Join(dir, reportName))
}

// readReport decodes the report in dir, at most budget findings and at most
// maxReportBytes of it.
func readReport(dir string, budget int) ([]wireFinding, bool, error) {
	f, err := os.Open(filepath.Clean(filepath.Join(dir, reportName)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, errReportMissing
	}
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	return decodeReport(f, budget)
}
