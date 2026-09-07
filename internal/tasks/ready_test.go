package tasks

import "testing"

// TestReady_ExcludesActiveBlocker is the fixture where a naive
// implementation (one that ignores related_tasks entirely, or treats any
// "blocked" entry as terminal regardless of its own Done state) would
// wrongly include a task that has a live blocker.
func TestReady_ExcludesActiveBlocker(t *testing.T) {
	blocker := Task{ID: 1, Title: "blocker", Done: false, Position: 1}
	blocked := Task{
		ID: 2, Title: "blocked", Done: false, Position: 2,
		RelatedTasks: map[string][]Task{
			RelationKindBlocked: {blocker},
		},
	}
	got := Ready([]Task{blocker, blocked})

	for _, task := range got {
		if task.ID == blocked.ID {
			t.Fatalf("Ready() included task %d, which has an active (not-done) blocker", blocked.ID)
		}
	}
	if len(got) != 1 || got[0].ID != blocker.ID {
		t.Fatalf("Ready() = %+v, want only the blocker itself", got)
	}
}

// TestReady_IncludesOnceBlockerIsDone is the mirror of the exclusion case —
// the same task, with its blocker's Done flipped, must now appear.
func TestReady_IncludesOnceBlockerIsDone(t *testing.T) {
	blocker := Task{ID: 1, Title: "blocker", Done: true, Position: 1}
	blocked := Task{
		ID: 2, Title: "blocked", Done: false, Position: 2,
		RelatedTasks: map[string][]Task{
			RelationKindBlocked: {blocker},
		},
	}
	got := Ready([]Task{blocked})

	if len(got) != 1 || got[0].ID != blocked.ID {
		t.Fatalf("Ready() = %+v, want the formerly-blocked task once its blocker is done", got)
	}
}

func TestReady_ExcludesDoneTasksAndSortsByPosition(t *testing.T) {
	done := Task{ID: 1, Title: "done", Done: true, Position: 0}
	second := Task{ID: 2, Title: "second", Done: false, Position: 5}
	first := Task{ID: 3, Title: "first", Done: false, Position: 1}

	got := Ready([]Task{done, second, first})

	if len(got) != 2 {
		t.Fatalf("Ready() returned %d tasks, want 2 (done task excluded)", len(got))
	}
	if got[0].ID != first.ID || got[1].ID != second.ID {
		t.Fatalf("Ready() order = [%d, %d], want ranked by position [%d, %d]", got[0].ID, got[1].ID, first.ID, second.ID)
	}
}

// TestReady_IgnoresOtherRelationKinds asserts that a "blocking" (this task
// blocks something else) or "subtask" relation never suppresses readiness —
// only "blocked" does.
func TestReady_IgnoresOtherRelationKinds(t *testing.T) {
	other := Task{ID: 1, Title: "other", Done: false}
	task := Task{
		ID: 2, Title: "task", Done: false,
		RelatedTasks: map[string][]Task{
			"blocking": {other},
			"subtask":  {other},
		},
	}
	if !IsReady(task) {
		t.Fatal("IsReady() = false for a task with no 'blocked' relation; only 'blocked' should count")
	}
}
