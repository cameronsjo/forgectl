package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// wantCapped holds an error to the QuoteArgMax form: the planted text is
// escaped, cut at the input budget, and marked with the ellipsis (#706, #738).
func wantCapped(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: err = nil", what)
	}
	msg := err.Error()
	if strings.Contains(msg, "\x1b") || strings.Contains(msg, strings.Repeat("K", 81)) {
		t.Errorf("%s: error = %q, echoes the name uncapped or unescaped", what, msg)
	}
	if !strings.Contains(msg, `\x1b`) || !strings.Contains(msg, `"…`) {
		t.Errorf("%s: error = %q, want the escaped, capped name", what, msg)
	}
}

// planted is a workflow-file or --param name built to flood a terminal.
var planted = "x\x1b[2J" + strings.Repeat("K", 400)

func TestRun_UnknownStepVerbIsCapped(t *testing.T) {
	exe := NewExecutor(&exec.FakeRunner{}, testRegistry(t))
	plan := Plan{Name: "demo", Steps: []PlanStep{{Uses: planted}}}
	wantCapped(t, "unknown step verb", exe.Run(context.Background(), plan, NewContext(nil)))
}

func TestResolveParams_NamesAreCapped(t *testing.T) {
	_, err := resolveParams(map[string]Param{planted: {Required: true}}, nil)
	wantCapped(t, "missing required param", err)
	_, err = resolveParams(nil, map[string]string{planted: "v"})
	wantCapped(t, "unknown param", err)
}
