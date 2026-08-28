//go:build unix

package config

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func boundaryFixture(t *testing.T, body []byte) (EnvSnapshot, string, string) {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "forgectl-263-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	env := EnvSnapshot{Home: filepath.Join(base, "home"), XDGConfigHome: base, UserConfigHome: base}
	configPath := filepath.Join(base, "forgectl", "config.toml")
	legacyPath := filepath.Join(base, "claunch", "claunch.conf")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if body != nil {
		if err := os.WriteFile(legacyPath, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return env, configPath, legacyPath
}

func TestBoundary_CapturesAndDecodesOneImmutablePayload(t *testing.T) {
	body := []byte("[defaults]\nmodel = \"sonnet\"\n")
	env, configPath, legacyPath := boundaryFixture(t, body)

	b, err := PrepareLegacyMigrationBoundary(env, NativeMigrationFS())
	if err != nil {
		t.Fatalf("PrepareLegacyMigrationBoundary() error = %v", err)
	}
	defer b.Close()
	if b.Status != BoundaryMigratable {
		t.Fatalf("Status = %v, want BoundaryMigratable (refusal=%v)", b.Status, b.Refusal)
	}
	if b.ConfigPath != configPath || b.LegacyPath != legacyPath {
		t.Fatalf("paths = (%q, %q), want (%q, %q)", b.ConfigPath, b.LegacyPath, configPath, legacyPath)
	}
	if got := string(b.Source.Data); got != string(body) {
		t.Fatalf("captured data = %q, want exact %q", got, body)
	}
	if b.Source.Launch.Defaults.Model != "sonnet" {
		t.Fatalf("decoded model = %q, want sonnet", b.Source.Launch.Defaults.Model)
	}
	if err := b.Source.Revalidate(); err != nil {
		t.Fatalf("Revalidate() error = %v", err)
	}
}

func TestBoundary_RefusesMalformedAndNonregularBeforeConfigNamespaceMutation(t *testing.T) {
	tests := []struct {
		name string
		make func(t *testing.T, path string)
		want error
	}{
		{
			name: "malformed regular",
			make: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("not [toml"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrLegacyMalformed,
		},
		{
			name: "symlink",
			make: func(t *testing.T, path string) {
				t.Helper()
				target := filepath.Join(filepath.Dir(path), "target")
				if err := os.WriteFile(target, []byte("[defaults]\nmodel = \"opus\"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrLegacyNonRegular,
		},
		{
			name: "directory",
			make: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrLegacyNonRegular,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, configPath, legacyPath := boundaryFixture(t, nil)
			tt.make(t, legacyPath)
			b, err := PrepareLegacyMigrationBoundary(env, NativeMigrationFS())
			if err != nil {
				t.Fatalf("PrepareLegacyMigrationBoundary() error = %v", err)
			}
			defer b.Close()
			if b.Status != BoundaryRefused || !errors.Is(b.Refusal, tt.want) {
				t.Fatalf("status/refusal = %v/%v, want refused/%v", b.Status, b.Refusal, tt.want)
			}
			if _, err := os.Lstat(filepath.Dir(configPath)); !os.IsNotExist(err) {
				t.Fatalf("config parent was mutated on refusal: %v", err)
			}
		})
	}
}

func TestBoundary_NoSourceBypassesInvalidPairWithoutMutation(t *testing.T) {
	base := t.TempDir()
	env := EnvSnapshot{Home: filepath.Join(base, "home"), XDGConfigHome: "relative", UserConfigHome: filepath.Join(base, "native")}
	b, err := PrepareLegacyMigrationBoundary(env, NativeMigrationFS())
	if err != nil {
		t.Fatalf("PrepareLegacyMigrationBoundary() error = %v", err)
	}
	defer b.Close()
	if b.Status != BoundaryNoSource {
		t.Fatalf("Status = %v, want BoundaryNoSource", b.Status)
	}
	if _, err := os.Lstat(filepath.Join(base, "native", "forgectl")); !os.IsNotExist(err) {
		t.Fatalf("config parent was mutated: %v", err)
	}
}

func TestBoundary_SpecialFilesRefuseWithoutBlockingOrCreatingConfigParent(t *testing.T) {
	tests := []struct {
		name string
		make func(*testing.T, string)
	}{
		{name: "fifo", make: func(t *testing.T, path string) {
			if err := unix.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unix socket", make: func(t *testing.T, path string) {
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				if errors.Is(err, unix.EPERM) {
					t.Skipf("sandbox refuses Unix sockets: %v", err)
				}
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, configPath, legacyPath := boundaryFixture(t, nil)
			tt.make(t, legacyPath)
			done := make(chan *LegacyMigrationBoundary, 1)
			go func() {
				boundary, _ := PrepareLegacyMigrationBoundary(env, NativeMigrationFS())
				done <- boundary
			}()
			select {
			case boundary := <-done:
				if boundary == nil {
					t.Fatal("boundary is nil")
				}
				defer boundary.Close() //nolint:errcheck
				if boundary.Status != BoundaryRefused || !errors.Is(boundary.Refusal, ErrLegacyNonRegular) {
					t.Fatalf("status/refusal=%v/%v, want nonregular refusal", boundary.Status, boundary.Refusal)
				}
			case <-time.After(time.Second):
				t.Fatal("special-file boundary probe blocked")
			}
			if _, err := os.Lstat(filepath.Dir(configPath)); !os.IsNotExist(err) {
				t.Fatalf("config parent was mutated: %v", err)
			}
		})
	}
}

type nonregularTestProbe struct{}

func (nonregularTestProbe) Exists() bool                      { return true }
func (nonregularTestProbe) Regular() bool                     { return false }
func (nonregularTestProbe) Capture() (*LegacySnapshot, error) { panic("nonregular capture called") }
func (nonregularTestProbe) Close() error                      { return nil }

type deviceClassTestFS struct{}

func (deviceClassTestFS) probe(string) (legacyProbe, error) { return nonregularTestProbe{}, nil }
func (deviceClassTestFS) loadReadOnly(string) (LaunchConfig, error) {
	return LaunchConfig{}, ErrLegacyNonRegular
}

type captureBarrierFS struct {
	barrier func(*unixLegacyProbe)
}

func (fs captureBarrierFS) probe(path string) (legacyProbe, error) {
	probe, err := (nativeMigrationFS{}).probe(path)
	if err != nil {
		return nil, err
	}
	if unixProbe, ok := probe.(*unixLegacyProbe); ok {
		fs.barrier(unixProbe)
	}
	return probe, nil
}
func (captureBarrierFS) loadReadOnly(string) (LaunchConfig, error) {
	return LaunchConfig{}, ErrNoLegacyLaunch
}
func (captureBarrierFS) mutationSupported() bool { return true }

func TestBoundary_CaptureIdentityAndStableReadBarrierMatrixRefuses(t *testing.T) {
	tests := []struct {
		name    string
		barrier func(*unixLegacyProbe, string)
	}{
		{name: "replacement between fstatat and openat", barrier: func(probe *unixLegacyProbe, path string) {
			probe.beforeOpen = func() {
				_ = os.Rename(path, path+".captured")
				_ = os.WriteFile(path, []byte("[defaults]\nmodel = \"replacement\"\n"), 0o600)
			}
		}},
		{name: "disappearance between fstatat and openat", barrier: func(probe *unixLegacyProbe, path string) {
			probe.beforeOpen = func() { _ = os.Remove(path) }
		}},
		{name: "replacement between openat and identity confirmation", barrier: func(probe *unixLegacyProbe, path string) {
			probe.afterOpen = func() {
				_ = os.Rename(path, path+".captured")
				_ = os.WriteFile(path, []byte("[defaults]\nmodel = \"replacement\"\n"), 0o600)
			}
		}},
		{name: "in place mutation between comparison reads", barrier: func(probe *unixLegacyProbe, path string) {
			probe.betweenReads = func() { _ = os.WriteFile(path, []byte("[defaults]\nmodel = \"changed\"\n"), 0o600) }
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, configPath, legacyPath := boundaryFixture(t, []byte("[defaults]\nmodel = \"sonnet\"\n"))
			fs := captureBarrierFS{barrier: func(probe *unixLegacyProbe) { tt.barrier(probe, legacyPath) }}
			boundary, err := PrepareLegacyMigrationBoundary(env, fs)
			if err != nil {
				t.Fatal(err)
			}
			defer boundary.Close() //nolint:errcheck
			if boundary.Status != BoundaryRefused || !errors.Is(boundary.Refusal, ErrLegacyDrift) {
				t.Fatalf("status/refusal=%v/%v, want drift refusal", boundary.Status, boundary.Refusal)
			}
			if _, err := os.Lstat(filepath.Dir(configPath)); !os.IsNotExist(err) {
				t.Fatalf("capture barrier created config parent: %v", err)
			}
		})
	}
}
func (deviceClassTestFS) mutationSupported() bool { return true }

func TestBoundary_InjectedDeviceClassificationRefusesBeforeCapture(t *testing.T) {
	base := t.TempDir()
	env := EnvSnapshot{Home: base, XDGConfigHome: base, UserConfigHome: base}
	boundary, err := PrepareLegacyMigrationBoundary(env, deviceClassTestFS{})
	if err != nil {
		t.Fatal(err)
	}
	if boundary.Status != BoundaryRefused || !errors.Is(boundary.Refusal, ErrLegacyNonRegular) {
		t.Fatalf("status/refusal=%v/%v, want nonregular refusal", boundary.Status, boundary.Refusal)
	}
	if _, err := os.Lstat(filepath.Dir(boundary.ConfigPath)); !os.IsNotExist(err) {
		t.Fatalf("config parent was mutated: %v", err)
	}
}

// TestBoundary_CarriesUndecodedKeys pins the forgectl#417 contract at its
// source: a legacy file carrying fields forgectl cannot represent must expose
// them on the snapshot, so the migration transaction can refuse to render,
// back up, or retire a file it only partly understood. Decoding silently and
// dropping the remainder is the lossy-supersession bug.
func TestBoundary_CarriesUndecodedKeys(t *testing.T) {
	body := []byte("[defaults]\nmodel = \"sonnet\"\nunknown_field = \"x\"\n\n[gateway]\ntoken = \"y\"\n")
	env, _, _ := boundaryFixture(t, body)

	b, err := PrepareLegacyMigrationBoundary(env, NativeMigrationFS())
	if err != nil {
		t.Fatalf("PrepareLegacyMigrationBoundary() error = %v", err)
	}
	defer b.Close()
	if b.Status != BoundaryMigratable {
		t.Fatalf("Status = %v, want BoundaryMigratable (refusal=%v)", b.Status, b.Refusal)
	}
	if b.Source.Launch.Defaults.Model != "sonnet" {
		t.Fatalf("decoded model = %q, want sonnet", b.Source.Launch.Defaults.Model)
	}
	got := b.Source.UndecodedKeys
	want := []string{"defaults.unknown_field", "gateway", "gateway.token"}
	if len(got) != len(want) {
		t.Fatalf("UndecodedKeys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("UndecodedKeys = %v, want %v (sorted)", got, want)
		}
	}
}

// TestBoundary_NoUndecodedKeysOnAFullyModelledFile is the control: without it
// the test above could pass against an implementation that reports every key
// as undecoded.
func TestBoundary_NoUndecodedKeysOnAFullyModelledFile(t *testing.T) {
	env, _, _ := boundaryFixture(t, []byte("[defaults]\nmodel = \"sonnet\"\n\n[[project]]\nmatch = \"/tmp\"\n"))

	b, err := PrepareLegacyMigrationBoundary(env, NativeMigrationFS())
	if err != nil {
		t.Fatalf("PrepareLegacyMigrationBoundary() error = %v", err)
	}
	defer b.Close()
	if len(b.Source.UndecodedKeys) != 0 {
		t.Fatalf("UndecodedKeys = %v, want none on a fully modelled file", b.Source.UndecodedKeys)
	}
}

// TestBoundary_UnmigratableSiblingIsDerivedFromTheLegacyDirectory pins the one
// way this probe can be silently wrong (#417). The config base and the legacy
// base diverge whenever XDG_CONFIG_HOME is unset — that is the default on
// darwin — so a probe derived from the config directory would report absent on
// the exact file the probe exists to name.
func TestBoundary_UnmigratableSiblingIsDerivedFromTheLegacyDirectory(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "forgectl-417-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	env := EnvSnapshot{Home: base, UserConfigHome: filepath.Join(base, "native")}
	legacyDir := filepath.Join(base, ".config", "claunch")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(legacyDir, "config.toml")
	if err := os.WriteFile(sibling, []byte("[defaults]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A decoy in the config directory: a configDir-derived probe would find
	// this one and report the wrong path.
	configDir := filepath.Join(base, "native", "forgectl")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("log_level = \"off\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	b, err := PrepareLegacyMigrationBoundary(env, NativeMigrationFS())
	if err != nil {
		t.Fatalf("PrepareLegacyMigrationBoundary() error = %v", err)
	}
	defer b.Close()
	if b.Status != BoundaryNoSource {
		t.Fatalf("Status = %v, want BoundaryNoSource (no claunch.conf was written)", b.Status)
	}
	if filepath.Dir(b.ConfigPath) == filepath.Dir(b.LegacyPath) {
		t.Fatalf("fixture does not diverge: config dir and legacy dir are both %q", filepath.Dir(b.ConfigPath))
	}
	if got := b.UnmigratableSiblingPath(); got != sibling {
		t.Fatalf("UnmigratableSiblingPath() = %q, want %q (the legacy directory's sibling)", got, sibling)
	}
}

// TestBoundary_NoUnmigratableSiblingWhenAbsent is the control for the probe.
func TestBoundary_NoUnmigratableSiblingWhenAbsent(t *testing.T) {
	env, _, _ := boundaryFixture(t, []byte("[defaults]\nmodel = \"sonnet\"\n"))
	b, err := PrepareLegacyMigrationBoundary(env, NativeMigrationFS())
	if err != nil {
		t.Fatalf("PrepareLegacyMigrationBoundary() error = %v", err)
	}
	defer b.Close()
	if got := b.UnmigratableSiblingPath(); got != "" {
		t.Fatalf("UnmigratableSiblingPath() = %q, want \"\" with no sibling present", got)
	}
}
