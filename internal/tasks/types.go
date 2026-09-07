package tasks

// Task is the subset of Vikunja's task shape this client reads. Title and
// Description are UNTRUSTED INPUT — a later prompt-injection sink — and are
// never interpreted here; a rendering caller is responsible for sanitizing
// before writing them to a terminal (see internal/termsafe.SafeLine).
type Task struct {
	ID          int     `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description,omitempty"`
	Done        bool    `json:"done"`
	DoneAt      string  `json:"done_at,omitempty"`
	Priority    int     `json:"priority,omitempty"`
	Position    float64 `json:"position"`
	ProjectID   int     `json:"project_id"`
	Labels      []Label `json:"labels,omitempty"`
	// RelatedTasks maps a relation kind ("blocked", "blocking", "precedes",
	// "follows", "subtask", "parenttask", …) to the related task objects
	// Vikunja embeds inline — including each one's own Done, which `ready`
	// reads directly rather than re-resolving IDs against a second fetch.
	RelatedTasks map[string][]Task `json:"related_tasks,omitempty"`
}

// Project is the subset of Vikunja's project shape this client reads.
type Project struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	IsArchived  bool   `json:"is_archived,omitempty"`
}

// Label is the subset of Vikunja's label shape this client reads.
type Label struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	Color string `json:"hex_color,omitempty"`
}

// RelationKindBlocked is the relation kind `ready` treats as an active
// blocker when the related task is not yet Done.
const RelationKindBlocked = "blocked"

// IsReady reports whether t has no active "blocked" relation — every task
// named in its RelatedTasks["blocked"] is itself Done. An empty or absent
// "blocked" list is vacuously ready.
func IsReady(t Task) bool {
	for _, blocker := range t.RelatedTasks[RelationKindBlocked] {
		if !blocker.Done {
			return false
		}
	}
	return true
}
