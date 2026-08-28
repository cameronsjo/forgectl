package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

type configAction uint8

const (
	configUnchanged configAction = iota
	configWritten
	configConfirmed
)

type backupState uint8

const (
	backupNotAllocated backupState = iota
	backupPartialOwned
	backupPartialIdentityLost
	backupCompleteNotDurable
	backupDurableUnverified
	backupDurableVerified
	backupDrifted
)

type retirementState uint8

const (
	retirementNotStarted retirementState = iota
	retirementSourceRetained
	retirementSourceMissingUnproved
	retirementRemoved
	retirementDurabilityUnknown
)

type MigrationResult struct {
	Effective  config.LaunchConfig
	Commit     commitState
	Action     configAction
	Backup     backupState
	Retirement retirementState
	BackupPath string
	Notice     string
	Err        error
}

type migrationTxnOps struct {
	parent           configParentOps
	atomic           atomicWriteOps
	ensureParent     func(string, configParentOps) ([]string, error)
	withLock         func(string, func() error) error
	readFile         func(string) ([]byte, error)
	writeConfig      func(string, []byte, atomicWriteOps) (commitState, error)
	confirmConfig    func(string, []byte, atomicWriteOps) error
	sourceRevalidate func(*config.LegacySnapshot) error
	allocateBackup   func(*config.LegacySnapshot) (*config.BackupAllocation, error)
	backupWrite      func(*config.BackupAllocation, []byte) error
	backupMode       func(*config.BackupAllocation, os.FileMode) error
	backupSyncFile   func(*config.BackupAllocation) error
	backupClose      func(*config.BackupAllocation) error
	backupSyncParent func(*config.BackupAllocation) error
	backupValidate   func(*config.BackupAllocation, []byte) error
	backupRevalidate func(*config.BackupAllocation, []byte) error
	backupCleanup    func(*config.BackupAllocation) error
	sourceUnlink     func(*config.LegacySnapshot) error
	sourceSyncParent func(*config.LegacySnapshot) error
}

func nativeMigrationTxnOps() migrationTxnOps {
	return migrationTxnOps{
		parent:        nativeConfigParentOps(),
		atomic:        nativeAtomicWriteOps(),
		ensureParent:  ensureConfigParentDurable,
		withLock:      config.WithFileLock,
		readFile:      config.ReadPath,
		writeConfig:   writeConfigAtomicWithOps,
		confirmConfig: confirmConfigDurableWithOps,
		sourceRevalidate: func(source *config.LegacySnapshot) error {
			return source.Revalidate()
		},
		allocateBackup: func(source *config.LegacySnapshot) (*config.BackupAllocation, error) {
			return source.AllocateBackup()
		},
		backupWrite: func(backup *config.BackupAllocation, data []byte) error { return backup.Write(data) },
		backupMode: func(backup *config.BackupAllocation, mode os.FileMode) error {
			return backup.SetPrivateMode(mode)
		},
		backupSyncFile:   func(backup *config.BackupAllocation) error { return backup.SyncFile() },
		backupClose:      func(backup *config.BackupAllocation) error { return backup.CloseWriter() },
		backupSyncParent: func(backup *config.BackupAllocation) error { return backup.SyncParent() },
		backupValidate: func(backup *config.BackupAllocation, expected []byte) error {
			return backup.Validate(expected)
		},
		backupRevalidate: func(backup *config.BackupAllocation, expected []byte) error {
			return backup.Revalidate(expected)
		},
		backupCleanup:    func(backup *config.BackupAllocation) error { return backup.CleanupPartial() },
		sourceUnlink:     func(source *config.LegacySnapshot) error { return source.UnlinkNamedSource() },
		sourceSyncParent: func(source *config.LegacySnapshot) error { return source.SyncParent() },
	}
}

func rejectTOMLPathControls(path string) error {
	if !utf8.ValidString(path) {
		return config.ErrLegacyPathControl
	}
	for _, r := range path {
		if unicode.IsControl(r) {
			return config.ErrLegacyPathControl
		}
	}
	return nil
}

func renderImportedLaunch(existing []byte, launchConfig config.LaunchConfig, legacyPath string) ([]byte, error) {
	if err := rejectTOMLPathControls(legacyPath); err != nil {
		return nil, err
	}
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(struct {
		Launch config.LaunchConfig `toml:"launch"`
	}{Launch: launchConfig}); err != nil {
		return nil, fmt.Errorf("encode imported launch config: %w", err)
	}
	header := fmt.Sprintf("\n# ── launch: imported from %s (forgectl launch migrate) ──\n", legacyPath)
	return append(bytes.Clone(existing), []byte(header+encoded.String())...), nil
}

func renderReplacedLaunch(existing []byte, merged config.LaunchConfig, legacyPath string) ([]byte, error) {
	if err := rejectTOMLPathControls(legacyPath); err != nil {
		return nil, err
	}
	lines := strings.Split(string(existing), "\n")
	dropped := make([]bool, len(lines))
	commentOrBlank := make([]bool, len(lines))
	var scanner tomlLineScanner
	inLaunch := false
	for i, line := range lines {
		mid := scanner.inTripleBasic || scanner.inTripleLiteral || scanner.bracketDepth > 0
		trimmed := strings.TrimSpace(line)
		commentOrBlank[i] = !mid && (trimmed == "" || strings.HasPrefix(trimmed, "#"))
		table, ok := scanner.scanLine(line)
		if !ok {
			return nil, fmt.Errorf("rewrite config: line %d has ambiguous TOML; refusing to guess", i+1)
		}
		if table != "" {
			inLaunch = isLaunchTable(table)
		}
		dropped[i] = inLaunch
	}
	if scanner.pending() {
		return nil, fmt.Errorf("rewrite config: file ends inside an unterminated multi-line value")
	}
	for i := len(dropped) - 2; i >= 0; i-- {
		if commentOrBlank[i] {
			dropped[i] = dropped[i+1]
		}
	}
	var kept []string
	firstLaunch := -1
	for i, line := range lines {
		if dropped[i] {
			if firstLaunch < 0 {
				firstLaunch = len(kept)
			}
			continue
		}
		kept = append(kept, line)
	}
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(struct {
		Launch config.LaunchConfig `toml:"launch"`
	}{Launch: merged}); err != nil {
		return nil, err
	}
	block := append([]string{fmt.Sprintf("# ── launch: merged with %s (forgectl launch migrate) ──", legacyPath)},
		strings.Split(strings.TrimRight(encoded.String(), "\n"), "\n")...)
	var out []string
	if firstLaunch < 0 {
		out = append(kept, "")
		out = append(out, block...)
	} else {
		out = append(out, kept[:firstLaunch]...)
		out = append(out, block...)
		out = append(out, kept[firstLaunch:]...)
	}
	return []byte(strings.Join(out, "\n")), nil
}

func refusalResult(boundary *config.LegacyMigrationBoundary, cfg config.Config, cause error) MigrationResult {
	result := MigrationResult{
		Effective:  cfg.Launch,
		Action:     configUnchanged,
		Backup:     backupNotAllocated,
		Retirement: retirementSourceRetained,
		Err:        cause,
	}
	if !cfg.HasLaunchSection() {
		if boundary != nil && boundary.Source != nil {
			result.Effective = boundary.Source.Launch
		} else if fallback, err := boundary.LoadReadOnlyLegacy(); err == nil {
			result.Effective = fallback
		}
	}
	result.Notice = "automatic legacy migration skipped; source retained"
	return result
}

// unsupportedFieldsCause is the single predicate behind forgectl#417's gate,
// shared by the automatic and the explicit surface so the two can never drift
// apart on what counts as unrepresentable. It returns nil when the source was
// decoded in full.
func unsupportedFieldsCause(boundary *config.LegacyMigrationBoundary) error {
	if boundary == nil || boundary.Source == nil || len(boundary.Source.UndecodedKeys) == 0 {
		return nil
	}
	return config.UnsupportedFieldsError(boundary.Source.UndecodedKeys)
}

// unsupportedFieldsRefusal is forgectl#417's gate. A legacy source carrying
// keys LaunchConfig has no field for was only partly decoded, so rendering
// [launch] from that decode would drop the remainder — and the migration
// transaction then backs up and unlinks the one file that still held it.
// Refusing is a result, not a failure: the source stays on disk, config.toml
// is untouched, and the caller keeps reading the legacy file leniently.
func unsupportedFieldsRefusal(boundary *config.LegacyMigrationBoundary, cfg config.Config) (MigrationResult, bool) {
	cause := unsupportedFieldsCause(boundary)
	if cause == nil {
		return MigrationResult{}, false
	}
	result := refusalResult(boundary, cfg, cause)
	result.Notice = fmt.Sprintf(
		"%s left in place: forgectl cannot represent %s, so migrating it would silently drop those settings — resolve them by hand, or set %s=1 to stop this notice",
		termsafe.QuotePath(boundary.LegacyPath), summarizeKeys(boundary.Source.UndecodedKeys), skipLegacyMigrateEnv)
	return result, true
}

// summarizeKeys renders an undecoded-key list for a one-line notice that
// prints on every launch until the operator settles the file. A foreign schema
// can contribute dozens of keys — and a nested table contributes its own name
// as well as each child — so the list is capped rather than allowed to grow
// into a wall of text on every invocation (#418 review).
func summarizeKeys(keys []string) string {
	const maxNamed = 5
	if len(keys) <= maxNamed {
		return termsafe.SafeLine(strings.Join(keys, ", "))
	}
	return fmt.Sprintf("%s (+%d more)",
		termsafe.SafeLine(strings.Join(keys[:maxNamed], ", ")), len(keys)-maxNamed)
}

func authoritativePeerWinner(raw []byte, locked config.Config, source config.LaunchConfig) bool {
	if !locked.HasLaunchSection() && !hasLaunchSection(raw) {
		return false
	}
	_, added := config.MergeLegacyIntoLaunch(locked, source)
	return added == 0
}

func unprovedMissingSourceResult(boundary *config.LegacyMigrationBoundary, locked config.Config, cause error) MigrationResult {
	result := refusalResult(boundary, locked, cause)
	result.Retirement = retirementSourceMissingUnproved
	result.Notice = "legacy source disappeared without authoritative peer-migration proof; captured fallback remains effective"
	return result
}

func readLockedConfig(path string, readFile func(string) ([]byte, error)) ([]byte, config.Config, error) {
	raw, err := readFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, config.Config{}, nil
		}
		return nil, config.Config{}, err
	}
	cfg, err := config.DecodeStrict(raw)
	return raw, cfg, err
}

func backupFailure(result MigrationResult, backup *config.BackupAllocation, state backupState, err error, ops migrationTxnOps, cleanup bool) MigrationResult {
	result.Err = err
	result.Retirement = retirementSourceRetained
	result.Backup = state
	if backup != nil {
		result.BackupPath = backup.Path
		if cleanup {
			if cleanupErr := ops.backupCleanup(backup); cleanupErr != nil {
				result.Backup = backupPartialIdentityLost
				result.Err = errors.Join(err, cleanupErr)
			} else {
				result.Backup = backupNotAllocated
			}
		}
	}
	result.Notice = "config committed; legacy source retained because backup did not complete"
	return result
}

func migrateLocked(boundary *config.LegacyMigrationBoundary, ops migrationTxnOps) MigrationResult {
	raw, locked, err := readLockedConfig(boundary.ConfigPath, ops.readFile)
	if err != nil {
		return refusalResult(boundary, locked, err)
	}
	if err := ops.sourceRevalidate(boundary.Source); err != nil {
		if errors.Is(err, config.ErrLegacySourceMissing) {
			if authoritativePeerWinner(raw, locked, boundary.Source.Launch) {
				return MigrationResult{Effective: locked.Launch, Action: configUnchanged, Backup: backupNotAllocated, Retirement: retirementRemoved}
			}
			return unprovedMissingSourceResult(boundary, locked, err)
		}
		return refusalResult(boundary, locked, err)
	}

	// Defense in depth, not a live branch: migrateLegacyAutomatically is the
	// only caller today and gates on the same predicate before it, and
	// UndecodedKeys is immutable between the two checks — so this arm cannot
	// fire as the code stands. It is here for a future second caller of
	// migrateLocked, which would otherwise reach the render/backup/unlink
	// ladder ungated (#418 review).
	if refusal, refused := unsupportedFieldsRefusal(boundary, locked); refused {
		return refusal
	}

	shadow := locked.HasLaunchSection() || hasLaunchSection(raw)
	intended := boundary.Source.Launch
	added := 0
	var rendered []byte
	if shadow {
		intended, added = config.MergeLegacyIntoLaunch(locked, boundary.Source.Launch)
		if added == 0 {
			rendered = bytes.Clone(raw)
		} else {
			rendered, err = renderReplacedLaunch(raw, intended, boundary.LegacyPath)
		}
	} else {
		if intended.IsZero() {
			return MigrationResult{Effective: intended, Action: configUnchanged, Backup: backupNotAllocated, Retirement: retirementSourceRetained}
		}
		rendered, err = renderImportedLaunch(raw, intended, boundary.LegacyPath)
	}
	if err != nil {
		return refusalResult(boundary, locked, err)
	}

	result := MigrationResult{Effective: intended, Backup: backupNotAllocated, Retirement: retirementSourceRetained}
	if bytes.Equal(raw, rendered) {
		result.Action = configConfirmed
		if err := ops.confirmConfig(boundary.ConfigPath, raw, ops.atomic); err != nil {
			result.Err = err
			result.Notice = "legacy source retained because current config durability could not be confirmed"
			return result
		}
		result.Commit = commitDurable
	} else {
		result.Action = configWritten
		result.Commit, err = ops.writeConfig(boundary.ConfigPath, rendered, ops.atomic)
		if err != nil {
			result.Err = err
			if result.Commit == commitNone {
				if shadow {
					result.Effective = locked.Launch
				} else {
					result.Effective = boundary.Source.Launch
				}
			}
			result.Notice = "legacy source retained because config commit did not become durable"
			return result
		}
	}

	backup, err := ops.allocateBackup(boundary.Source)
	if err != nil {
		return backupFailure(result, nil, backupNotAllocated, err, ops, false)
	}
	defer backup.Close() //nolint:errcheck
	result.Backup = backupPartialOwned
	result.BackupPath = backup.Path
	if err := ops.backupWrite(backup, boundary.Source.Data); err != nil {
		return backupFailure(result, backup, backupPartialOwned, err, ops, true)
	}
	if err := ops.backupMode(backup, boundary.Source.Mode); err != nil {
		return backupFailure(result, backup, backupPartialOwned, err, ops, true)
	}
	if err := ops.backupSyncFile(backup); err != nil {
		return backupFailure(result, backup, backupPartialOwned, err, ops, true)
	}
	if err := ops.backupClose(backup); err != nil {
		return backupFailure(result, backup, backupPartialOwned, err, ops, true)
	}
	result.Backup = backupCompleteNotDurable
	if err := ops.backupSyncParent(backup); err != nil {
		return backupFailure(result, backup, backupCompleteNotDurable, err, ops, false)
	}
	result.Backup = backupDurableUnverified
	if err := ops.backupValidate(backup, boundary.Source.Data); err != nil {
		return backupFailure(result, backup, backupDrifted, err, ops, false)
	}
	result.Backup = backupDurableVerified

	// Keep both validation descriptors open and repeat both pathname/content
	// checks immediately before unlinkat. The residual check-to-unlink window
	// is an explicit platform limitation in the migration contract.
	for range 2 {
		if err := ops.sourceRevalidate(boundary.Source); err != nil {
			result.Err = err
			result.Notice = "config and backup committed; legacy source retained after source drift"
			return result
		}
		if err := ops.backupRevalidate(backup, boundary.Source.Data); err != nil {
			result.Backup = backupDrifted
			result.Err = err
			result.Notice = "config committed; legacy source retained after backup drift"
			return result
		}
	}
	if err := ops.sourceUnlink(boundary.Source); err != nil {
		result.Err = err
		result.Notice = "config and backup committed; legacy source could not be retired"
		return result
	}
	result.Retirement = retirementRemoved
	if err := ops.sourceSyncParent(boundary.Source); err != nil {
		result.Retirement = retirementDurabilityUnknown
		result.Err = err
		result.Notice = "config and backup committed; legacy source removal durability is unknown"
		return result
	}
	switch {
	case !shadow:
		result.Notice = fmt.Sprintf(
			"migrated %d profile(s) from claunch.conf into config.toml's [launch] section (old file kept as %s)",
			len(boundary.Source.Launch.Projects), filepath.Base(result.BackupPath))
	case added == 0:
		result.Notice = fmt.Sprintf(
			"claunch.conf was fully superseded by config.toml's [launch] section, so nothing was merged (old file kept as %s)",
			filepath.Base(result.BackupPath))
	default:
		result.Notice = fmt.Sprintf(
			"merged %d addition(s) from claunch.conf into config.toml's [launch] section (old file kept as %s)",
			added, filepath.Base(result.BackupPath))
	}
	return result
}

func migrateLegacyAutomatically(boundary *config.LegacyMigrationBoundary, startup config.Config, ops migrationTxnOps) MigrationResult {
	if boundary == nil || boundary.Status == config.BoundaryNoSource {
		return MigrationResult{Effective: startup.Launch, Action: configUnchanged, Backup: backupNotAllocated, Retirement: retirementNotStarted}
	}
	if boundary.Status == config.BoundaryRefused {
		return refusalResult(boundary, startup, boundary.Refusal)
	}
	// #417's gate runs before ensureParent, which mkdirs the config parent:
	// a refusal must leave nothing behind, not even an empty directory the
	// operator never asked for. migrateLocked re-checks it immediately before
	// the render/backup/unlink ladder, so the guarantee does not depend on
	// this call alone.
	if refusal, refused := unsupportedFieldsRefusal(boundary, startup); refused {
		return refusal
	}
	if err := config.ValidatePath(boundary.ConfigPath); err != nil {
		return refusalResult(boundary, startup, err)
	}
	if err := ops.sourceRevalidate(boundary.Source); err != nil {
		return refusalResult(boundary, startup, err)
	}
	if _, err := ops.ensureParent(boundary.ConfigPath, ops.parent); err != nil {
		return refusalResult(boundary, startup, err)
	}
	if err := ops.sourceRevalidate(boundary.Source); err != nil {
		return refusalResult(boundary, startup, err)
	}
	var result MigrationResult
	err := ops.withLock(boundary.ConfigPath, func() error {
		result = migrateLocked(boundary, ops)
		return nil
	})
	if err != nil {
		return refusalResult(boundary, startup, err)
	}
	return result
}

func migrateLegacyExplicit(boundary *config.LegacyMigrationBoundary, ops migrationTxnOps) MigrationResult {
	result := MigrationResult{Action: configUnchanged, Backup: backupNotAllocated, Retirement: retirementSourceRetained}
	if boundary == nil || boundary.Status == config.BoundaryNoSource {
		result.Err = config.ErrNoLegacyLaunch
		return result
	}
	if boundary.Status == config.BoundaryRefused {
		result.Err = boundary.Refusal
		return result
	}
	if cause := unsupportedFieldsCause(boundary); cause != nil {
		result.Err = cause
		return result
	}
	if boundary.Source.Launch.IsZero() {
		result.Err = fmt.Errorf("legacy claunch.conf has no [defaults] or [[project]] to import")
		return result
	}
	if err := config.ValidatePath(boundary.ConfigPath); err != nil {
		result.Err = err
		return result
	}
	if err := ops.sourceRevalidate(boundary.Source); err != nil {
		result.Err = err
		return result
	}
	if _, err := ops.ensureParent(boundary.ConfigPath, ops.parent); err != nil {
		result.Err = err
		return result
	}
	if err := ops.sourceRevalidate(boundary.Source); err != nil {
		result.Err = err
		return result
	}
	err := ops.withLock(boundary.ConfigPath, func() error {
		raw, locked, readErr := readLockedConfig(boundary.ConfigPath, ops.readFile)
		if readErr != nil {
			return readErr
		}
		result.Effective = locked.Launch
		if hasLaunchSection(raw) {
			return fmt.Errorf("config already has a [launch] section; refusing to overwrite an existing launch profile")
		}
		if recheckErr := ops.sourceRevalidate(boundary.Source); recheckErr != nil {
			return recheckErr
		}
		rendered, renderErr := renderImportedLaunch(raw, boundary.Source.Launch, boundary.LegacyPath)
		if renderErr != nil {
			return renderErr
		}
		result.Effective = boundary.Source.Launch
		result.Action = configWritten
		result.Commit, renderErr = ops.writeConfig(boundary.ConfigPath, rendered, ops.atomic)
		return renderErr
	})
	if err != nil {
		result.Err = err
		return result
	}
	if result.Commit != commitDurable {
		result.Err = fmt.Errorf("explicit migration config commit did not become durable")
		return result
	}
	result.Notice = fmt.Sprintf("Imported %d launch profile(s)", len(boundary.Source.Launch.Projects))
	return result
}
