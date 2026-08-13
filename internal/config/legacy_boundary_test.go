package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func TestBoundary_ExactXDGPairPolicy(t *testing.T) {
	tests := []struct {
		name    string
		goos    string
		env     EnvSnapshot
		wantCfg string
		wantOld string
		wantErr error
	}{
		{
			name:    "linux explicit absolute XDG",
			goos:    "linux",
			env:     EnvSnapshot{Home: "/home/alice", XDGConfigHome: "/srv/config/../config"},
			wantCfg: "/srv/config/forgectl/config.toml",
			wantOld: "/srv/config/claunch/claunch.conf",
		},
		{
			name:    "linux cleaned equivalent absolute XDG",
			goos:    "linux",
			env:     EnvSnapshot{Home: "/home/alice", XDGConfigHome: "/srv/one/../config", UserConfigHome: "/srv/config"},
			wantCfg: "/srv/config/forgectl/config.toml",
			wantOld: "/srv/config/claunch/claunch.conf",
		},
		{
			name:    "linux relative XDG",
			goos:    "linux",
			env:     EnvSnapshot{Home: "/home/alice", XDGConfigHome: "relative"},
			wantErr: ErrLegacyPathPolicy,
		},
		{
			name:    "darwin historical pair without XDG",
			goos:    "darwin",
			env:     EnvSnapshot{Home: "/tmp/alice"},
			wantCfg: "/tmp/alice/Library/Application Support/forgectl/config.toml",
			wantOld: "/tmp/alice/.config/claunch/claunch.conf",
		},
		{
			name:    "darwin explicit foreign XDG refuses split pair",
			goos:    "darwin",
			env:     EnvSnapshot{Home: "/tmp/alice", XDGConfigHome: "/tmp/config"},
			wantErr: ErrLegacyPathPolicy,
		},
		{
			name: "darwin explicit native base is accepted",
			goos: "darwin",
			env: EnvSnapshot{
				Home:          "/tmp/alice",
				XDGConfigHome: "/tmp/alice/Library/Application Support",
			},
			wantCfg: "/tmp/alice/Library/Application Support/forgectl/config.toml",
			wantOld: "/tmp/alice/Library/Application Support/claunch/claunch.conf",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, legacy, err := resolveLegacyMigrationPaths(tt.env, tt.goos)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("resolveLegacyMigrationPaths() error = %v, want %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if cfg != filepath.Clean(tt.wantCfg) || legacy != filepath.Clean(tt.wantOld) {
				t.Fatalf("paths = (%q, %q), want (%q, %q)", cfg, legacy, filepath.Clean(tt.wantCfg), filepath.Clean(tt.wantOld))
			}
		})
	}
}

type probeErrorFS struct{}

func (probeErrorFS) probe(string) (legacyProbe, error) {
	return nil, fmt.Errorf("injected probe failure")
}
func (probeErrorFS) loadReadOnly(string) (LaunchConfig, error) {
	return LaunchConfig{}, ErrNoLegacyLaunch
}
func (probeErrorFS) mutationSupported() bool { return true }

func TestBoundary_ControlPathMasksProbeErrorsBeforeConfigParsingOrLogging(t *testing.T) {
	env := EnvSnapshot{Home: "/home/alice", XDGConfigHome: "/tmp/control\npath", UserConfigHome: "/tmp/control\npath"}
	boundary, err := PrepareLegacyMigrationBoundary(env, probeErrorFS{})
	if err != nil {
		t.Fatal(err)
	}
	if boundary.Status != BoundaryRefused || !errors.Is(boundary.Refusal, ErrLegacyPathControl) {
		t.Fatalf("status/refusal=%v/%v, want terminal-safe control refusal", boundary.Status, boundary.Refusal)
	}
}

func TestBoundary_ExactPairRejectsEitherChildDriftAndPrefixLookalikes(t *testing.T) {
	env := EnvSnapshot{Home: "/home/alice", XDGConfigHome: "/srv/config", UserConfigHome: "/srv/config"}
	tests := []struct {
		name       string
		configPath string
		legacyPath string
	}{
		{name: "config child drift", configPath: "/srv/config/forgectl-other/config.toml", legacyPath: "/srv/config/claunch/claunch.conf"},
		{name: "legacy child drift", configPath: "/srv/config/forgectl/config.toml", legacyPath: "/srv/config/claunch-other/claunch.conf"},
		{name: "config prefix lookalike", configPath: "/srv/configuration/forgectl/config.toml", legacyPath: "/srv/config/claunch/claunch.conf"},
		{name: "legacy prefix lookalike", configPath: "/srv/config/forgectl/config.toml", legacyPath: "/srv/configuration/claunch/claunch.conf"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateLegacyMigrationPaths(env, tt.configPath, tt.legacyPath); !errors.Is(err, ErrLegacyPathPolicy) {
				t.Fatalf("error=%v, want exact-pair refusal", err)
			}
		})
	}
}

func TestBoundary_RejectsEveryUnicodeControl(t *testing.T) {
	for _, value := range []string{"line\nfeed", "carriage\rreturn", "crlf\r\nreturn", "tab\there", "escape\x1bhere", "csi\x1b[2Khere", "osc\x1b]0;forged\x07", "delete\x7fhere", "c1\u009bhere"} {
		t.Run(filepath.Base(value), func(t *testing.T) {
			_, _, err := resolveLegacyMigrationPaths(EnvSnapshot{Home: "/home/alice", XDGConfigHome: "/tmp/" + value}, "linux")
			if !errors.Is(err, ErrLegacyPathControl) {
				t.Fatalf("error = %v, want ErrLegacyPathControl", err)
			}
		})
	}
}
