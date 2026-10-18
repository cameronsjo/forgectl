//go:build unix

package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func codexRemoteViewFixture(fake *codexObservationFake, sandbox map[string]any) {
	fake.provider = codexSupportedModelProvider
	fake.resume = map[string]any{
		"thread": map[string]any{
			"id": fake.threadID, "cwd": fake.threadCWD, "model": fake.model, "modelProvider": fake.provider,
		},
		"cwd":                     fake.threadCWD,
		"model":                   fake.model,
		"modelProvider":           fake.provider,
		"runtimeWorkspaceRoots":   []string{fake.threadCWD},
		"approvalPolicy":          "on-request",
		"approvalsReviewer":       "user",
		"sandbox":                 sandbox,
		"activePermissionProfile": nil,
	}
}

func TestCodexPrepareRemoteViewUsesExactResumePreparation(test *testing.T) {
	fake := newCodexObservationFake(test)
	codexRemoteViewFixture(fake, map[string]any{
		"type":                "workspaceWrite",
		"networkAccess":       false,
		"writableRoots":       []string{fake.threadCWD + "/extra"},
		"excludeTmpdirEnvVar": true,
		"excludeSlashTmp":     true,
	})
	view, err := (CodexAdapter{Timeout: time.Second}).PrepareRemoteView(context.Background(), fake.worker())
	if err != nil {
		test.Fatalf("PrepareRemoteView: %v", err)
	}
	want := CodexRemoteView{
		NativeSessionID:       fake.threadID,
		Provider:              codexSupportedModelProvider,
		Model:                 fake.model,
		CWD:                   fake.threadCWD,
		RuntimeWorkspaceRoots: []string{fake.threadCWD},
		ApprovalPolicy:        "on-request",
		ApprovalsReviewer:     "user",
		Sandbox: CodexRemoteSandbox{
			Type:                "workspace-write",
			WritableRoots:       []string{fake.threadCWD + "/extra"},
			ExcludeTmpdirEnvVar: true,
			ExcludeSlashTmp:     true,
		},
	}
	if !view.Equal(want) {
		test.Fatalf("view = %#v, want %#v", view, want)
	}
	if got := fake.seenMethods(); fmt.Sprint(got) != fmt.Sprint([]string{"initialize", "initialized", "thread/read", "thread/resume"}) {
		test.Fatalf("methods = %v", got)
	}
	resumeParams := fake.seenParams("thread/resume")
	if len(resumeParams) != 1 {
		test.Fatalf("thread/resume params = %v", resumeParams)
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(resumeParams[0], &params); err != nil {
		test.Fatalf("thread/resume params: %v", err)
	}
	if got, want := len(params), 2; got != want {
		test.Fatalf("thread/resume params keys = %v, want %d", params, want)
	}
	var threadID string
	if err := json.Unmarshal(params["threadId"], &threadID); err != nil || threadID != fake.threadID {
		test.Fatalf("threadId = %s, want %q", params["threadId"], fake.threadID)
	}
	var excludeTurns bool
	if err := json.Unmarshal(params["excludeTurns"], &excludeTurns); err != nil || !excludeTurns {
		test.Fatalf("excludeTurns = %s, want true", params["excludeTurns"])
	}
}

func TestCodexPrepareRemoteViewReadOnlyPreservesEffectivePosture(test *testing.T) {
	fake := newCodexObservationFake(test)
	codexRemoteViewFixture(fake, map[string]any{"type": "readOnly", "networkAccess": false})
	fake.resume["approvalPolicy"] = "never"
	view, err := (CodexAdapter{Timeout: time.Second}).PrepareRemoteView(context.Background(), fake.worker())
	if err != nil {
		test.Fatalf("PrepareRemoteView: %v", err)
	}
	if view.ApprovalPolicy != "never" || view.Sandbox.Type != "read-only" || view.Sandbox.NetworkAccess {
		test.Fatalf("view = %#v", view)
	}
}

func TestCodexPrepareRemoteViewPreservesEmptyWritableRoots(test *testing.T) {
	fake := newCodexObservationFake(test)
	codexRemoteViewFixture(fake, map[string]any{
		"type":          "workspaceWrite",
		"networkAccess": false,
		"writableRoots": []string{},
	})
	view, err := (CodexAdapter{Timeout: time.Second}).PrepareRemoteView(context.Background(), fake.worker())
	if err != nil {
		test.Fatalf("PrepareRemoteView: %v", err)
	}
	if view.Sandbox.WritableRoots == nil || len(view.Sandbox.WritableRoots) != 0 {
		test.Fatalf("writable roots = %#v, want a preserved empty list", view.Sandbox.WritableRoots)
	}
}

func TestCodexPrepareRemoteViewRejectsMissingRolloutWithoutNativeBody(test *testing.T) {
	fake := newCodexObservationFake(test)
	codexRemoteViewFixture(fake, map[string]any{"type": "readOnly", "networkAccess": false})
	fake.resumeErr = map[string]any{"code": -32600, "message": "no rollout found for thread secret-thread"}
	_, err := (CodexAdapter{Timeout: time.Second}).PrepareRemoteView(context.Background(), fake.worker())
	if err == nil {
		test.Fatal("PrepareRemoteView unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), "no rollout found") || strings.Contains(err.Error(), "secret-thread") {
		test.Fatalf("error exposed native body: %v", err)
	}
}

func TestCodexPrepareRemoteViewRejectsUnsafeOrMismatchedPosture(test *testing.T) {
	cases := []struct {
		name string
		edit func(*codexObservationFake)
	}{
		{name: "unsupported provider", edit: func(fake *codexObservationFake) {
			fake.provider = "anthropic"
			fake.resume["modelProvider"] = "anthropic"
			fake.resume["thread"].(map[string]any)["modelProvider"] = "anthropic"
		}},
		{name: "custom permission profile", edit: func(fake *codexObservationFake) {
			fake.resume["activePermissionProfile"] = map[string]any{"id": ":workspace"}
		}},
		{name: "danger sandbox", edit: func(fake *codexObservationFake) { fake.resume["sandbox"] = map[string]any{"type": "dangerFullAccess"} }},
		{name: "external sandbox", edit: func(fake *codexObservationFake) {
			fake.resume["sandbox"] = map[string]any{"type": "externalSandbox", "networkAccess": "restricted"}
		}},
		{name: "network access", edit: func(fake *codexObservationFake) {
			fake.resume["sandbox"] = map[string]any{"type": "readOnly", "networkAccess": true}
		}},
		{name: "relative runtime root", edit: func(fake *codexObservationFake) { fake.resume["runtimeWorkspaceRoots"] = []string{"relative"} }},
		{name: "duplicate runtime root", edit: func(fake *codexObservationFake) {
			fake.resume["runtimeWorkspaceRoots"] = []string{fake.threadCWD, fake.threadCWD}
		}},
		{name: "runtime roots omit cwd", edit: func(fake *codexObservationFake) { fake.resume["runtimeWorkspaceRoots"] = []string{"/tmp"} }},
		{name: "too many runtime roots", edit: func(fake *codexObservationFake) {
			roots := make([]string, codexRemoteMaxRoots+1)
			for index := range roots {
				roots[index] = fmt.Sprintf("%s/root-%d", fake.threadCWD, index)
			}
			roots[0] = fake.threadCWD
			fake.resume["runtimeWorkspaceRoots"] = roots
		}},
		{name: "oversized runtime root", edit: func(fake *codexObservationFake) {
			fake.resume["runtimeWorkspaceRoots"] = []string{fake.threadCWD, "/" + strings.Repeat("x", codexRemoteMaxRootBytes)}
		}},
		{name: "formatted runtime root", edit: func(fake *codexObservationFake) {
			fake.resume["runtimeWorkspaceRoots"] = []string{fake.threadCWD, fake.threadCWD + "/\u202e"}
		}},
		{name: "relative writable root", edit: func(fake *codexObservationFake) {
			fake.resume["sandbox"] = map[string]any{"type": "workspaceWrite", "writableRoots": []string{"relative"}}
		}},
		{name: "duplicate writable root", edit: func(fake *codexObservationFake) {
			fake.resume["sandbox"] = map[string]any{"type": "workspaceWrite", "writableRoots": []string{fake.threadCWD, fake.threadCWD}}
		}},
		{name: "bad approval", edit: func(fake *codexObservationFake) { fake.resume["approvalPolicy"] = "untrusted" }},
		{name: "missing approval", edit: func(fake *codexObservationFake) { delete(fake.resume, "approvalPolicy") }},
		{name: "bad reviewer", edit: func(fake *codexObservationFake) { fake.resume["approvalsReviewer"] = "auto_review" }},
		{name: "missing reviewer", edit: func(fake *codexObservationFake) { delete(fake.resume, "approvalsReviewer") }},
		{name: "missing runtime roots", edit: func(fake *codexObservationFake) { delete(fake.resume, "runtimeWorkspaceRoots") }},
		{name: "missing sandbox", edit: func(fake *codexObservationFake) { delete(fake.resume, "sandbox") }},
		{name: "mismatched model", edit: func(fake *codexObservationFake) { fake.resume["model"] = "other-model" }},
		{name: "mismatched resume id", edit: func(fake *codexObservationFake) {
			fake.resume["thread"].(map[string]any)["id"] = "other-thread"
		}},
		{name: "mismatched resume cwd", edit: func(fake *codexObservationFake) { fake.resume["cwd"] = fake.threadCWD + "/other" }},
		{name: "mismatched resume provider", edit: func(fake *codexObservationFake) { fake.resume["modelProvider"] = "other-provider" }},
		{name: "read-only write exclusions", edit: func(fake *codexObservationFake) {
			fake.resume["sandbox"] = map[string]any{"type": "readOnly", "excludeSlashTmp": true}
		}},
		{name: "read-only unknown sandbox field", edit: func(fake *codexObservationFake) {
			fake.resume["sandbox"] = map[string]any{"type": "readOnly", "futurePermission": true}
		}},
		{name: "workspace-write unknown sandbox field", edit: func(fake *codexObservationFake) {
			fake.resume["sandbox"] = map[string]any{"type": "workspaceWrite", "writableRoots": []string{}, "futurePermission": true}
		}},
	}
	for _, testCase := range cases {
		test.Run(testCase.name, func(test *testing.T) {
			fake := newCodexObservationFake(test)
			codexRemoteViewFixture(fake, map[string]any{"type": "readOnly", "networkAccess": false})
			testCase.edit(fake)
			if _, err := (CodexAdapter{Timeout: time.Second}).PrepareRemoteView(context.Background(), fake.worker()); err == nil {
				test.Fatal("PrepareRemoteView unexpectedly succeeded")
			}
		})
	}
}

func TestCodexPrepareRemoteViewAllowsSpacesInRoots(test *testing.T) {
	fake := newCodexObservationFake(test)
	spaceRoot := fake.threadCWD + "/root with spaces"
	codexRemoteViewFixture(fake, map[string]any{
		"type":          "workspaceWrite",
		"networkAccess": false,
		"writableRoots": []string{spaceRoot},
	})
	fake.resume["runtimeWorkspaceRoots"] = []string{fake.threadCWD, spaceRoot}
	view, err := (CodexAdapter{Timeout: time.Second}).PrepareRemoteView(context.Background(), fake.worker())
	if err != nil {
		test.Fatalf("PrepareRemoteView: %v", err)
	}
	if !reflect.DeepEqual(view.RuntimeWorkspaceRoots, []string{fake.threadCWD, spaceRoot}) || !reflect.DeepEqual(view.Sandbox.WritableRoots, []string{spaceRoot}) {
		test.Fatalf("view roots = %#v", view)
	}
}

func TestCodexPrepareRemoteViewNeverChangesInspectSequence(test *testing.T) {
	fake := newCodexObservationFake(test)
	fake.provider = codexSupportedModelProvider
	if _, err := (CodexAdapter{Timeout: time.Second}).Inspect(context.Background(), fake.worker()); err != nil {
		test.Fatalf("Inspect: %v", err)
	}
	if got, want := fake.seenMethods(), []string{"initialize", "initialized", "thread/read", "thread/turns/list"}; !reflect.DeepEqual(got, want) {
		test.Fatalf("Inspect methods = %v, want %v", got, want)
	}
}

func TestCodexRemoteViewEqualIsStrict(test *testing.T) {
	base := CodexRemoteView{
		NativeSessionID:       "thread-1",
		Provider:              "openai",
		Model:                 "gpt-test",
		CWD:                   "/work",
		RuntimeWorkspaceRoots: []string{"/work", "/extra"},
		ApprovalPolicy:        "on-request",
		ApprovalsReviewer:     "user",
		Sandbox: CodexRemoteSandbox{
			Type:                "workspace-write",
			WritableRoots:       []string{"/work", "/extra"},
			ExcludeTmpdirEnvVar: true,
			ExcludeSlashTmp:     true,
		},
	}
	if !base.Equal(base) || !EqualCodexRemoteViews(base, base) {
		test.Fatal("identical views were not equal")
	}
	changed := base
	changed.RuntimeWorkspaceRoots = []string{"/extra", "/work"}
	if base.Equal(changed) {
		test.Fatal("root order was ignored")
	}
	changed = base
	changed.Sandbox.ExcludeSlashTmp = false
	if base.Equal(changed) {
		test.Fatal("sandbox exclusion was ignored")
	}
}
