//go:build !unix

package config

import (
	"fmt"
	"io"
	"os"

	"github.com/BurntSushi/toml"
)

type nativeMigrationFS struct{}

func NativeMigrationFS() MigrationFS { return nativeMigrationFS{} }

func (nativeMigrationFS) mutationSupported() bool { return false }

type otherLegacyProbe struct {
	path    string
	exists  bool
	regular bool
}

func (p *otherLegacyProbe) Exists() bool  { return p != nil && p.exists }
func (p *otherLegacyProbe) Regular() bool { return p != nil && p.regular }
func (p *otherLegacyProbe) Close() error  { return nil }
func (p *otherLegacyProbe) Capture() (*LegacySnapshot, error) {
	return nil, ErrLegacyMigrationUnsupported
}

func (nativeMigrationFS) probe(path string) (legacyProbe, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &otherLegacyProbe{path: path}, nil
		}
		return nil, fmt.Errorf("inspect legacy config entry: %w", err)
	}
	return &otherLegacyProbe{path: path, exists: true, regular: info.Mode().IsRegular()}, nil
}

func identityFromFileInfo(os.FileInfo) (FileIdentity, error) {
	return FileIdentity{}, ErrLegacyMigrationUnsupported
}

func (nativeMigrationFS) loadReadOnly(path string) (LaunchConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return LaunchConfig{}, ErrNoLegacyLaunch
		}
		return LaunchConfig{}, err
	}
	defer f.Close() //nolint:errcheck
	info, err := f.Stat()
	if err != nil {
		return LaunchConfig{}, err
	}
	if !info.Mode().IsRegular() {
		return LaunchConfig{}, ErrLegacyNonRegular
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return LaunchConfig{}, err
	}
	var launch LaunchConfig
	if _, err := toml.Decode(string(data), &launch); err != nil {
		return LaunchConfig{}, fmt.Errorf("%w: %v", ErrLegacyMalformed, err)
	}
	return launch, nil
}
