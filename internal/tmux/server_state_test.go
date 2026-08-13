package tmux

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	internalexec "github.com/cameronsjo/forgectl/internal/exec"
)

// Aliases keep production seam types explicit without test-only public API.
type FileInfo = os.FileInfo

var ErrNotExist = os.ErrNotExist

func commandFailure(name string, args []string, stderr string) error {
	return &internalexec.CommandError{Name: name, Args: append([]string(nil), args...), Stderr: stderr, ExitCode: 1, Err: errors.New("exit status 1")}
}

func TestClassifyServerFailure(t *testing.T) {
	args := []string{"list-windows", "-a", "-F", windowFormat}
	tests := []struct {
		name      string
		ctx       context.Context
		err       error
		tmux      string
		tmp       string
		lstatErr  error
		want      serverFailureKind
		wantLstat bool
	}{
		{"absent default", context.Background(), commandFailure("tmux", args, "anything"), "", "/tmp", os.ErrNotExist, serverAbsentDefault, true},
		{"custom socket", context.Background(), commandFailure("tmux", args, "anything"), "/tmp/custom,1,0", "/tmp", os.ErrNotExist, serverCustomSocket, false},
		{"relative root", context.Background(), commandFailure("tmux", args, "anything"), "", "relative", os.ErrNotExist, serverUnknown, false},
		{"stale socket", context.Background(), commandFailure("tmux", args, "anything"), "", "/tmp", nil, serverStaleSocket, true},
		{"permission", context.Background(), commandFailure("tmux", args, "anything"), "", "/tmp", os.ErrPermission, serverSocketPermission, true},
		{"plain error", context.Background(), errors.New("no server running"), "", "/tmp", os.ErrNotExist, serverUnknown, false},
		{"mismatched args", context.Background(), commandFailure("tmux", []string{"list-sessions"}, "anything"), "", "/tmp", os.ErrNotExist, serverUnknown, false},
		{"wrong exit", context.Background(), &internalexec.CommandError{Name: "tmux", Args: args, ExitCode: 2, Err: errors.New("exit")}, "", "/tmp", os.ErrNotExist, serverUnknown, false},
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests = append(tests, struct {
		name      string
		ctx       context.Context
		err       error
		tmux      string
		tmp       string
		lstatErr  error
		want      serverFailureKind
		wantLstat bool
	}{"canceled", canceled, commandFailure("tmux", args, "anything"), "", "/tmp", os.ErrNotExist, serverCanceled, false})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New(&internalexec.FakeRunner{})
			c.getenv = func(key string) string {
				if key == "TMUX" {
					return tt.tmux
				}
				if key == "TMUX_TMPDIR" {
					return tt.tmp
				}
				return ""
			}
			c.geteuid = func() int { return 501 }
			lstatCalls := 0
			c.lstat = func(path string) (os.FileInfo, error) {
				lstatCalls++
				if path != "/tmp/tmux-501/default" {
					t.Errorf("lstat path = %q", path)
				}
				return nil, tt.lstatErr
			}
			got := c.classifyServerFailure(tt.ctx, args, tt.err)
			if got.Kind != tt.want {
				t.Errorf("kind = %v, want %v", got.Kind, tt.want)
			}
			if (lstatCalls > 0) != tt.wantLstat {
				t.Errorf("lstat calls = %d, wantLstat=%v", lstatCalls, tt.wantLstat)
			}
		})
	}
}

func TestListWindows_ServerFailureSemantics(t *testing.T) {
	args := []string{"list-windows", "-a", "-F", windowFormat}
	for _, tc := range []struct {
		name     string
		lstatErr error
		wantErr  bool
	}{
		{"absent", os.ErrNotExist, false},
		{"permission", os.ErrPermission, true},
		{"stale", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &internalexec.FakeRunner{RunFunc: func(name string, got []string) (string, error) {
				if !reflect.DeepEqual(got, args) {
					t.Errorf("args = %v, want %v", got, args)
				}
				return "", commandFailure(name, got, "localized")
			}}
			c := New(fake)
			c.getenv = func(string) string { return "" }
			c.geteuid = func() int { return 501 }
			c.lstat = func(string) (os.FileInfo, error) { return nil, tc.lstatErr }
			wins, err := c.ListWindows(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("ListWindows error = %v, wantErr=%v", err, tc.wantErr)
			}
			if err == nil && len(wins) != 0 {
				t.Errorf("windows = %+v, want empty", wins)
			}
		})
	}
}

func TestListAPIs_AbsentDefaultSoftEmpty(t *testing.T) {
	tests := []struct {
		name string
		call func(context.Context, *Client) error
	}{
		{"sessions", func(ctx context.Context, c *Client) error { _, err := c.ListSessions(ctx); return err }},
		{"windows", func(ctx context.Context, c *Client) error { _, err := c.ListWindows(ctx); return err }},
		{"panes", func(ctx context.Context, c *Client) error { _, err := c.ListPanes(ctx); return err }},
		{"most recent", func(ctx context.Context, c *Client) error { _, err := c.mostRecentSession(ctx); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &internalexec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
				return "", commandFailure(name, args, "no server running")
			}}
			c := New(fake)
			c.getenv = func(string) string { return "" }
			c.geteuid = func() int { return 501 }
			c.lstat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
			if err := tt.call(context.Background(), c); err != nil {
				t.Fatalf("call = %v, want soft empty", err)
			}
		})
	}
}
