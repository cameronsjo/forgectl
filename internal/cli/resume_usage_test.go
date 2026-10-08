package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/resume"
)

// pinCwd restores the process working directory, which resumeSession changes
// on its way to the exec.
func pinCwd(t *testing.T) {
	t.Helper()
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(before) }) //nolint:errcheck // best-effort restore
}

func usageResumeConfig(enabled bool) config.Config {
	return config.Config{Launch: config.LaunchConfig{
		Defaults:   config.LaunchDefaults{Harness: "claude", Model: "opus"},
		UsageStats: enabled,
	}}
}

func deadSession(t *testing.T) resume.Session {
	t.Helper()
	return resume.Session{
		ID:         "aaaaaaaa-0000-0000-0000-00000000000f",
		Name:       "lone-anvil",
		Repo:       "cc",
		Branch:     "main",
		Cwd:        t.TempDir(),
		LastActive: time.Now(),
	}
}

func TestResumeSession_ResumeAndForkRecordExactlyOnceBeforeExec(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fork     bool
		wantMode string
	}{
		{"continue", false, launch.UsageSessionResume},
		{"fork", true, launch.UsageSessionFork},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeClaudeBin(t)
			pinResumePaths(t)
			pinCwd(t)
			probe := installUsageProbe(t)
			session := deadSession(t)

			cmd, _, _ := newTestCmd()
			if err := resumeSession(cmd, usageResumeConfig(true), nil, session, tc.fork, false); err != nil {
				t.Fatalf("resumeSession: %v", err)
			}
			if len(probe.events) != 1 || probe.execs != 1 {
				t.Fatalf("recorded %d event(s) and execed %d time(s), want exactly one of each",
					len(probe.events), probe.execs)
			}
			ev := probe.events[0]
			if ev.SessionMode != tc.wantMode || ev.Posture != launch.UsagePostureDefault {
				t.Fatalf("classification = %s/%s, want %s/default", ev.SessionMode, ev.Posture, tc.wantMode)
			}
			if ev.Harness != "claude" {
				t.Fatalf("harness = %q; forgectl resume is Claude-only", ev.Harness)
			}
			// The session id and its working directory are exactly the values
			// a resume knows and a row must never carry.
			row, err := launch.EncodeUsageEvent(ev)
			if err != nil {
				t.Fatalf("EncodeUsageEvent: %v", err)
			}
			for _, secret := range []string{session.ID, session.Cwd, session.Name, session.Repo} {
				if strings.Contains(string(row), secret) {
					t.Fatalf("resume row leaked %q: %s", secret, row)
				}
			}
		})
	}
}

func TestResumeSession_DryRunAndBlockersRecordZero(t *testing.T) {
	t.Run("dry run", func(t *testing.T) {
		fakeClaudeBin(t)
		pinResumePaths(t)
		pinCwd(t)
		probe := installUsageProbe(t)
		cmd, _, _ := newTestCmd()
		if err := resumeSession(cmd, usageResumeConfig(true), nil, deadSession(t), false, true); err != nil {
			t.Fatalf("dry-run resumeSession: %v", err)
		}
		if len(probe.events) != 0 || probe.execs != 0 {
			t.Fatalf("a dry run recorded %d event(s) and execed %d time(s), want none",
				len(probe.events), probe.execs)
		}
	})

	t.Run("missing working directory", func(t *testing.T) {
		fakeClaudeBin(t)
		pinResumePaths(t)
		pinCwd(t)
		probe := installUsageProbe(t)
		session := deadSession(t)
		session.Cwd = ""
		cmd, _, _ := newTestCmd()
		if err := resumeSession(cmd, usageResumeConfig(true), nil, session, false, false); err == nil {
			t.Fatal("resumeSession accepted a session with no recorded cwd")
		}
		if len(probe.events) != 0 || probe.execs != 0 {
			t.Fatalf("a refused resume recorded %d event(s) and execed %d time(s), want none",
				len(probe.events), probe.execs)
		}
	})

	t.Run("live session without fork", func(t *testing.T) {
		fakeClaudeBin(t)
		pinResumePaths(t)
		pinCwd(t)
		probe := installUsageProbe(t)
		session := deadSession(t)
		session.Live = true
		session.Pid = os.Getpid()
		cmd, _, _ := newTestCmd()
		if err := resumeSession(cmd, usageResumeConfig(true), nil, session, false, false); err == nil {
			t.Fatal("resumeSession continued a live session")
		}
		if len(probe.events) != 0 || probe.execs != 0 {
			t.Fatalf("a refused resume recorded %d event(s) and execed %d time(s), want none",
				len(probe.events), probe.execs)
		}
	})
}

// TestResumeSession_StdoutTerminalDecidesAllowDanger is the resume half of
// forgectl#899. `forgectl resume` execs claude onto the same stdout, so off a
// terminal the resumed session is not interactive and the profile's
// allow_danger (the built-in default, true) must not reach it. On a terminal
// it still does, and the rest of ResumeArgs stays either way.
//
// Mutations that turn it red: drop the launchStdoutIsTerminal check in
// resumeSession (the piped row regains the flag), or clear AllowDanger
// unconditionally (the terminal row loses it).
func TestResumeSession_StdoutTerminalDecidesAllowDanger(t *testing.T) {
	const danger = "--allow-dangerously-skip-permissions"
	for _, terminal := range []bool{true, false} {
		t.Run(fmt.Sprintf("terminal=%t", terminal), func(t *testing.T) {
			fakeClaudeBin(t)
			pinResumePaths(t)
			pinCwd(t)
			got := pinLaunchStdoutTerminal(t, terminal)
			session := deadSession(t)

			cmd, _, _ := newTestCmd()
			if err := resumeSession(cmd, usageResumeConfig(false), nil, session, false, false); err != nil {
				t.Fatalf("resumeSession: %v", err)
			}
			if len(*got) == 0 {
				t.Fatal("resumeSession never reached the exec seam")
			}
			if has := slices.Contains(*got, danger); has != terminal {
				t.Errorf("argv %q: contains %s = %t, want %t", *got, danger, has, terminal)
			}
			for _, keep := range []string{"--permission-mode", "--model", "--ide", "--resume", session.ID} {
				if !slices.Contains(*got, keep) {
					t.Errorf("argv %q lost %s; only the danger flag may be withheld", *got, keep)
				}
			}
		})
	}
}
