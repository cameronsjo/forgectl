//go:build unix

package goal

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/privdir"
)

func TestStoreReadAbsentDoesNotCreatePrivateState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "projects")
	store := Store{Root: root}
	id := "goal-" + strings.Repeat("0", 24)
	if _, err := store.Load(context.Background(), id); !errors.Is(err, privdir.ErrAbsent) {
		t.Fatalf("Load missing goal = %v, want ErrAbsent", err)
	}
	states, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List absent store: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("List absent store = %d states, want zero", len(states))
	}
	if _, err := store.ReadArtifact(context.Background(), id, "json", 0); !errors.Is(err, privdir.ErrAbsent) {
		t.Fatalf("ReadArtifact missing goal = %v, want ErrAbsent", err)
	}
	requireFileAbsent(t, root)
}

func TestStoreCreateLoadAndRejectsImmutableGoalAlteration(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	loaded, err := fixture.store.Load(context.Background(), fixture.state.ID)
	if err != nil {
		t.Fatalf("Load created goal: %v", err)
	}
	if loaded.ID != fixture.state.ID || loaded.GoalSHA256 != fixture.state.GoalSHA256 || !reflect.DeepEqual(loaded.Spec, fixture.state.Spec) {
		t.Fatalf("loaded state does not preserve the created goal: %#v", loaded)
	}
	goalPath := filepath.Join(fixture.state.RunDir, "goal.json")
	writeGoalFile(t, goalPath, "{}\n")
	_, err = fixture.store.Load(context.Background(), fixture.state.ID)
	requireErrorContains(t, err, "approved goal record changed")
}

func TestStoreRejectsSymlinkAndHardlinkPrivateRecords(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		fixture := newGoalFixture(t, "src")
		statePath := filepath.Join(fixture.state.RunDir, "state.json")
		contents, err := os.ReadFile(statePath) //nolint:gosec // G304: fixture path is under t.TempDir
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "state.json")
		writeGoalFile(t, target, string(contents))
		if err := os.Remove(statePath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, statePath); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		_, err = fixture.store.Load(context.Background(), fixture.state.ID)
		if !errors.Is(err, privdir.ErrUnsafeFile) || !strings.Contains(err.Error(), "state.json") {
			t.Fatalf("Load symlinked state = %v, want state.json unsafe-file refusal", err)
		}
	})
	t.Run("hardlink", func(t *testing.T) {
		fixture := newGoalFixture(t, "src")
		statePath := filepath.Join(fixture.state.RunDir, "state.json")
		contents, err := os.ReadFile(statePath) //nolint:gosec // G304: fixture path is under t.TempDir
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "state.json")
		writeGoalFile(t, target, string(contents))
		if err := os.Remove(statePath); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(target, statePath); err != nil {
			t.Skipf("hardlinks unavailable: %v", err)
		}
		_, err = fixture.store.Load(context.Background(), fixture.state.ID)
		if !errors.Is(err, privdir.ErrUnsafeFile) || !strings.Contains(err.Error(), "hardlinked") {
			t.Fatalf("Load hardlinked state = %v, want hardlink refusal", err)
		}
	})
}

func TestStoreRejectsPrivateRecordWithUnsafeMode(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	statePath := filepath.Join(fixture.state.RunDir, "state.json")
	if err := os.Chmod(statePath, 0o644); err != nil { //nolint:gosec // G302: the broad mode is the hostile fixture
		t.Fatal(err)
	}
	_, err := fixture.store.Load(context.Background(), fixture.state.ID)
	if !errors.Is(err, privdir.ErrUnsafeFile) || !strings.Contains(err.Error(), "state.json") {
		t.Fatalf("Load broad-mode state = %v, want state.json unsafe-file refusal", err)
	}
}

func TestStoreRejectsUnsafeArtifactNames(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	for _, name := range []string{"../escape", ".hidden", "nested/file"} {
		t.Run(name, func(t *testing.T) {
			requireErrorContains(t, fixture.store.WriteArtifact(fixture.state.ID, name, []byte("x")), "invalid artifact name")
		})
	}
}

func acquireSupervisorLease(t testing.TB, fixture goalFixture) *os.File {
	t.Helper()
	directory, err := fixture.store.directory(fixture.state.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := lockFile(context.Background(), directory, "supervisor.lock", true)
	_ = unix.Close(directory)
	if err != nil {
		t.Fatalf("lock supervisor lease: %v", err)
	}
	return lease
}

func TestStoreRequestRequiresActiveLeaseAndSequencesControls(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	if _, err := fixture.store.Request(context.Background(), fixture.state.ID, "cancel", "", ""); err == nil {
		t.Fatal("Request without supervisor lease succeeded")
	} else {
		requireErrorContains(t, err, "goal has no active responsive supervisor")
	}
	lease := acquireSupervisorLease(t, fixture)
	control, err := fixture.store.Request(context.Background(), fixture.state.ID, "cancel", "", "")
	if err != nil {
		t.Fatalf("first cancel request: %v", err)
	}
	if control.Sequence != 1 || control.Action != "cancel" {
		t.Fatalf("first control = %#v, want cancel sequence 1", control)
	}
	if _, err := fixture.store.nextControl(fixture.state); err != nil {
		t.Fatalf("nextControl after first request: %v", err)
	}
	if _, err := fixture.store.Request(context.Background(), fixture.state.ID, "cancel", "", ""); err == nil {
		t.Fatal("duplicate cancel request succeeded before supervisor consumed sequence 1")
	} else {
		requireErrorContains(t, err, "another control is pending")
	}
	fixture.state.LastControl = 1
	if err := fixture.store.Save(context.Background(), fixture.state); err != nil {
		t.Fatalf("Save consumed control: %v", err)
	}
	control, err = fixture.store.Request(context.Background(), fixture.state.ID, "cancel", "", "")
	if err != nil {
		t.Fatalf("second cancel request: %v", err)
	}
	if control.Sequence != 2 {
		t.Fatalf("second control sequence = %d, want 2", control.Sequence)
	}
	releaseLock(lease)
	if _, err := fixture.store.Request(context.Background(), fixture.state.ID, "cancel", "", ""); err == nil {
		t.Fatal("Request after supervisor lease release succeeded")
	} else {
		requireErrorContains(t, err, "goal has no active responsive supervisor")
	}
}

func TestStoreRequestRejectsStaleAnswers(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	now := time.Now().UTC()
	fixture.state.Phase = PhaseNeedsOperator
	fixture.state.Question = &Question{ID: "question-1", Role: "coordinator", Text: "choose", CreatedAt: now}
	if err := fixture.store.Save(context.Background(), fixture.state); err != nil {
		t.Fatalf("Save question state: %v", err)
	}
	lease := acquireSupervisorLease(t, fixture)
	if _, err := fixture.store.Request(context.Background(), fixture.state.ID, "respond", "wrong-question", "answer"); err == nil {
		t.Fatal("response to a stale question succeeded")
	} else {
		requireErrorContains(t, err, "question is no longer the current unanswered decision")
	}
	control, err := fixture.store.Request(context.Background(), fixture.state.ID, "respond", "question-1", "answer")
	if err != nil {
		t.Fatalf("current question response: %v", err)
	}
	if control.Sequence != 1 || control.QuestionID != "question-1" || control.Answer != "answer" {
		t.Fatalf("response control = %#v, want sequence 1 for question-1", control)
	}
	fixture.state.LastControl = 1
	fixture.state.Question.Answer = "answer"
	if err := fixture.store.Save(context.Background(), fixture.state); err != nil {
		t.Fatalf("Save answered question: %v", err)
	}
	if _, err := fixture.store.Request(context.Background(), fixture.state.ID, "respond", "question-1", "second answer"); err == nil {
		t.Fatal("response to an already answered question succeeded")
	} else {
		requireErrorContains(t, err, "question is no longer the current unanswered decision")
	}
	releaseLock(lease)
}

func TestStoreLoadMarksStaleHeartbeatUnknown(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	fixture.state.HeartbeatAt = time.Now().UTC().Add(-time.Minute)
	if err := fixture.store.Save(context.Background(), fixture.state); err != nil {
		t.Fatalf("Save stale state: %v", err)
	}
	loaded, err := fixture.store.Load(context.Background(), fixture.state.ID)
	if err != nil {
		t.Fatalf("Load stale state: %v", err)
	}
	if loaded.Phase != PhaseUnknown || !strings.Contains(loaded.Summary, "Supervisor lease is absent") {
		t.Fatalf("stale state = phase %q summary %q, want unknown absent-lease evidence", loaded.Phase, loaded.Summary)
	}
}

func TestStoreLoadRejectsUnexpectedPrivateRecordShape(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	path := filepath.Join(fixture.state.RunDir, "state.json")
	writeGoalFile(t, path, "{}\n{}\n")
	_, err := fixture.store.Load(context.Background(), fixture.state.ID)
	if !errors.Is(err, fs.ErrInvalid) && !strings.Contains(err.Error(), "exactly one JSON document") {
		t.Fatalf("Load with two state JSON documents = %v, want exact-document refusal", err)
	}
}
