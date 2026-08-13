package tmux

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	internalexec "github.com/cameronsjo/forgectl/internal/exec"
)

func TestParseTmuxVersion(t *testing.T) {
	tests := []struct {
		input      string
		major      int
		minor      int
		normalized string
		ok         bool
	}{
		{"tmux 2.2", 2, 2, "tmux 2.2", true},
		{"tmux 2.2a", 2, 2, "tmux 2.2a", true},
		{"tmux 3.7b", 3, 7, "tmux 3.7b", true},
		{"tmux 3.2a-4ubuntu0.2", 3, 2, "tmux 3.2a-4ubuntu0.2", true},
		{"tmux 3.2+build", 3, 2, "tmux 3.2+build", true},
		{"tmux 3.2~rc1", 3, 2, "tmux 3.2~rc1", true},
		{"tmux 2.2\n", 0, 0, "", false},
		{" tmux 2.2", 0, 0, "", false},
		{"tmux next-3.5", 0, 0, "", false},
		{"tmux 2", 0, 0, "", false},
		{"tmux 2.2beta", 0, 0, "", false},
		{"tmux 999999999999999999999.2", 0, 0, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			major, minor, normalized, err := parseTmuxVersion(tt.input)
			if (err == nil) != tt.ok {
				t.Fatalf("parseTmuxVersion(%q) error = %v, want ok=%v", tt.input, err, tt.ok)
			}
			if err == nil && (major != tt.major || minor != tt.minor || normalized != tt.normalized) {
				t.Fatalf("parseTmuxVersion(%q) = (%d,%d,%q), want (%d,%d,%q)", tt.input, major, minor, normalized, tt.major, tt.minor, tt.normalized)
			}
		})
	}
}

func TestCheckGenerationCapability_CallOrderAndFloor(t *testing.T) {
	for _, tc := range []struct {
		version string
		wantErr bool
	}{
		{"tmux 2.1", true},
		{"tmux 2.2", false},
		{"tmux 2.3", false},
		{"tmux 3.7b", false},
	} {
		t.Run(tc.version, func(t *testing.T) {
			fake := &internalexec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
				switch args[0] {
				case "-V":
					return tc.version, nil
				case "display-message":
					return "123" + sep + "456" + sep + "@7", nil
				default:
					return "", errors.New("unexpected call")
				}
			}}
			c := New(fake)
			capability, err := c.CheckGenerationCapability(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckGenerationCapability() error = %v, wantErr=%v", err, tc.wantErr)
			}
			if !tc.wantErr && capability.Version != tc.version {
				t.Errorf("version = %q, want %q", capability.Version, tc.version)
			}
			wantCalls := 2
			if tc.wantErr {
				wantCalls = 1
			}
			if len(fake.Calls) != wantCalls {
				t.Fatalf("calls = %+v, want %d", fake.Calls, wantCalls)
			}
			if !reflect.DeepEqual(fake.Calls[0].Args, []string{"-V"}) {
				t.Errorf("first args = %v, want [-V]", fake.Calls[0].Args)
			}
			if !tc.wantErr {
				want := []string{"display-message", "-p", IdentityFormat}
				if !reflect.DeepEqual(fake.Calls[1].Args, want) {
					t.Errorf("display args = %v, want %v", fake.Calls[1].Args, want)
				}
			}
		})
	}
}

func TestCheckGenerationCapability_AbsentDefaultSucceedsWithoutParsingStderr(t *testing.T) {
	fake := &internalexec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if args[0] == "-V" {
			return "tmux 3.7b", nil
		}
		return "", commandFailure(name, args, "mensaje localizado")
	}}
	c := New(fake)
	c.getenv = func(string) string { return "" }
	c.getuid = func() int { return 501 }
	c.lstat = func(string) (FileInfo, error) { return nil, ErrNotExist }
	if _, err := c.CheckGenerationCapability(context.Background()); err != nil {
		t.Fatalf("CheckGenerationCapability() = %v, want success for absent default", err)
	}
}

func TestCheckGenerationCapability_RefusalMatrix(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	exitOne := func(name string, args []string) error { return commandFailure(name, args, "localized") }
	tests := []struct {
		name       string
		ctx        context.Context
		versionOut string
		versionErr error
		displayOut string
		displayErr func(string, []string) error
		tmuxEnv    string
		lstatErr   error
	}{
		{name: "malformed version", ctx: context.Background(), versionOut: "tmux next-3.5"},
		{name: "missing binary", ctx: context.Background(), versionErr: &internalexec.CommandError{Name: "tmux", Args: []string{"-V"}, ExitCode: -1, Err: errors.New("not found")}},
		{name: "malformed active identity", ctx: context.Background(), versionOut: "tmux 3.7b", displayOut: "123" + sep + "bad" + sep + "@0"},
		{name: "custom socket", ctx: context.Background(), versionOut: "tmux 3.7b", displayErr: exitOne, tmuxEnv: "/tmp/custom,1,0", lstatErr: ErrNotExist},
		{name: "stale socket", ctx: context.Background(), versionOut: "tmux 3.7b", displayErr: exitOne},
		{name: "permission", ctx: context.Background(), versionOut: "tmux 3.7b", displayErr: exitOne, lstatErr: os.ErrPermission},
		{name: "canceled", ctx: canceled, versionOut: "tmux 3.7b", displayErr: exitOne, lstatErr: ErrNotExist},
		{name: "unknown display", ctx: context.Background(), versionOut: "tmux 3.7b", displayErr: func(string, []string) error { return errors.New("plain failure") }, lstatErr: ErrNotExist},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &internalexec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
				if args[0] == "-V" {
					return tt.versionOut, tt.versionErr
				}
				if tt.displayErr != nil {
					return "", tt.displayErr(name, args)
				}
				return tt.displayOut, nil
			}}
			c := New(fake)
			c.getenv = func(key string) string {
				if key == "TMUX" {
					return tt.tmuxEnv
				}
				return ""
			}
			c.getuid = func() int { return 501 }
			c.lstat = func(string) (FileInfo, error) { return nil, tt.lstatErr }
			if _, err := c.CheckGenerationCapability(tt.ctx); err == nil {
				t.Fatal("capability succeeded, want refusal")
			}
		})
	}
}
