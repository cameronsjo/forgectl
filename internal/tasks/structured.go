package tasks

import (
	"fmt"
	"sort"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// Structured output for the read tools.
//
// structuredContent sits OUTSIDE the board-text fence: a client hands it to
// the model as plain JSON, with no frame saying "this is data". So it carries
// ONLY values the server has validated and that cannot hold prose — numeric
// ids, bools, counts, a relation kind checked against Vikunja's fixed enum,
// and a timestamp re-rendered from a parsed time. A title, a description, or
// any other string that arrived from the board never appears here; those stay
// in the fenced text content. TestStructuredContent_CarriesNoBoardText pins
// that, and every output struct below has no free-text string field by design.
// Adding one is the change that reopens the injection channel the fence
// exists to close.

// relationKinds is Vikunja's fixed set of relation kinds. A kind outside it
// arrives as a JSON object key from the same untrusted response as the titles
// beside it, so it is dropped from structured output rather than passed
// through; the fenced text still shows it.
var relationKinds = []string{
	"subtask", "parenttask", "related", "duplicateof", "duplicates",
	"blocking", "blocked", "precedes", "follows", "copiedfrom", "copiedto",
}

func isRelationKind(kind string) bool {
	for _, k := range relationKinds {
		if k == kind {
			return true
		}
	}
	return false
}

type (
	projectRef struct {
		ID int `json:"id" jsonschema:"the project id"`
	}
	listProjectsOutput struct {
		Total     int          `json:"total" jsonschema:"number of projects this credential can see"`
		Shown     int          `json:"shown" jsonschema:"number of projects in this result"`
		Truncated bool         `json:"truncated" jsonschema:"true when shown is less than total"`
		Projects  []projectRef `json:"projects" jsonschema:"the projects shown, in the same order as the text content"`
	}

	taskRef struct {
		ID        int    `json:"id" jsonschema:"the task id"`
		ProjectID int    `json:"project_id" jsonschema:"the id of the project the task is in"`
		Done      bool   `json:"done" jsonschema:"whether the task is done"`
		DoneAt    string `json:"done_at,omitempty" jsonschema:"when the task was marked done, RFC 3339 UTC; absent when not done or unknown"`
		Priority  int    `json:"priority" jsonschema:"the task priority as a number (0 = unset)"`
	}
	taskListOutput struct {
		Total     int       `json:"total" jsonschema:"number of tasks that matched"`
		Shown     int       `json:"shown" jsonschema:"number of tasks in this result"`
		Truncated bool      `json:"truncated" jsonschema:"true when shown is less than total"`
		Tasks     []taskRef `json:"tasks" jsonschema:"the tasks shown, in the same order as the text content"`
	}

	relationRef struct {
		Kind string `json:"kind" jsonschema:"the relation kind, one of Vikunja's fixed relation kinds"`
		ID   int    `json:"id" jsonschema:"the related task id"`
		Done bool   `json:"done" jsonschema:"whether the related task is done"`
	}
	getTaskOutput struct {
		taskRef
		HasDescription bool          `json:"has_description" jsonschema:"whether the task has a description (the text is in the fenced text content)"`
		Relations      []relationRef `json:"relations" jsonschema:"relations with a recognised kind, sorted by kind then id"`
	}
)

// doneAtPattern is the only shape structuredTime emits. Declaring it in the
// schema makes the SDK's output validation refuse any other string in done_at,
// so a later edit that copies board bytes into the field fails the call
// instead of shipping them.
const doneAtPattern = `^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`

// outputSchema reflects T's schema and pins every done_at property to
// doneAtPattern. It panics on failure: the output types are fixed at compile
// time, so a reflection error is a programming error, and mcp.AddTool itself
// panics on the same class of mistake.
func outputSchema[T any]() *jsonschema.Schema {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("tasks: output schema: %v", err))
	}
	pinDoneAt(schema)
	return schema
}

// pinDoneAt sets doneAtPattern on every done_at property at any depth.
func pinDoneAt(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	for name, p := range s.Properties {
		if name == "done_at" {
			p.Pattern = doneAtPattern
		}
		pinDoneAt(p)
	}
	pinDoneAt(s.Items)
}

// getTaskSchema is the reflected getTaskOutput schema with the relation kind
// narrowed to its enum, so the SDK's output validation rejects a kind that
// bypassed isRelationKind rather than shipping it.
func getTaskSchema() *jsonschema.Schema {
	schema := outputSchema[getTaskOutput]()
	rels, ok := schema.Properties["relations"]
	if !ok || rels.Items == nil {
		panic("tasks: get_task output schema has no relations items")
	}
	kind, ok := rels.Items.Properties["kind"]
	if !ok {
		panic("tasks: get_task output schema has no relation kind")
	}
	kind.Enum = make([]any, 0, len(relationKinds))
	for _, k := range relationKinds {
		kind.Enum = append(kind.Enum, k)
	}
	return schema
}

// structuredTime re-renders a board timestamp as RFC 3339 UTC, or returns ""
// when it does not parse or is Vikunja's zero time ("0001-01-01T00:00:00Z" on
// every task never marked done). The output is built from the parsed value,
// never copied from the input, so no board byte survives the trip.
//
// The year bound matters: "9999-12-31T23:00:00-02:00" parses, but lands in
// year 10000 in UTC, which renders outside doneAtPattern — and the SDK's
// output validation would then fail the WHOLE call, letting one planted
// timestamp take down list_tasks for everyone reading that board.
func structuredTime(s string) string {
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return ""
	}
	ts = ts.UTC()
	if ts.Year() <= 1 || ts.Year() > 9999 {
		return ""
	}
	return ts.Format(time.RFC3339)
}

func toTaskRef(t Task) taskRef {
	return taskRef{
		ID:        t.ID,
		ProjectID: t.ProjectID,
		Done:      t.Done,
		DoneAt:    structuredTime(t.DoneAt),
		Priority:  t.Priority,
	}
}

func toGetTaskOutput(t Task) getTaskOutput {
	out := getTaskOutput{
		taskRef:        toTaskRef(t),
		HasDescription: t.Description != "",
		Relations:      []relationRef{},
	}
	for kind, rels := range t.RelatedTasks {
		if !isRelationKind(kind) {
			continue
		}
		for _, rel := range rels {
			out.Relations = append(out.Relations, relationRef{Kind: kind, ID: rel.ID, Done: rel.Done})
		}
	}
	sort.Slice(out.Relations, func(i, j int) bool {
		if out.Relations[i].Kind != out.Relations[j].Kind {
			return out.Relations[i].Kind < out.Relations[j].Kind
		}
		return out.Relations[i].ID < out.Relations[j].ID
	})
	return out
}
