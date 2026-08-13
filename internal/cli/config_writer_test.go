package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

func TestConfigWriterLock_ConcurrentRereadModifyWritePreservesBothUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	ops := nativeConfigWriterOps()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, section := range []string{"[bench]\ntelemetry = true\n", "[net]\nprovider = \"tailscale\"\n"} {
		wg.Add(1)
		go func(i int, section string) {
			defer wg.Done()
			_, errs[i] = updateConfigLocked(path, ops, func(raw []byte) ([]byte, error) {
				return append(bytes.Clone(raw), []byte("\n"+section)...), nil
			})
		}(i, section)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d error = %v", i, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[bench]", "[net]"} {
		if strings.Count(string(data), want) != 1 {
			t.Fatalf("config missing exactly one %s:\n%s", want, data)
		}
	}
}

func twoPartyLockBarrier() func(string, func() error) error {
	var mu sync.Mutex
	arrived := 0
	release := make(chan struct{})
	return func(path string, fn func() error) error {
		mu.Lock()
		arrived++
		if arrived == 2 {
			close(release)
		}
		mu.Unlock()
		<-release
		return config.WithFileLock(path, fn)
	}
}

func secondTransactionBoundary(t *testing.T, first *config.LegacyMigrationBoundary) *config.LegacyMigrationBoundary {
	t.Helper()
	base := filepath.Dir(filepath.Dir(first.LegacyPath))
	boundary, err := config.PrepareLegacyMigrationBoundary(config.EnvSnapshot{Home: filepath.Join(base, "home"), XDGConfigHome: base, UserConfigHome: base}, config.NativeMigrationFS())
	if err != nil {
		t.Fatal(err)
	}
	if boundary.Status != config.BoundaryMigratable {
		boundary.Close() //nolint:errcheck
		t.Fatalf("second boundary status=%v refusal=%v", boundary.Status, boundary.Refusal)
	}
	t.Cleanup(func() { _ = boundary.Close() })
	return boundary
}

func runLaunchInitWriter(path string, lock func(string, func() error) error) error {
	ops := nativeConfigWriterOps()
	ops.withLock = lock
	_, err := updateConfigLocked(path, ops, func(raw []byte) ([]byte, error) {
		if hasLaunchSection(raw) {
			return nil, fmt.Errorf("launch already present")
		}
		return append(raw, []byte(launchScaffold)...), nil
	})
	return err
}

func runTopInitWriter(path string, lock func(string, func() error) error) error {
	ops := nativeConfigWriterOps()
	ops.withLock = lock
	_, err := updateConfigLocked(path, ops, func(raw []byte) ([]byte, error) {
		data := bytes.Clone(raw)
		for _, section := range initSections {
			if hasSection(data, section.name) {
				continue
			}
			if section.name == "" {
				data = append([]byte(section.template), data...)
			} else {
				data = append(data, []byte(section.template)...)
			}
		}
		return data, nil
	})
	return err
}

func TestConfigWriteLock_BarrierControlledWriterPairMatrixHasOnlyLegalSerialResults(t *testing.T) {
	tests := []struct {
		name       string
		left       string
		right      string
		retire     bool
		needSecond bool
	}{
		{name: "automatic vs automatic", left: "automatic", right: "automatic", retire: true, needSecond: true},
		{name: "automatic vs explicit", left: "automatic", right: "explicit", retire: true, needSecond: true},
		{name: "automatic vs launch init", left: "automatic", right: "launch-init", retire: true},
		{name: "automatic vs top init", left: "automatic", right: "top-init", retire: true},
		{name: "explicit vs launch init", left: "explicit", right: "launch-init", needSecond: true},
		{name: "top init vs top init", left: "top-init", right: "top-init"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			legacy := []byte("[[project]]\nmatch = \"/tmp/project\"\nmodel = \"sonnet\"\n")
			first := transactionBoundary(t, base, legacy)
			if err := os.MkdirAll(filepath.Dir(first.ConfigPath), 0o700); err != nil {
				t.Fatal(err)
			}
			const existing = "# unrelated-sentinel\n[bench]\ntelemetry = true\n"
			if err := os.WriteFile(first.ConfigPath, []byte(existing), 0o600); err != nil {
				t.Fatal(err)
			}
			var second *config.LegacyMigrationBoundary
			if tt.needSecond {
				second = secondTransactionBoundary(t, first)
			}
			barrier := twoPartyLockBarrier()
			var mu sync.Mutex
			var automaticResults []MigrationResult
			run := func(kind string, boundary *config.LegacyMigrationBoundary) error {
				switch kind {
				case "automatic":
					ops := nativeMigrationTxnOps()
					ops.withLock = barrier
					result := migrateLegacyAutomatically(boundary, config.Config{}, ops)
					mu.Lock()
					automaticResults = append(automaticResults, result)
					mu.Unlock()
					return result.Err
				case "explicit":
					ops := nativeMigrationTxnOps()
					ops.withLock = barrier
					return migrateLegacyExplicit(boundary, ops).Err
				case "launch-init":
					return runLaunchInitWriter(first.ConfigPath, barrier)
				case "top-init":
					return runTopInitWriter(first.ConfigPath, barrier)
				default:
					panic(kind)
				}
			}
			leftBoundary := first
			rightBoundary := first
			if second != nil {
				if tt.left == "automatic" && tt.right == "explicit" {
					rightBoundary = second
				} else if tt.left == "explicit" {
					leftBoundary = second
				} else {
					rightBoundary = second
				}
			}
			errs := make([]error, 2)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); errs[0] = run(tt.left, leftBoundary) }()
			go func() { defer wg.Done(); errs[1] = run(tt.right, rightBoundary) }()
			wg.Wait()

			for _, result := range automaticResults {
				if result.Err != nil {
					t.Fatalf("automatic result=%+v; pair errors=%v", result, errs)
				}
			}
			raw, err := os.ReadFile(first.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := config.DecodeStrict(raw); err != nil {
				t.Fatalf("serialized config is invalid TOML: %v\n%s", err, raw)
			}
			if strings.Count(string(raw), "# unrelated-sentinel") != 1 || strings.Count(string(raw), "\n[bench]\n") != 1 {
				t.Fatalf("unrelated preexisting bytes were not preserved exactly once:\n%s", raw)
			}
			if count := len(regexp.MustCompile(`(?m)^\s*\[launch\.defaults\]\s*$`).FindAll(raw, -1)); count != 1 {
				t.Fatalf("launch defaults count != 1:\n%s", raw)
			}
			backups, err := filepath.Glob(first.LegacyPath + ".bak*")
			if err != nil {
				t.Fatal(err)
			}
			if tt.retire {
				if _, err := os.Lstat(first.LegacyPath); !os.IsNotExist(err) {
					t.Fatalf("automatic pair did not retire source: %v", err)
				}
				if len(backups) != 1 {
					t.Fatalf("backups=%v, want exactly one", backups)
				}
			} else {
				if _, err := os.Stat(first.LegacyPath); err != nil {
					t.Fatalf("non-automatic pair changed source: %v", err)
				}
				if len(backups) != 0 {
					t.Fatalf("non-automatic pair created backups=%v", backups)
				}
			}
		})
	}
}

func TestConfigWriterLock_RereadsInsideLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("log_level = \"off\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ops := nativeConfigWriterOps()
	nativeLock := ops.withLock
	ops.withLock = func(path string, fn func() error) error {
		return nativeLock(path, func() error {
			if err := os.WriteFile(path, []byte("no_icons = true\n"), 0o600); err != nil {
				return err
			}
			return fn()
		})
	}
	_, err := updateConfigLocked(path, ops, func(raw []byte) ([]byte, error) {
		if !bytes.Contains(raw, []byte("no_icons = true")) {
			return nil, fmt.Errorf("render received stale pre-lock bytes: %q", raw)
		}
		return append(raw, []byte("[bench]\ntelemetry = true\n")...), nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
