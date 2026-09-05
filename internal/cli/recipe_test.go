package cli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

func withRecipeEnv(t *testing.T, values map[string]string) {
	t.Helper()
	previous := lookupRecipeEnv
	lookupRecipeEnv = func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
	t.Cleanup(func() { lookupRecipeEnv = previous })
}

func TestResolveRecipeHerdrTarget(t *testing.T) {
	tests := []struct {
		name     string
		explicit string
		env      map[string]string
		want     string
		wantErr  bool
	}{
		{
			name:     "explicit target wins",
			explicit: "reviewer",
			env:      map[string]string{herdrPaneIDEnv: "w1:p2"},
			want:     "reviewer",
		},
		{
			name: "pane id env wins over active pane env",
			env: map[string]string{
				herdrPaneIDEnv:       "w1:p2",
				herdrActivePaneIDEnv: "w1:p3",
			},
			want: "w1:p2",
		},
		{
			name: "active pane fallback",
			env:  map[string]string{herdrActivePaneIDEnv: "w1:p3"},
			want: "w1:p3",
		},
		{
			name:    "missing target fails",
			env:     map[string]string{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withRecipeEnv(t, tt.env)
			got, ok := resolveRecipeHerdrTarget(tt.explicit)
			if tt.wantErr {
				if ok {
					t.Fatal("resolveRecipeHerdrTarget() = ok, want missing target")
				}
				return
			}
			if !ok {
				t.Fatal("resolveRecipeHerdrTarget() = missing target, want ok")
			}
			if got != tt.want {
				t.Fatalf("resolveRecipeHerdrTarget() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRecipeAfkRunsJournalThenInteractiveCompact(t *testing.T) {
	withRecipeEnv(t, map[string]string{herdrPaneIDEnv: "w1:p2"})
	fake := &exec.FakeRunner{}

	if err := runRecipeAfk(context.Background(), fake, recipeAfkOptions{}); err != nil {
		t.Fatalf("runRecipeAfk() error = %v, want nil", err)
	}

	want := []exec.Call{
		{Name: "herdr", Args: []string{"agent", "prompt", "w1:p2", "/journal", "--wait"}},
		{Name: "herdr", Args: []string{"agent", "type-submit", "w1:p2", "/compact"}},
	}
	if !reflect.DeepEqual(fake.Calls, want) {
		t.Fatalf("calls = %#v, want %#v", fake.Calls, want)
	}
}

func TestRecipeAfkUsesExplicitTarget(t *testing.T) {
	withRecipeEnv(t, map[string]string{herdrPaneIDEnv: "w1:p2"})
	fake := &exec.FakeRunner{}

	if err := runRecipeAfk(context.Background(), fake, recipeAfkOptions{Target: "reviewer"}); err != nil {
		t.Fatalf("runRecipeAfk() error = %v, want nil", err)
	}

	if got := fake.Calls[0].Args[2]; got != "reviewer" {
		t.Fatalf("prompt target = %q, want reviewer", got)
	}
	if got := fake.Calls[1].Args[2]; got != "reviewer" {
		t.Fatalf("type-submit target = %q, want reviewer", got)
	}
}

func TestRecipeAfkFailsBeforeSendingWithoutTarget(t *testing.T) {
	withRecipeEnv(t, map[string]string{})
	fake := &exec.FakeRunner{}

	err := runRecipeAfk(context.Background(), fake, recipeAfkOptions{})
	if err == nil {
		t.Fatal("runRecipeAfk() error = nil, want target error")
	}
	if ExitCode(err) != 2 {
		t.Fatalf("ExitCode(error) = %d, want 2", ExitCode(err))
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("calls = %#v, want none", fake.Calls)
	}
}

func TestRecipeAfkStopsBeforeCompactWhenJournalFails(t *testing.T) {
	withRecipeEnv(t, map[string]string{herdrPaneIDEnv: "w1:p2"})
	journalErr := errors.New("journal failed")
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) >= 4 && args[3] == "/journal" {
			return "", journalErr
		}
		return "", nil
	}}

	err := runRecipeAfk(context.Background(), fake, recipeAfkOptions{})
	if !errors.Is(err, journalErr) {
		t.Fatalf("runRecipeAfk() error = %v, want wrapped journal error", err)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("calls = %#v, want only journal prompt", fake.Calls)
	}
}

func TestRecipeCommandAliasAndAfkSubcommand(t *testing.T) {
	withRecipeEnv(t, map[string]string{herdrPaneIDEnv: "w1:p2"})
	fake := &exec.FakeRunner{}
	root := newRoot(module.Deps{Runner: fake})
	root.SetArgs([]string{"r", "afk"})

	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("ExecuteContext() error = %v, want nil", err)
	}
	if len(fake.Calls) != 2 {
		t.Fatalf("calls = %#v, want two herdr calls", fake.Calls)
	}
}

func TestRecipeAfkCommandRejectsArgs(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	root.SetArgs([]string{"recipe", "afk", "extra"})

	err := root.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("ExecuteContext() error = nil, want arg error")
	}
	if !strings.Contains(err.Error(), "unknown command") && !strings.Contains(err.Error(), "accepts 0 arg") {
		t.Fatalf("ExecuteContext() error = %v, want Cobra argument error", err)
	}
}
