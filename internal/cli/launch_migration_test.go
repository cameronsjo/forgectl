package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

func transactionBoundary(t *testing.T, base string, legacy []byte) *config.LegacyMigrationBoundary {
	t.Helper()
	legacyPath := filepath.Join(base, "claunch", "claunch.conf")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	env := config.EnvSnapshot{Home: filepath.Join(base, "home"), XDGConfigHome: base, UserConfigHome: base}
	b, err := config.PrepareLegacyMigrationBoundary(env, config.NativeMigrationFS())
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != config.BoundaryMigratable {
		b.Close() //nolint:errcheck
		t.Fatalf("boundary status = %v, refusal = %v", b.Status, b.Refusal)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestMigrationTransaction_FallbackWritesDurablyBacksUpExactBytesAndRetires(t *testing.T) {
	base := t.TempDir()
	legacy := []byte("[defaults]\nmodel = \"sonnet\"\n")
	b := transactionBoundary(t, base, legacy)

	result := migrateLegacyAutomatically(b, config.Config{}, nativeMigrationTxnOps())
	if result.Err != nil {
		t.Fatalf("migration error = %v", result.Err)
	}
	if result.Commit != commitDurable || result.Action != configWritten || result.Backup != backupDurableVerified || result.Retirement != retirementRemoved {
		t.Fatalf("states = commit:%v action:%v backup:%v retirement:%v", result.Commit, result.Action, result.Backup, result.Retirement)
	}
	if result.Effective.Defaults.Model != "sonnet" {
		t.Fatalf("effective model = %q, want sonnet", result.Effective.Defaults.Model)
	}
	data, err := os.ReadFile(b.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "[launch.defaults]") {
		t.Fatalf("config bytes missing launch section:\n%s", data)
	}
	if got, err := os.ReadFile(result.BackupPath); err != nil || string(got) != string(legacy) {
		t.Fatalf("backup = %q, error %v; want exact source %q", got, err, legacy)
	}
	if _, err := os.Lstat(b.LegacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy source remains after durable retirement: %v", err)
	}
}

func TestMigrationTransaction_VisibleRenameFailureUsesNewEffectiveAndRetainsSource(t *testing.T) {
	base := t.TempDir()
	b := transactionBoundary(t, base, []byte("[defaults]\nmodel = \"sonnet\"\n"))
	ops := nativeMigrationTxnOps()
	ops.atomic.syncDir = func(path string) error {
		if path == filepath.Dir(b.ConfigPath) {
			return errors.New("injected config parent sync")
		}
		return syncDirectory(path)
	}

	result := migrateLegacyAutomatically(b, config.Config{}, ops)
	if result.Commit != commitRenamed || result.Action != configWritten || result.Effective.Defaults.Model != "sonnet" {
		t.Fatalf("result = %+v, want visible written config with new effective", result)
	}
	if result.Err == nil || result.Retirement != retirementSourceRetained || result.Backup != backupNotAllocated {
		t.Fatalf("result = %+v, want error/source retained/no backup", result)
	}
	if _, err := os.Stat(b.LegacyPath); err != nil {
		t.Fatalf("source not retained: %v", err)
	}
	if _, err := os.Lstat(b.LegacyPath + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("backup allocated before config durability: %v", err)
	}
}

func TestMigrationRetryDurability_AddedZeroConfirmsCurrentFileBeforeBackup(t *testing.T) {
	base := t.TempDir()
	legacy := []byte("[defaults]\nmodel = \"sonnet\"\n")
	first := transactionBoundary(t, base, legacy)
	firstOps := nativeMigrationTxnOps()
	firstOps.atomic.syncDir = func(path string) error {
		if path == filepath.Dir(first.ConfigPath) {
			return errors.New("injected first-attempt config parent sync")
		}
		return syncDirectory(path)
	}
	firstResult := migrateLegacyAutomatically(first, config.Config{}, firstOps)
	if firstResult.Commit != commitRenamed {
		t.Fatalf("first commit = %v, want renamed", firstResult.Commit)
	}
	_ = first.Close()

	second := transactionBoundary(t, base, legacy)
	loaded := config.LoadPath(second.ConfigPath)
	secondOps := nativeMigrationTxnOps()
	var trace []string
	nativeOpen := secondOps.atomic.openRegular
	nativeRead := secondOps.atomic.readAll
	nativeSync := secondOps.atomic.syncFile
	nativeClose := secondOps.atomic.closeFile
	nativeSyncDir := secondOps.atomic.syncDir
	nativeAllocate := secondOps.allocateBackup
	secondOps.atomic.openRegular = func(path string) (*os.File, error) {
		trace = append(trace, "open")
		return nativeOpen(path)
	}
	secondOps.atomic.readAll = func(reader io.Reader) ([]byte, error) {
		trace = append(trace, "read")
		return nativeRead(reader)
	}
	secondOps.atomic.syncFile = func(file *os.File) error {
		trace = append(trace, "file-sync")
		return nativeSync(file)
	}
	secondOps.atomic.closeFile = func(file *os.File) error {
		trace = append(trace, "close")
		return nativeClose(file)
	}
	secondOps.atomic.syncDir = func(path string) error {
		trace = append(trace, "parent-sync")
		return nativeSyncDir(path)
	}
	secondOps.allocateBackup = func(source *config.LegacySnapshot) (*config.BackupAllocation, error) {
		trace = append(trace, "backup")
		return nativeAllocate(source)
	}
	result := migrateLegacyAutomatically(second, loaded, secondOps)
	if result.Err != nil || result.Action != configConfirmed || result.Commit != commitDurable || result.Retirement != retirementRemoved {
		t.Fatalf("retry result = %+v", result)
	}
	if strings.Join(trace, ",") != "open,read,file-sync,close,parent-sync,backup" {
		t.Fatalf("trace = %v, want complete current-attempt durability proof before backup", trace)
	}
}

func TestMigrationRetryDurability_ConfirmationFailureRetainsSourceWithoutBackup(t *testing.T) {
	base := t.TempDir()
	legacy := []byte("[defaults]\nmodel = \"sonnet\"\n")
	b := transactionBoundary(t, base, legacy)
	seed := nativeMigrationTxnOps()
	seed.atomic.syncDir = func(path string) error {
		if path == filepath.Dir(b.ConfigPath) {
			return errors.New("seed parent sync")
		}
		return syncDirectory(path)
	}
	_ = migrateLegacyAutomatically(b, config.Config{}, seed)
	_ = b.Close()

	retry := transactionBoundary(t, base, legacy)
	ops := nativeMigrationTxnOps()
	ops.confirmConfig = func(string, []byte, atomicWriteOps) error { return errors.New("injected confirmation") }
	result := migrateLegacyAutomatically(retry, config.LoadPath(retry.ConfigPath), ops)
	if result.Err == nil || result.Backup != backupNotAllocated || result.Retirement != retirementSourceRetained {
		t.Fatalf("retry result = %+v, want error/no backup/source retained", result)
	}
	if _, err := os.Lstat(retry.LegacyPath + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("backup allocated after confirmation failure: %v", err)
	}
}

func TestMigrationRetryDurability_EveryConfirmationFailurePrecedesBackup(t *testing.T) {
	tests := []struct {
		name   string
		inject func(*atomicWriteOps)
	}{
		{name: "open", inject: func(ops *atomicWriteOps) {
			ops.openRegular = func(string) (*os.File, error) { return nil, errors.New("injected open") }
		}},
		{name: "read", inject: func(ops *atomicWriteOps) {
			ops.readAll = func(io.Reader) ([]byte, error) { return nil, errors.New("injected read") }
		}},
		{name: "changed bytes", inject: func(ops *atomicWriteOps) {
			ops.readAll = func(io.Reader) ([]byte, error) { return []byte("changed"), nil }
		}},
		{name: "file sync", inject: func(ops *atomicWriteOps) {
			ops.syncFile = func(*os.File) error { return errors.New("injected file sync") }
		}},
		{name: "close", inject: func(ops *atomicWriteOps) {
			ops.closeFile = func(*os.File) error { return errors.New("injected close") }
		}},
		{name: "parent sync", inject: func(ops *atomicWriteOps) {
			ops.syncDir = func(string) error { return errors.New("injected parent sync") }
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			legacy := []byte("[defaults]\nmodel = \"sonnet\"\n")
			first := transactionBoundary(t, base, legacy)
			seed := nativeMigrationTxnOps()
			seed.atomic.syncDir = func(path string) error {
				if path == filepath.Dir(first.ConfigPath) {
					return errors.New("seed visible rename")
				}
				return syncDirectory(path)
			}
			seeded := migrateLegacyAutomatically(first, config.Config{}, seed)
			if seeded.Commit != commitRenamed {
				t.Fatalf("seed result=%+v", seeded)
			}
			_ = first.Close()

			retry := transactionBoundary(t, base, legacy)
			ops := nativeMigrationTxnOps()
			tt.inject(&ops.atomic)
			backupCalls := 0
			ops.allocateBackup = func(*config.LegacySnapshot) (*config.BackupAllocation, error) {
				backupCalls++
				return nil, errors.New("backup must not run")
			}
			result := migrateLegacyAutomatically(retry, config.LoadPath(retry.ConfigPath), ops)
			if result.Err == nil || result.Action != configConfirmed || result.Backup != backupNotAllocated || result.Retirement != retirementSourceRetained {
				t.Fatalf("result=%+v", result)
			}
			if backupCalls != 0 {
				t.Fatalf("backup calls=%d, want zero", backupCalls)
			}
			if _, err := os.Stat(retry.LegacyPath); err != nil {
				t.Fatalf("source not retained: %v", err)
			}
		})
	}
}

func TestMigrationTransaction_BackupFailureKeepsDurableConfigEffectiveAndSource(t *testing.T) {
	base := t.TempDir()
	b := transactionBoundary(t, base, []byte("[defaults]\nmodel = \"sonnet\"\n"))
	ops := nativeMigrationTxnOps()
	ops.backupSyncFile = func(*config.BackupAllocation) error { return errors.New("injected backup sync") }
	result := migrateLegacyAutomatically(b, config.Config{}, ops)
	if result.Commit != commitDurable || result.Effective.Defaults.Model != "sonnet" || result.Err == nil {
		t.Fatalf("result = %+v, want durable new effective with backup error", result)
	}
	if result.Retirement != retirementSourceRetained {
		t.Fatalf("retirement = %v, want source retained", result.Retirement)
	}
	if _, err := os.Stat(b.LegacyPath); err != nil {
		t.Fatalf("source not retained: %v", err)
	}
}

func TestMigrationExplicit_UsesPreparedBoundaryAndSharedLockWithoutRetiringSource(t *testing.T) {
	base := t.TempDir()
	b := transactionBoundary(t, base, []byte("[defaults]\nmodel = \"sonnet\"\n"))
	ops := nativeMigrationTxnOps()
	lockCalls := 0
	nativeLock := ops.withLock
	ops.withLock = func(path string, fn func() error) error {
		lockCalls++
		return nativeLock(path, fn)
	}
	result := migrateLegacyExplicit(b, ops)
	if result.Err != nil || result.Commit != commitDurable || result.Action != configWritten {
		t.Fatalf("explicit result = %+v", result)
	}
	if lockCalls != 1 {
		t.Fatalf("lock calls = %d, want 1", lockCalls)
	}
	if _, err := os.Stat(b.LegacyPath); err != nil {
		t.Fatalf("explicit migration retired source: %v", err)
	}
	if _, err := os.Lstat(b.LegacyPath + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("explicit migration allocated backup: %v", err)
	}
}

func TestMigrationExplicit_RefusalDoesNotCreateConfigParentOrLock(t *testing.T) {
	base := t.TempDir()
	legacyPath := filepath.Join(base, "claunch", "claunch.conf")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "target"), legacyPath); err != nil {
		t.Fatal(err)
	}
	env := config.EnvSnapshot{Home: filepath.Join(base, "home"), XDGConfigHome: base, UserConfigHome: base}
	b, err := config.PrepareLegacyMigrationBoundary(env, config.NativeMigrationFS())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close() //nolint:errcheck
	result := migrateLegacyExplicit(b, nativeMigrationTxnOps())
	if result.Err == nil {
		t.Fatal("explicit refusal error = nil")
	}
	if _, err := os.Lstat(filepath.Dir(b.ConfigPath)); !os.IsNotExist(err) {
		t.Fatalf("config parent created on refusal: %v", err)
	}
	if _, err := os.Lstat(b.ConfigPath + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("lock created on refusal: %v", err)
	}
}

func TestMigrationParentDurabilityFailureStopsBeforeLockConfigBackup(t *testing.T) {
	base := t.TempDir()
	b := transactionBoundary(t, base, []byte("[defaults]\nmodel = \"sonnet\"\n"))
	ops := nativeMigrationTxnOps()
	wantErr := errors.New("injected parent durability")
	ops.ensureParent = func(string, configParentOps) ([]string, error) {
		return []string{filepath.Dir(b.ConfigPath)}, wantErr
	}
	lockCalls := 0
	ops.withLock = func(string, func() error) error { lockCalls++; return nil }
	result := migrateLegacyAutomatically(b, config.Config{}, ops)
	if !errors.Is(result.Err, wantErr) || result.Action != configUnchanged || result.Backup != backupNotAllocated {
		t.Fatalf("result=%+v", result)
	}
	if lockCalls != 0 {
		t.Fatalf("lock calls=%d, want zero", lockCalls)
	}
	for _, path := range []string{b.ConfigPath, b.ConfigPath + ".lock", b.LegacyPath + ".bak"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("parent failure mutated %q: %v", path, err)
		}
	}
	if _, err := os.Stat(b.LegacyPath); err != nil {
		t.Fatalf("source not retained: %v", err)
	}
}

func TestConfigScaffolds_BoundaryRefusalCreatesNoConfigNamespace(t *testing.T) {
	base := t.TempDir()
	legacyPath := filepath.Join(base, "claunch", "claunch.conf")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte("not [toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	boundary, err := config.PrepareLegacyMigrationBoundary(config.EnvSnapshot{Home: base, XDGConfigHome: base, UserConfigHome: base}, config.NativeMigrationFS())
	if err != nil {
		t.Fatal(err)
	}
	defer boundary.Close() //nolint:errcheck
	if boundary.Status != config.BoundaryRefused {
		t.Fatalf("boundary status=%v, want refused", boundary.Status)
	}

	commands := map[string]*cobra.Command{
		"launch init": newLaunchInitCmd(boundary),
		"top init":    newInitCmd(module.Deps{LegacyBoundary: boundary}),
	}
	for name, cmd := range commands {
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.Execute(); !errors.Is(err, config.ErrLegacyMalformed) {
			t.Errorf("%s error=%v, want malformed boundary refusal", name, err)
		}
	}
	for _, path := range []string{boundary.ConfigPath, boundary.ConfigPath + ".lock"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("refused scaffold mutated %q: %v", path, err)
		}
	}
	if _, err := os.Lstat(filepath.Dir(boundary.ConfigPath)); !os.IsNotExist(err) {
		t.Fatalf("refused scaffold created config parent: %v", err)
	}
}

func TestMigrationRefusal_ExplicitlyEmptyLaunchTableRemainsAuthoritative(t *testing.T) {
	base := t.TempDir()
	b := transactionBoundary(t, base, []byte("[defaults]\nmodel = \"sonnet\"\n"))
	if err := os.MkdirAll(filepath.Dir(b.ConfigPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.ConfigPath, []byte("[launch]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	startup := config.LoadPath(b.ConfigPath)
	ops := nativeMigrationTxnOps()
	ops.sourceRevalidate = func(*config.LegacySnapshot) error { return config.ErrLegacyDrift }
	result := migrateLegacyAutomatically(b, startup, ops)
	if result.Err == nil || !result.Effective.IsZero() {
		t.Fatalf("result = %+v, want refusal with empty config launch effective", result)
	}
	startup.Launch = result.Effective
	resolved, source := resolveLaunchConfig(b, startup, "")
	if !resolved.IsZero() || source != b.ConfigPath {
		t.Fatalf("resolved = %+v from %q, want authoritative empty config at %q", resolved, source, b.ConfigPath)
	}
}

func TestMigrationInitialCaptureFailureMatrix_ReplacementMutationAndDisappearanceRefuseBeforeConfigNamespace(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *config.LegacyMigrationBoundary)
	}{
		{name: "replacement", mutate: func(t *testing.T, b *config.LegacyMigrationBoundary) {
			old := b.LegacyPath + ".captured"
			if err := os.Rename(b.LegacyPath, old); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(b.LegacyPath, []byte("[defaults]\nmodel = \"opus\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "in place mutation", mutate: func(t *testing.T, b *config.LegacyMigrationBoundary) {
			if err := os.WriteFile(b.LegacyPath, []byte("[defaults]\nmodel = \"changed\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "disappearance", mutate: func(t *testing.T, b *config.LegacyMigrationBoundary) {
			if err := os.Remove(b.LegacyPath); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			b := transactionBoundary(t, base, []byte("[defaults]\nmodel = \"sonnet\"\n"))
			tt.mutate(t, b)
			result := migrateLegacyAutomatically(b, config.Config{}, nativeMigrationTxnOps())
			if result.Err == nil || result.Action != configUnchanged || result.Backup != backupNotAllocated {
				t.Fatalf("result=%+v, want pre-commit refusal", result)
			}
			if _, err := os.Lstat(filepath.Dir(b.ConfigPath)); !os.IsNotExist(err) {
				t.Fatalf("config namespace created on initial refusal: %v", err)
			}
		})
	}
}

func TestMigrationFailureMatrix_PostCommitFailuresKeepIntendedEffectiveAndSource(t *testing.T) {
	tests := []struct {
		name       string
		inject     func(*migrationTxnOps)
		wantBackup backupState
		wantRetire retirementState
	}{
		{name: "backup allocation", wantBackup: backupNotAllocated, wantRetire: retirementSourceRetained, inject: func(ops *migrationTxnOps) {
			ops.allocateBackup = func(*config.LegacySnapshot) (*config.BackupAllocation, error) {
				return nil, errors.New("injected allocation")
			}
		}},
		{name: "backup write", wantBackup: backupNotAllocated, wantRetire: retirementSourceRetained, inject: func(ops *migrationTxnOps) {
			ops.backupWrite = func(*config.BackupAllocation, []byte) error { return errors.New("injected write") }
		}},
		{name: "backup mode", wantBackup: backupNotAllocated, wantRetire: retirementSourceRetained, inject: func(ops *migrationTxnOps) {
			ops.backupMode = func(*config.BackupAllocation, os.FileMode) error { return errors.New("injected mode") }
		}},
		{name: "backup file sync", wantBackup: backupNotAllocated, wantRetire: retirementSourceRetained, inject: func(ops *migrationTxnOps) {
			ops.backupSyncFile = func(*config.BackupAllocation) error { return errors.New("injected file sync") }
		}},
		{name: "backup close", wantBackup: backupNotAllocated, wantRetire: retirementSourceRetained, inject: func(ops *migrationTxnOps) {
			ops.backupClose = func(*config.BackupAllocation) error { return errors.New("injected close") }
		}},
		{name: "backup parent sync", wantBackup: backupCompleteNotDurable, wantRetire: retirementSourceRetained, inject: func(ops *migrationTxnOps) {
			ops.backupSyncParent = func(*config.BackupAllocation) error { return errors.New("injected parent sync") }
		}},
		{name: "backup validation", wantBackup: backupDrifted, wantRetire: retirementSourceRetained, inject: func(ops *migrationTxnOps) {
			ops.backupValidate = func(*config.BackupAllocation, []byte) error { return errors.New("injected validation") }
		}},
		{name: "backup final revalidation", wantBackup: backupDrifted, wantRetire: retirementSourceRetained, inject: func(ops *migrationTxnOps) {
			ops.backupRevalidate = func(*config.BackupAllocation, []byte) error { return errors.New("injected revalidation") }
		}},
		{name: "source final revalidation", wantBackup: backupDurableVerified, wantRetire: retirementSourceRetained, inject: func(ops *migrationTxnOps) {
			native := ops.sourceRevalidate
			calls := 0
			ops.sourceRevalidate = func(source *config.LegacySnapshot) error {
				calls++
				if calls == 4 {
					return errors.New("injected final source validation")
				}
				return native(source)
			}
		}},
		{name: "source unlink", wantBackup: backupDurableVerified, wantRetire: retirementSourceRetained, inject: func(ops *migrationTxnOps) {
			ops.sourceUnlink = func(*config.LegacySnapshot) error { return errors.New("injected unlink") }
		}},
		{name: "source final parent sync", wantBackup: backupDurableVerified, wantRetire: retirementDurabilityUnknown, inject: func(ops *migrationTxnOps) {
			ops.sourceSyncParent = func(*config.LegacySnapshot) error { return errors.New("injected final sync") }
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			b := transactionBoundary(t, base, []byte("[defaults]\nmodel = \"sonnet\"\n"))
			ops := nativeMigrationTxnOps()
			tt.inject(&ops)
			result := migrateLegacyAutomatically(b, config.Config{}, ops)
			if result.Err == nil || result.Commit != commitDurable || result.Action != configWritten || result.Effective.Defaults.Model != "sonnet" {
				t.Fatalf("result=%+v, want durable intended effective plus injected error", result)
			}
			if result.Backup != tt.wantBackup || result.Retirement != tt.wantRetire {
				t.Fatalf("backup/retirement=%v/%v, want %v/%v", result.Backup, result.Retirement, tt.wantBackup, tt.wantRetire)
			}
			_, sourceErr := os.Lstat(b.LegacyPath)
			if tt.wantRetire == retirementDurabilityUnknown {
				if !os.IsNotExist(sourceErr) {
					t.Fatalf("source remains after successful unlink: %v", sourceErr)
				}
			} else if sourceErr != nil {
				t.Fatalf("source not retained: %v", sourceErr)
			}
		})
	}
}

func TestMigrationFailureMatrix_ShadowEffectiveFollowsCommitPhase(t *testing.T) {
	for _, tt := range []struct {
		name      string
		state     commitState
		wantAdded bool
	}{
		{name: "pre rename", state: commitNone, wantAdded: false},
		{name: "visible rename", state: commitRenamed, wantAdded: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			b := transactionBoundary(t, base, []byte("[defaults]\npermission_mode = \"plan\"\n"))
			if err := os.MkdirAll(filepath.Dir(b.ConfigPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(b.ConfigPath, []byte("[launch.defaults]\nmodel = \"opus\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			startup := config.LoadPath(b.ConfigPath)
			ops := nativeMigrationTxnOps()
			ops.writeConfig = func(string, []byte, atomicWriteOps) (commitState, error) {
				return tt.state, errors.New("injected commit phase")
			}
			result := migrateLegacyAutomatically(b, startup, ops)
			if result.Err == nil || result.Commit != tt.state || result.Effective.Defaults.Model != "opus" {
				t.Fatalf("result=%+v", result)
			}
			if gotAdded := result.Effective.Defaults.PermissionMode == "plan"; gotAdded != tt.wantAdded {
				t.Fatalf("effective permission added=%t, want %t for phase %v", gotAdded, tt.wantAdded, tt.state)
			}
		})
	}
}

func TestMigrationConcurrent_WaiterMissingSourceUsesWinnerConfigWithoutBackup(t *testing.T) {
	base := t.TempDir()
	b := transactionBoundary(t, base, []byte("[defaults]\nmodel = \"sonnet\"\n"))
	if err := os.MkdirAll(filepath.Dir(b.ConfigPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.ConfigPath, []byte("[launch.defaults]\nmodel = \"winner\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ops := nativeMigrationTxnOps()
	native := ops.sourceRevalidate
	calls := 0
	ops.sourceRevalidate = func(source *config.LegacySnapshot) error {
		calls++
		if calls == 3 {
			return config.ErrLegacySourceMissing
		}
		return native(source)
	}
	result := migrateLegacyAutomatically(b, config.LoadPath(b.ConfigPath), ops)
	if result.Err != nil || result.Effective.Defaults.Model != "winner" || result.Retirement != retirementRemoved || result.Backup != backupNotAllocated {
		t.Fatalf("waiter result=%+v", result)
	}
}

func TestMigrationMissingSourceWithoutAuthoritativeWinnerRefusesAndKeepsCapturedFallback(t *testing.T) {
	base := t.TempDir()
	b := transactionBoundary(t, base, []byte("[defaults]\nmodel = \"sonnet\"\n"))
	ops := nativeMigrationTxnOps()
	nativeRevalidate := ops.sourceRevalidate
	calls := 0
	ops.sourceRevalidate = func(source *config.LegacySnapshot) error {
		calls++
		if calls == 3 {
			if err := os.Remove(b.LegacyPath); err != nil {
				return err
			}
		}
		return nativeRevalidate(source)
	}

	result := migrateLegacyAutomatically(b, config.Config{}, ops)
	if !errors.Is(result.Err, config.ErrLegacySourceMissing) {
		t.Fatalf("error=%v, want missing-source refusal", result.Err)
	}
	if result.Effective.Defaults.Model != "sonnet" {
		t.Fatalf("effective=%+v, want captured fallback", result.Effective)
	}
	if result.Action != configUnchanged || result.Backup != backupNotAllocated || result.Retirement != retirementSourceMissingUnproved {
		t.Fatalf("unproved winner state=%+v", result)
	}
	if result.Notice == "" {
		t.Fatal("unproved winner refusal has no notice")
	}
}

func TestMigrationMissingSourceWithIncompleteLaunchDoesNotProveWinner(t *testing.T) {
	base := t.TempDir()
	legacy := []byte("[[project]]\nmatch = \"/tmp/project\"\nmodel = \"sonnet\"\n")
	b := transactionBoundary(t, base, legacy)
	if err := os.MkdirAll(filepath.Dir(b.ConfigPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.ConfigPath, []byte("[launch.defaults]\nmodel = \"opus\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ops := nativeMigrationTxnOps()
	nativeRevalidate := ops.sourceRevalidate
	calls := 0
	ops.sourceRevalidate = func(source *config.LegacySnapshot) error {
		calls++
		if calls == 3 {
			return config.ErrLegacySourceMissing
		}
		return nativeRevalidate(source)
	}

	result := migrateLegacyAutomatically(b, config.Config{}, ops)
	if !errors.Is(result.Err, config.ErrLegacySourceMissing) || result.Retirement != retirementSourceMissingUnproved {
		t.Fatalf("incomplete launch was accepted as winner: %+v", result)
	}
	if result.Effective.Defaults.Model != "opus" {
		t.Fatalf("effective=%+v, want authoritative shadow config", result.Effective)
	}
}

func TestMigrationTempSubstitutionNeverReachesBackupOrSourceRetirement(t *testing.T) {
	base := t.TempDir()
	b := transactionBoundary(t, base, []byte("[defaults]\nmodel = \"sonnet\"\n"))
	ops := nativeMigrationTxnOps()
	nativeRename := ops.atomic.rename
	ops.atomic.rename = func(oldPath, newPath string) error {
		if err := os.Remove(oldPath); err != nil {
			return err
		}
		if err := os.WriteFile(oldPath, []byte("attacker-controlled\n"), 0o644); err != nil {
			return err
		}
		if err := os.Chmod(oldPath, 0o644); err != nil {
			return err
		}
		return nativeRename(oldPath, newPath)
	}
	backupCalls := 0
	ops.allocateBackup = func(*config.LegacySnapshot) (*config.BackupAllocation, error) {
		backupCalls++
		return nil, errors.New("backup must not be reached")
	}

	result := migrateLegacyAutomatically(b, config.Config{}, ops)
	if result.Commit != commitRenamed || !errors.Is(result.Err, errConfigCommittedValidation) {
		t.Fatalf("result=%+v, want visible/non-durable validation failure", result)
	}
	if result.Backup != backupNotAllocated || result.Retirement != retirementSourceRetained || backupCalls != 0 {
		t.Fatalf("substitution advanced transaction: result=%+v backup_calls=%d", result, backupCalls)
	}
	if _, err := os.Stat(b.LegacyPath); err != nil {
		t.Fatalf("legacy source was not retained: %v", err)
	}
}

func TestBackupIdentity_ReplacementAfterDurableValidationRetainsSourceAndOccupant(t *testing.T) {
	base := t.TempDir()
	b := transactionBoundary(t, base, []byte("[defaults]\nmodel = \"sonnet\"\n"))
	ops := nativeMigrationTxnOps()
	native := ops.backupRevalidate
	calls := 0
	var occupantPath string
	ops.backupRevalidate = func(backup *config.BackupAllocation, expected []byte) error {
		calls++
		if calls == 1 {
			occupantPath = backup.Path
			replacement := filepath.Join(filepath.Dir(backup.Path), "replacement")
			if err := os.WriteFile(replacement, []byte("current occupant"), 0o600); err != nil {
				return err
			}
			if err := os.Rename(replacement, backup.Path); err != nil {
				return err
			}
		}
		return native(backup, expected)
	}
	result := migrateLegacyAutomatically(b, config.Config{}, ops)
	if !errors.Is(result.Err, config.ErrBackupDrift) || result.Backup != backupDrifted || result.Retirement != retirementSourceRetained {
		t.Fatalf("result=%+v", result)
	}
	if got, _ := os.ReadFile(occupantPath); string(got) != "current occupant" {
		t.Fatalf("occupant=%q", got)
	}
	if _, err := os.Stat(b.LegacyPath); err != nil {
		t.Fatalf("source not retained: %v", err)
	}
}

func TestBackupCleanupIdentity_ReplacementDuringPartialFailureIsNotRemoved(t *testing.T) {
	base := t.TempDir()
	b := transactionBoundary(t, base, []byte("[defaults]\nmodel = \"sonnet\"\n"))
	ops := nativeMigrationTxnOps()
	var occupantPath string
	ops.backupWrite = func(backup *config.BackupAllocation, _ []byte) error {
		occupantPath = backup.Path
		if err := backup.CloseWriter(); err != nil {
			return err
		}
		if err := os.Remove(backup.Path); err != nil {
			return err
		}
		if err := os.WriteFile(backup.Path, []byte("replacement"), 0o600); err != nil {
			return err
		}
		return errors.New("injected write after replacement")
	}
	result := migrateLegacyAutomatically(b, config.Config{}, ops)
	if result.Backup != backupPartialIdentityLost || !errors.Is(result.Err, config.ErrBackupIdentityLost) {
		t.Fatalf("result=%+v", result)
	}
	if got, _ := os.ReadFile(occupantPath); string(got) != "replacement" {
		t.Fatalf("replacement=%q", got)
	}
	if _, err := os.Stat(b.LegacyPath); err != nil {
		t.Fatalf("source not retained: %v", err)
	}
}

func TestMigrationTransaction_ExactOperationTrace(t *testing.T) {
	base := t.TempDir()
	b := transactionBoundary(t, base, []byte("[defaults]\nmodel = \"sonnet\"\n"))
	if string(b.Source.Data) != "[defaults]\nmodel = \"sonnet\"\n" || b.Source.Launch.Defaults.Model != "sonnet" {
		t.Fatal("authoritative capture/decode did not precede transaction")
	}
	ops := nativeMigrationTxnOps()
	var trace []string
	appendTrace := func(event string) { trace = append(trace, event) }

	nativeParent := ops.parent
	configParent := filepath.Dir(b.ConfigPath)
	ops.parent.stat = func(path string) (os.FileInfo, error) {
		if path == configParent {
			appendTrace("parent-stat-missing")
		} else {
			appendTrace("ancestor-stat")
		}
		return nativeParent.stat(path)
	}
	ops.parent.mkdir = func(path string, mode os.FileMode) error {
		appendTrace("parent-mkdir")
		return nativeParent.mkdir(path, mode)
	}
	ops.parent.syncDir = func(path string) error {
		if path == configParent {
			appendTrace("parent-sync-new")
		} else {
			appendTrace("ancestor-sync")
		}
		return nativeParent.syncDir(path)
	}
	nativeSource := ops.sourceRevalidate
	ops.sourceRevalidate = func(source *config.LegacySnapshot) error { appendTrace("source-check"); return nativeSource(source) }
	nativeLock := ops.withLock
	ops.withLock = func(path string, fn func() error) error { appendTrace("lock"); return nativeLock(path, fn) }
	nativeRead := ops.readFile
	ops.readFile = func(path string) ([]byte, error) { appendTrace("config-reread"); return nativeRead(path) }

	nativeAtomic := ops.atomic
	lstatEvents := []string{"config-lstat", "config-temp-owner-check", "config-destination-check"}
	lstatCalls := 0
	ops.atomic.lstat = func(path string) (os.FileInfo, error) {
		event := "config-unexpected-lstat"
		if lstatCalls < len(lstatEvents) {
			event = lstatEvents[lstatCalls]
		}
		lstatCalls++
		appendTrace(event)
		return nativeAtomic.lstat(path)
	}
	ops.atomic.createTemp = func(dir, pattern string) (*os.File, error) {
		appendTrace("config-temp")
		return nativeAtomic.createTemp(dir, pattern)
	}
	ops.atomic.writeAll = func(file *os.File, data []byte) error {
		appendTrace("config-write")
		return nativeAtomic.writeAll(file, data)
	}
	ops.atomic.chmodFile = func(file *os.File, mode os.FileMode) error {
		appendTrace("config-mode")
		return nativeAtomic.chmodFile(file, mode)
	}
	ops.atomic.syncFile = func(file *os.File) error { appendTrace("config-file-sync"); return nativeAtomic.syncFile(file) }
	ops.atomic.pinTemp = func(file *os.File) (*os.File, error) {
		appendTrace("config-pin")
		return nativeAtomic.pinTemp(file)
	}
	ops.atomic.closeFile = func(file *os.File) error { appendTrace("config-close"); return nativeAtomic.closeFile(file) }
	ops.atomic.rename = func(oldPath, newPath string) error {
		appendTrace("config-rename")
		return nativeAtomic.rename(oldPath, newPath)
	}
	ops.atomic.readAll = func(reader io.Reader) ([]byte, error) {
		appendTrace("config-validate-read")
		return nativeAtomic.readAll(reader)
	}
	ops.atomic.closePinned = func(file *os.File) error {
		appendTrace("config-pinned-close")
		return nativeAtomic.closePinned(file)
	}
	ops.atomic.syncDir = func(path string) error { appendTrace("config-parent-sync"); return nativeAtomic.syncDir(path) }

	nativeAllocate := ops.allocateBackup
	ops.allocateBackup = func(source *config.LegacySnapshot) (*config.BackupAllocation, error) {
		appendTrace("backup-allocate")
		return nativeAllocate(source)
	}
	nativeBackupWrite := ops.backupWrite
	ops.backupWrite = func(backup *config.BackupAllocation, data []byte) error {
		appendTrace("backup-write")
		return nativeBackupWrite(backup, data)
	}
	nativeBackupMode := ops.backupMode
	ops.backupMode = func(backup *config.BackupAllocation, mode os.FileMode) error {
		appendTrace("backup-mode")
		return nativeBackupMode(backup, mode)
	}
	nativeBackupSync := ops.backupSyncFile
	ops.backupSyncFile = func(backup *config.BackupAllocation) error {
		appendTrace("backup-file-sync")
		return nativeBackupSync(backup)
	}
	nativeBackupClose := ops.backupClose
	ops.backupClose = func(backup *config.BackupAllocation) error {
		appendTrace("backup-close")
		return nativeBackupClose(backup)
	}
	nativeBackupParent := ops.backupSyncParent
	ops.backupSyncParent = func(backup *config.BackupAllocation) error {
		appendTrace("backup-parent-sync")
		return nativeBackupParent(backup)
	}
	nativeBackupValidate := ops.backupValidate
	ops.backupValidate = func(backup *config.BackupAllocation, data []byte) error {
		appendTrace("backup-validate")
		return nativeBackupValidate(backup, data)
	}
	nativeBackupRevalidate := ops.backupRevalidate
	ops.backupRevalidate = func(backup *config.BackupAllocation, data []byte) error {
		appendTrace("backup-recheck")
		return nativeBackupRevalidate(backup, data)
	}
	nativeUnlink := ops.sourceUnlink
	ops.sourceUnlink = func(source *config.LegacySnapshot) error { appendTrace("source-unlink"); return nativeUnlink(source) }
	nativeSourceParent := ops.sourceSyncParent
	ops.sourceSyncParent = func(source *config.LegacySnapshot) error {
		appendTrace("source-parent-sync")
		return nativeSourceParent(source)
	}

	result := migrateLegacyAutomatically(b, config.Config{}, ops)
	if result.Err != nil {
		t.Fatalf("migration=%+v", result)
	}
	want := []string{
		"source-check", "parent-stat-missing", "ancestor-stat", "parent-mkdir", "parent-sync-new", "ancestor-sync", "source-check",
		"lock", "config-reread", "source-check", "config-lstat", "config-temp", "config-write", "config-mode", "config-file-sync", "config-pin", "config-close",
		"config-temp-owner-check", "config-rename", "config-destination-check", "config-validate-read", "config-pinned-close", "config-parent-sync",
		"backup-allocate", "backup-write", "backup-mode", "backup-file-sync", "backup-close", "backup-parent-sync", "backup-validate",
		"source-check", "backup-recheck", "source-check", "backup-recheck", "source-unlink", "source-parent-sync",
	}
	if strings.Join(trace, ",") != strings.Join(want, ",") {
		t.Fatalf("operation trace:\n got %v\nwant %v", trace, want)
	}
}

func TestIntegration_LaunchDarwinXDGAndControlRefusalMatrixHasZeroMutationAndSafeEffectiveState(t *testing.T) {
	controls := []string{"\n", "\r", "\r\n", "\x1b", "\x1b[2K", "\x1b]0;forged\x07", "\t", "\x7f", "\u009b"}
	for _, control := range controls {
		t.Run(termsafe.SafeLine(control), func(t *testing.T) {
			base := t.TempDir()
			xdg := filepath.Join(base, "foreign"+control+"config")
			legacyPath := filepath.Join(xdg, "claunch", "claunch.conf")
			if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(legacyPath, []byte("[defaults]\nmodel = \"sonnet\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			nativeBase := filepath.Join(base, "home", "Library", "Application Support")
			env := config.EnvSnapshot{Home: filepath.Join(base, "home"), XDGConfigHome: xdg, UserConfigHome: nativeBase}
			boundary, err := config.PrepareLegacyMigrationBoundary(env, config.NativeMigrationFS())
			if err != nil {
				t.Fatal(err)
			}
			defer boundary.Close() //nolint:errcheck
			if boundary.Status != config.BoundaryRefused || !errors.Is(boundary.Refusal, config.ErrLegacyPathControl) {
				t.Fatalf("status/refusal=%v/%v", boundary.Status, boundary.Refusal)
			}

			fallback, notice, _ := autoMigrateOrWarnLegacyLaunch(boundary, config.Config{})
			if fallback.Defaults.Model != "sonnet" || notice == "" || termsafe.SafeLine(notice) != notice {
				t.Fatalf("fallback=%+v notice=%q", fallback, notice)
			}
			shadowCfg, decodeErr := config.DecodeStrict([]byte("[launch.defaults]\nmodel = \"opus\"\n"))
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			shadow, shadowNotice, _ := autoMigrateOrWarnLegacyLaunch(boundary, shadowCfg)
			if shadow.Defaults.Model != "opus" || shadowNotice == "" || termsafe.SafeLine(shadowNotice) != shadowNotice {
				t.Fatalf("shadow=%+v notice=%q", shadow, shadowNotice)
			}
			explicit := migrateLegacyExplicit(boundary, nativeMigrationTxnOps())
			safeExplicit := termsafe.Error(explicit.Err)
			if !errors.Is(safeExplicit, config.ErrLegacyPathControl) || termsafe.SafeLine(safeExplicit.Error()) != safeExplicit.Error() {
				t.Fatalf("explicit error=%v", safeExplicit)
			}
			t.Setenv(skipLegacyMigrateEnv, "1")
			skipped, skipNotice, _ := autoMigrateOrWarnLegacyLaunch(boundary, config.Config{})
			if skipped.Defaults.Model != "sonnet" || skipNotice != "" {
				t.Fatalf("skip fallback=%+v notice=%q", skipped, skipNotice)
			}

			for _, path := range []string{boundary.ConfigPath, boundary.ConfigPath + ".lock", boundary.LegacyPath + ".bak"} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("refusal mutated %q: %v", termsafe.QuotePath(path), err)
				}
			}
			if got, err := os.ReadFile(boundary.LegacyPath); err != nil || !strings.Contains(string(got), "sonnet") {
				t.Fatalf("source changed=%q error=%v", got, err)
			}
		})
	}
}

func TestIntegration_LaunchDarwinForeignXDGPairRefusesWithoutMutation(t *testing.T) {
	base := t.TempDir()
	xdg := filepath.Join(base, "foreign-config")
	legacyPath := filepath.Join(xdg, "claunch", "claunch.conf")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte("[defaults]\nmodel = \"sonnet\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nativeBase := filepath.Join(base, "home", "Library", "Application Support")
	boundary, err := config.PrepareLegacyMigrationBoundary(config.EnvSnapshot{
		Home: filepath.Join(base, "home"), XDGConfigHome: xdg, UserConfigHome: nativeBase,
	}, config.NativeMigrationFS())
	if err != nil {
		t.Fatal(err)
	}
	defer boundary.Close() //nolint:errcheck
	if boundary.Status != config.BoundaryRefused || !errors.Is(boundary.Refusal, config.ErrLegacyPathPolicy) {
		t.Fatalf("status/refusal=%v/%v", boundary.Status, boundary.Refusal)
	}
	result := migrateLegacyAutomatically(boundary, config.Config{}, nativeMigrationTxnOps())
	if !errors.Is(result.Err, config.ErrLegacyPathPolicy) || result.Effective.Defaults.Model != "sonnet" {
		t.Fatalf("automatic result=%+v, want read-only fallback", result)
	}
	for _, path := range []string{boundary.ConfigPath, boundary.ConfigPath + ".lock", boundary.LegacyPath + ".bak"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("pair refusal mutated %q: %v", path, err)
		}
	}
}

// TestMigrationTransaction_RefusesUnrepresentableFields is forgectl#417's core
// contract. A legacy file carrying fields forgectl cannot represent must not
// be rendered, backed up, or retired: rendering [launch] from the modelled
// subset alone silently drops the rest, and the source that held it is then
// deleted. The gate sits ahead of the whole transaction, so nothing is
// written at all.
func TestMigrationTransaction_RefusesUnrepresentableFields(t *testing.T) {
	base := t.TempDir()
	legacy := []byte("[defaults]\nmodel = \"sonnet\"\nunknown_field = \"x\"\n")
	b := transactionBoundary(t, base, legacy)

	result := migrateLegacyAutomatically(b, config.Config{}, nativeMigrationTxnOps())

	if !errors.Is(result.Err, config.ErrLegacyUnsupportedFields) {
		t.Fatalf("result.Err = %v, want it to wrap ErrLegacyUnsupportedFields", result.Err)
	}
	if !strings.Contains(result.Notice, "unknown_field") {
		t.Errorf("notice = %q, want it to name the field forgectl cannot represent", result.Notice)
	}
	if result.Action != configUnchanged || result.Backup != backupNotAllocated || result.Retirement != retirementSourceRetained {
		t.Fatalf("states = action:%v backup:%v retirement:%v, want unchanged/not-allocated/retained", result.Action, result.Backup, result.Retirement)
	}
	if got, err := os.ReadFile(b.LegacyPath); err != nil || string(got) != string(legacy) {
		t.Fatalf("legacy source = %q, error %v; want it retained byte-identical", got, err)
	}
	if _, err := os.Lstat(b.ConfigPath); !os.IsNotExist(err) {
		t.Fatalf("config.toml was written despite the refusal: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Join(base, "claunch")); err == nil {
		for _, e := range entries {
			if strings.Contains(e.Name(), ".bak") {
				t.Fatalf("a backup was allocated despite the refusal: %s", e.Name())
			}
		}
	}
	// The read stays lenient: launch must still see the fields it does model.
	if result.Effective.Defaults.Model != "sonnet" {
		t.Errorf("effective model = %q, want sonnet — a refusal must not blank the profile", result.Effective.Defaults.Model)
	}
}

// TestMigrationTransaction_ExplicitRefusesUnrepresentableFields covers the
// on-demand surface. `launch migrate` never unlinks, but it does render
// [launch] from a partial decode — the same silent loss, one command earlier.
func TestMigrationTransaction_ExplicitRefusesUnrepresentableFields(t *testing.T) {
	base := t.TempDir()
	b := transactionBoundary(t, base, []byte("[defaults]\nmodel = \"sonnet\"\nunknown_field = \"x\"\n"))

	result := migrateLegacyExplicit(b, nativeMigrationTxnOps())

	if !errors.Is(result.Err, config.ErrLegacyUnsupportedFields) {
		t.Fatalf("result.Err = %v, want it to wrap ErrLegacyUnsupportedFields", result.Err)
	}
	if result.Action != configUnchanged {
		t.Errorf("action = %v, want configUnchanged", result.Action)
	}
	if _, err := os.Lstat(b.ConfigPath); !os.IsNotExist(err) {
		t.Fatalf("config.toml was written despite the refusal: %v", err)
	}
}

// TestMigrationTransaction_SupersededNoticeNamesTheBackup pins the second half
// of #417: "legacy config fully superseded, removed." asserted a deletion and
// named no recovery path, unlike both of its sibling notices.
func TestMigrationTransaction_SupersededNoticeNamesTheBackup(t *testing.T) {
	base := t.TempDir()
	b := transactionBoundary(t, base, []byte("[defaults]\nmodel = \"sonnet\"\n"))
	configPath := filepath.Join(base, "forgectl", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	// A [launch] section that already carries everything the legacy file has,
	// so MergeLegacyIntoLaunch adds nothing and the `added == 0` arm runs.
	if err := os.WriteFile(configPath, []byte("[launch.defaults]\nmodel = \"sonnet\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Launch: config.LaunchConfig{Defaults: config.LaunchDefaults{Model: "sonnet"}}}

	result := migrateLegacyAutomatically(b, cfg, nativeMigrationTxnOps())
	if result.Err != nil {
		t.Fatalf("migration error = %v", result.Err)
	}
	if result.Retirement != retirementRemoved {
		t.Fatalf("retirement = %v, want retirementRemoved", result.Retirement)
	}
	if result.BackupPath == "" {
		t.Fatal("BackupPath is empty on a completed retirement")
	}
	if !strings.Contains(result.Notice, filepath.Base(result.BackupPath)) {
		t.Errorf("notice = %q, want it to name the backup %q — a message asserting removal must carry the recovery pointer",
			result.Notice, filepath.Base(result.BackupPath))
	}
}

// TestMigrationTransaction_RefusalCreatesNoConfigDirectory pins the refusal as
// a true no-op. migrateLegacyAutomatically calls ensureParent — which mkdirs
// every missing ancestor of the config path — before it ever reaches the
// locked transaction, so a gate placed only inside migrateLocked would still
// leave a config directory behind on a first-ever run against a legacy file
// forgectl declined to migrate.
func TestMigrationTransaction_RefusalCreatesNoConfigDirectory(t *testing.T) {
	base := t.TempDir()
	b := transactionBoundary(t, base, []byte("[defaults]\nmodel = \"sonnet\"\nunknown_field = \"x\"\n"))
	if _, err := os.Lstat(filepath.Dir(b.ConfigPath)); !os.IsNotExist(err) {
		t.Fatalf("fixture already has a config parent at %q — the assertion below would pass vacuously", filepath.Dir(b.ConfigPath))
	}

	result := migrateLegacyAutomatically(b, config.Config{}, nativeMigrationTxnOps())

	if !errors.Is(result.Err, config.ErrLegacyUnsupportedFields) {
		t.Fatalf("result.Err = %v, want it to wrap ErrLegacyUnsupportedFields", result.Err)
	}
	if _, err := os.Lstat(filepath.Dir(b.ConfigPath)); !os.IsNotExist(err) {
		t.Errorf("config parent %q was created despite the refusal — a declined migration must leave nothing behind", filepath.Dir(b.ConfigPath))
	}
}
