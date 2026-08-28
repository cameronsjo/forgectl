package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode"

	"github.com/BurntSushi/toml"
)

// EnvSnapshot is the process environment used to resolve one legacy
// migration attempt. Callers capture it once and carry the resulting boundary
// through the whole command; migration code never consults the live
// environment again.
type EnvSnapshot struct {
	Home           string
	XDGConfigHome  string
	UserConfigHome string
}

var (
	ErrLegacyPathPolicy           = errors.New("legacy migration path policy refused")
	ErrLegacyPathControl          = errors.New("legacy migration path contains a control character")
	ErrLegacyNonRegular           = errors.New("legacy migration source is not a regular file")
	ErrLegacyMalformed            = errors.New("legacy migration source is malformed")
	ErrLegacyUnsupportedFields    = errors.New("legacy migration source carries fields forgectl cannot represent")
	ErrLegacyDrift                = errors.New("legacy migration source changed during the attempt")
	ErrLegacySourceMissing        = errors.New("legacy migration source was retired by another process")
	ErrLegacyMigrationUnsupported = errors.New("legacy migration is unsupported on this platform")
	ErrBackupDrift                = errors.New("legacy backup changed after allocation")
	ErrBackupIdentityLost         = errors.New("legacy backup name no longer identifies this attempt's allocation")
)

// BoundaryStatus is the authoritative result of preparing one process
// attempt. A refusal is data, not a process-fatal error: automatic launch can
// still choose its documented read-only fallback or shadow behavior.
type BoundaryStatus uint8

const (
	BoundaryNoSource BoundaryStatus = iota
	BoundaryMigratable
	BoundaryRefused
)

// FileIdentity is stable across renames and hardlinks on Unix. It is carried
// from source capture through backup validation and retirement.
type FileIdentity struct {
	Device uint64
	Inode  uint64
}

type stableFileMetadata struct {
	Identity FileIdentity
	Mode     os.FileMode
	Size     int64
	ModNanos int64
}

func metadataFromInfo(info os.FileInfo) (stableFileMetadata, error) {
	id, err := identityFromFileInfo(info)
	if err != nil {
		return stableFileMetadata{}, err
	}
	return stableFileMetadata{
		Identity: id,
		Mode:     info.Mode(),
		Size:     info.Size(),
		ModNanos: info.ModTime().UnixNano(),
	}, nil
}

func sameStableMetadata(a, b stableFileMetadata) bool {
	return a == b
}

// LegacySnapshot owns the pinned source and legacy-parent handles. Data is
// the one immutable payload used for both TOML decode and backup bytes;
// comparison reads performed later never replace it.
type LegacySnapshot struct {
	Data   []byte
	Launch LaunchConfig
	// UndecodedKeys names every key in Data that LaunchConfig has no field
	// for, as sorted dotted paths. A non-empty list means forgectl decoded
	// only part of the file: rendering [launch] from Launch alone would drop
	// the rest, so the migration transaction refuses to render, back up, or
	// retire the source (#417). The read-only launch path stays lenient and
	// ignores this.
	UndecodedKeys []string
	Identity      FileIdentity
	Mode          os.FileMode
	platform      legacySnapshotPlatform
}

// UnsupportedFieldsError wraps ErrLegacyUnsupportedFields with the offending
// key names, following the precedent in internal/workflow/parse.go. Callers
// render the names through termsafe: they are attacker-influenceable content
// read out of a config file.
func UnsupportedFieldsError(keys []string) error {
	return fmt.Errorf("%w: %s", ErrLegacyUnsupportedFields, strings.Join(keys, ", "))
}

// decodeLegacyLaunch is the single TOML decode shared by every legacy read
// site that goes through the migration boundary, so the keys the migration
// path refuses on and the keys its read-only fallback tolerates can never
// disagree. It returns the undecoded key names sorted; the caller decides
// whether they are fatal.
//
// LoadLegacyLaunch (config.go) deliberately stays outside this: it is the
// boundary-less compatibility loader, reached only when resolveLaunchConfig
// has no boundary at all, and it is lenient by contract — it decodes nothing
// it could refuse on, and nothing downstream of it can retire a file.
func decodeLegacyLaunch(data []byte) (LaunchConfig, []string, error) {
	var lc LaunchConfig
	md, err := toml.Decode(string(data), &lc)
	if err != nil {
		return LaunchConfig{}, nil, fmt.Errorf("%w: %v", ErrLegacyMalformed, err)
	}
	undecoded := md.Undecoded()
	keys := make([]string, 0, len(undecoded))
	for _, k := range undecoded {
		keys = append(keys, k.String())
	}
	sort.Strings(keys)
	return stripLegacyUsageOptIn(lc), keys, nil
}

type legacySnapshotPlatform interface {
	Revalidate([]byte) error
	AllocateBackup() (*BackupAllocation, error)
	UnlinkNamedSource() error
	SyncParent() error
	Close() error
}

func (s *LegacySnapshot) Revalidate() error {
	if s == nil || s.platform == nil {
		return ErrLegacyDrift
	}
	return s.platform.Revalidate(s.Data)
}

func (s *LegacySnapshot) Close() error {
	if s == nil || s.platform == nil {
		return nil
	}
	err := s.platform.Close()
	s.platform = nil
	return err
}

func (s *LegacySnapshot) AllocateBackup() (*BackupAllocation, error) {
	if s == nil || s.platform == nil {
		return nil, ErrLegacyDrift
	}
	return s.platform.AllocateBackup()
}

func (s *LegacySnapshot) UnlinkNamedSource() error {
	if s == nil || s.platform == nil {
		return ErrLegacyDrift
	}
	return s.platform.UnlinkNamedSource()
}

func (s *LegacySnapshot) SyncParent() error {
	if s == nil || s.platform == nil {
		return ErrLegacyDrift
	}
	return s.platform.SyncParent()
}

// BackupAllocation carries the exact inode allocated by this attempt. On Unix
// it keeps an identity descriptor open until Close, including after
// CloseWriter, so inode reuse cannot make a replacement look attempt-owned.
// Its pathname is display-only until Validate and Revalidate prove the
// directory entry still names that inode and contains the captured source
// bytes.
type BackupAllocation struct {
	Name     string
	Path     string
	Identity FileIdentity
	platform legacyBackupPlatform
}

type legacyBackupPlatform interface {
	Write([]byte) error
	SetPrivateMode(os.FileMode) error
	SyncFile() error
	CloseWriter() error
	SyncParent() error
	Validate([]byte) error
	Revalidate([]byte) error
	CleanupPartial() error
	Close() error
}

func (b *BackupAllocation) Write(data []byte) error {
	if b == nil || b.platform == nil {
		return ErrBackupIdentityLost
	}
	return b.platform.Write(data)
}

func (b *BackupAllocation) SetPrivateMode(source os.FileMode) error {
	if b == nil || b.platform == nil {
		return ErrBackupIdentityLost
	}
	return b.platform.SetPrivateMode(source)
}

func (b *BackupAllocation) SyncFile() error {
	if b == nil || b.platform == nil {
		return ErrBackupIdentityLost
	}
	return b.platform.SyncFile()
}

func (b *BackupAllocation) CloseWriter() error {
	if b == nil || b.platform == nil {
		return ErrBackupIdentityLost
	}
	return b.platform.CloseWriter()
}

func (b *BackupAllocation) SyncParent() error {
	if b == nil || b.platform == nil {
		return ErrBackupIdentityLost
	}
	return b.platform.SyncParent()
}

func (b *BackupAllocation) Validate(expected []byte) error {
	if b == nil || b.platform == nil {
		return ErrBackupIdentityLost
	}
	return b.platform.Validate(expected)
}

func (b *BackupAllocation) Revalidate(expected []byte) error {
	if b == nil || b.platform == nil {
		return ErrBackupIdentityLost
	}
	return b.platform.Revalidate(expected)
}

func (b *BackupAllocation) CleanupPartial() error {
	if b == nil || b.platform == nil {
		return ErrBackupIdentityLost
	}
	return b.platform.CleanupPartial()
}

func (b *BackupAllocation) Close() error {
	if b == nil || b.platform == nil {
		return nil
	}
	err := b.platform.Close()
	b.platform = nil
	return err
}

type legacyProbe interface {
	Exists() bool
	Regular() bool
	Capture() (*LegacySnapshot, error)
	Close() error
}

// MigrationFS is deliberately sealed to this package. Production and tests
// select an implementation per call; there is no package-global mutable seam.
type MigrationFS interface {
	probe(path string) (legacyProbe, error)
	loadReadOnly(path string) (LaunchConfig, error)
	mutationSupported() bool
}

// LegacyMigrationBoundary captures paths, source identity, source bytes, and
// typed refusal state once. The same pointer is carried through startup and
// every cooperating launch writer.
type LegacyMigrationBoundary struct {
	Home, XDGConfigHome string
	ConfigPath          string
	LegacyPath          string
	Source              *LegacySnapshot
	Status              BoundaryStatus
	Refusal             error
	fs                  MigrationFS
}

func (b *LegacyMigrationBoundary) Close() error {
	if b == nil || b.Source == nil {
		return nil
	}
	return b.Source.Close()
}

// LoadReadOnlyLegacy is the compatibility-only loader. Unlike a migration
// source it may follow a leaf symlink, but the opened descriptor is
// nonblocking and must resolve to a regular file before content is read.
func (b *LegacyMigrationBoundary) LoadReadOnlyLegacy() (LaunchConfig, error) {
	if b == nil || b.fs == nil {
		return LaunchConfig{}, ErrNoLegacyLaunch
	}
	return b.fs.loadReadOnly(b.LegacyPath)
}

// PrepareLegacyMigrationBoundary performs the authoritative source
// existence/classification/open/read/decode sequence before any config parent,
// lock, temp, config, or backup can be created.
func PrepareLegacyMigrationBoundary(env EnvSnapshot, fsys MigrationFS) (*LegacyMigrationBoundary, error) {
	if fsys == nil {
		fsys = NativeMigrationFS()
	}
	configPath, legacyPath := candidateLegacyMigrationPaths(env, runtime.GOOS)
	b := &LegacyMigrationBoundary{
		Home:          env.Home,
		XDGConfigHome: env.XDGConfigHome,
		ConfigPath:    configPath,
		LegacyPath:    legacyPath,
		Status:        BoundaryNoSource,
		fs:            fsys,
	}

	probe, err := fsys.probe(legacyPath)
	if err != nil {
		b.Status = BoundaryRefused
		if containsUnicodeControl(env.Home) || containsUnicodeControl(env.XDGConfigHome) || containsUnicodeControl(env.UserConfigHome) ||
			containsUnicodeControl(configPath) || containsUnicodeControl(legacyPath) {
			b.Refusal = ErrLegacyPathControl
		} else {
			b.Refusal = err
		}
		return b, nil
	}
	if probe == nil || !probe.Exists() {
		if probe != nil {
			_ = probe.Close()
		}
		return b, nil
	}
	defer probe.Close() // Capture transfers ownership and makes this a no-op.

	if err := validateLegacyMigrationPaths(env, configPath, legacyPath); err != nil {
		b.Status = BoundaryRefused
		b.Refusal = err
		return b, nil
	}
	if !probe.Regular() {
		b.Status = BoundaryRefused
		b.Refusal = ErrLegacyNonRegular
		return b, nil
	}
	if !fsys.mutationSupported() {
		b.Status = BoundaryRefused
		b.Refusal = ErrLegacyMigrationUnsupported
		return b, nil
	}

	snapshot, err := probe.Capture()
	if err != nil {
		b.Status = BoundaryRefused
		b.Refusal = err
		return b, nil
	}
	if snapshot == nil {
		b.Status = BoundaryRefused
		b.Refusal = ErrLegacyDrift
		return b, nil
	}
	// Defend the immutable-payload contract against an implementation handing
	// us a mutable scratch buffer.
	snapshot.Data = bytes.Clone(snapshot.Data)
	b.Source = snapshot
	b.Status = BoundaryMigratable
	return b, nil
}

// CaptureEnvSnapshot captures the inputs os.UserConfigDir uses before any
// migration-related config load or filesystem mutation.
func CaptureEnvSnapshot() (EnvSnapshot, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return EnvSnapshot{}, fmt.Errorf("resolve home directory: %w", err)
	}
	configHome, err := os.UserConfigDir()
	if err != nil {
		return EnvSnapshot{}, fmt.Errorf("resolve user config directory: %w", err)
	}
	return EnvSnapshot{
		Home:           home,
		XDGConfigHome:  os.Getenv("XDG_CONFIG_HOME"),
		UserConfigHome: configHome,
	}, nil
}

func containsUnicodeControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// candidateLegacyMigrationPaths deliberately performs no validation. Secure
// preparation first uses the candidate legacy name for its authoritative
// no-follow existence check; only an existing entry triggers pair/control
// refusal, preserving the historical silent no-source fast path.
func candidateLegacyMigrationPaths(env EnvSnapshot, goos string) (configPath, legacyPath string) {
	configBase := env.UserConfigHome
	if configBase == "" {
		switch goos {
		case "darwin":
			configBase = filepath.Join(env.Home, "Library", "Application Support")
		default:
			if env.XDGConfigHome != "" {
				configBase = env.XDGConfigHome
			} else {
				configBase = filepath.Join(env.Home, ".config")
			}
		}
	}
	legacyBase := env.XDGConfigHome
	if legacyBase == "" {
		legacyBase = filepath.Join(env.Home, ".config")
	}
	return filepath.Clean(filepath.Join(configBase, "forgectl", "config.toml")),
		filepath.Clean(filepath.Join(legacyBase, "claunch", "claunch.conf"))
}

// UnmigratableSiblingPath names a config file sitting in the legacy directory
// that forgectl cannot migrate, or "" when there is none. forgectl migrates
// the historical claunch.conf format only; a claunch that has since moved to
// a config.toml of its own is neither a migration source nor absent, and
// reporting it as absent sends the operator looking for a file that is right
// there (#417).
//
// The path is derived from LegacyPath's own directory, never from the config
// directory: on darwin those diverge (~/Library/Application Support vs
// ~/.config), so a configDir-derived probe would report absent on the exact
// file this exists to name. The file is probed for existence and never
// opened: it is not an import source, and forgectl models no schema for it.
func (b *LegacyMigrationBoundary) UnmigratableSiblingPath() string {
	if b == nil || b.LegacyPath == "" {
		return ""
	}
	candidate := filepath.Join(filepath.Dir(b.LegacyPath), "config.toml")
	info, err := os.Lstat(candidate)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return candidate
}

func validateLegacyMigrationPaths(env EnvSnapshot, configPath, legacyPath string) error {
	for _, value := range []string{env.Home, env.XDGConfigHome, env.UserConfigHome, configPath, legacyPath} {
		if containsUnicodeControl(value) {
			return ErrLegacyPathControl
		}
	}
	if env.XDGConfigHome == "" {
		return nil
	}
	if !filepath.IsAbs(env.XDGConfigHome) {
		return fmt.Errorf("%w: XDG_CONFIG_HOME must be absolute", ErrLegacyPathPolicy)
	}
	xdg := filepath.Clean(env.XDGConfigHome)
	wantConfig := filepath.Join(xdg, "forgectl", "config.toml")
	wantLegacy := filepath.Join(xdg, "claunch", "claunch.conf")
	if configPath != wantConfig || legacyPath != wantLegacy {
		return fmt.Errorf("%w: config and legacy paths do not share the captured XDG_CONFIG_HOME", ErrLegacyPathPolicy)
	}
	return nil
}

func resolveLegacyMigrationPaths(env EnvSnapshot, goos string) (configPath, legacyPath string, err error) {
	configPath, legacyPath = candidateLegacyMigrationPaths(env, goos)
	if err := validateLegacyMigrationPaths(env, configPath, legacyPath); err != nil {
		return "", "", err
	}
	return configPath, legacyPath, nil
}

// resolveCurrentLegacyMigrationPaths is kept narrow so tests can exercise the
// Darwin and Unix policies without mutating process-global environment.
func resolveCurrentLegacyMigrationPaths(env EnvSnapshot) (string, string, error) {
	return resolveLegacyMigrationPaths(env, runtime.GOOS)
}
