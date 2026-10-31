//go:build unix

package exec

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// TestRunStreamingScrubbed_DropsTheLauncherHandles: the long-lived hc
// subscription gets the same environment as a sensitive hc call: the
// launcher's handles gone, the core selection and the rest byte-exact.
func TestRunStreamingScrubbed_DropsTheLauncherHandles(t *testing.T) {
	gone := []string{"CLAUDE_CODE_MESSAGING_TOKEN", "HC_TERMINAL_ID", "HC_SCOPE", "HC_TERMINAL_SOCKET", "HERDR_ACTIVE_PANE_ID", "FORGECTL_WORKER", "CLAUDE_PID", "CLAUDE_FOO"}
	for _, k := range gone {
		t.Setenv(k, "launcher-"+k)
	}
	t.Setenv("HC_SESSION", "work")
	t.Setenv("FORGECTL_UNRELATED", "keep")
	t.Setenv("CLAUDE_CONFIG_DIR", "/x/claude")
	var out bytes.Buffer
	if err := (OSRunner{}).RunStreamingScrubbed(context.Background(), &out, io.Discard, "/usr/bin/env"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, k := range gone {
		if strings.Contains(got, k+"=") {
			t.Errorf("%s reached the streamed child", k)
		}
	}
	for _, kv := range []string{"HC_SESSION=work", "FORGECTL_UNRELATED=keep", "CLAUDE_CONFIG_DIR=/x/claude"} {
		if !strings.Contains(got, kv+"\n") {
			t.Errorf("%s did not reach the streamed child", kv)
		}
	}
}

// TestWithoutLauncherSessionHandles_NeverNil: an empty result must stay an
// empty slice, because a nil cmd.Env inherits this process's environment.
func TestWithoutLauncherSessionHandles_NeverNil(t *testing.T) {
	if got := withoutLauncherSessionHandles([]string{"HC_SCOPE=x"}); got == nil || len(got) != 0 {
		t.Fatalf("got %#v, want an empty non-nil slice", got)
	}
}
