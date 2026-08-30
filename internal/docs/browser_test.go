package docs

import (
	"context"
	"errors"
	"reflect"
	"testing"

	forgexec "github.com/cameronsjo/forgectl/internal/exec"
)

func TestOpenCMUXPreviewTargetsCallerWorkspaceWithoutTakingFocus(t *testing.T) {
	runner := &forgexec.FakeRunner{}
	err := OpenCMUXPreview(context.Background(), runner, " workspace:7 ", "http://127.0.0.1:4321/")
	if err != nil {
		t.Fatal(err)
	}
	call := runner.Last()
	if call.Name != "cmux" {
		t.Fatalf("command = %q, want cmux", call.Name)
	}
	want := []string{
		"new-pane", "--workspace", "workspace:7", "--type", "browser",
		"--direction", "right", "--url", "http://127.0.0.1:4321/",
		"--focus", "false", "--json",
	}
	if !reflect.DeepEqual(call.Args, want) {
		t.Fatalf("args = %#v, want %#v", call.Args, want)
	}
	if call.Interactive {
		t.Fatal("cmux browser creation unexpectedly used the interactive runner")
	}
}

func TestOpenCMUXPreviewRequiresWorkspace(t *testing.T) {
	runner := &forgexec.FakeRunner{}
	err := OpenCMUXPreview(context.Background(), runner, "  ", "http://127.0.0.1:4321/")
	if !errors.Is(err, ErrNoCMUXWorkspace) {
		t.Fatalf("error = %v, want ErrNoCMUXWorkspace", err)
	}
	if len(runner.Calls) != 0 {
		t.Fatalf("calls = %#v, want none", runner.Calls)
	}
}
