package goalnative

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeTurnFailureRetainsBoundedActionableMessage(t *testing.T) {
	message := nativeTurnFailure("failed", json.RawMessage(`{"message":"The requested model is unavailable."}`)).Error()
	if !strings.Contains(message, "failed") || !strings.Contains(message, "requested model is unavailable") {
		t.Fatalf("missing native diagnostic: %s", message)
	}
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`{"unknown":"not a message"}`), json.RawMessage(`invalid`)} {
		if got := nativeTurnFailure("interrupted", raw).Error(); got != "native turn ended with status interrupted" {
			t.Fatalf("unexpected fallback: %s", got)
		}
	}
	data, err := json.Marshal(map[string]string{"message": strings.Repeat("x", 4096)})
	if err != nil {
		t.Fatal(err)
	}
	bounded := nativeTurnFailure("failed", data).Error()
	if len(bounded) > 2200 || !strings.Contains(bounded, "truncated") {
		t.Fatalf("unbounded native diagnostic length=%d", len(bounded))
	}
}
