package launch

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return parsed
}

// forbiddenUsageFields are the identity and content classes #240 promises never
// to retain. The check runs against the encoded row and the struct's field
// names, so a field named after any of them fails here before it can ship.
var forbiddenUsageFields = []string{
	"cwd", "dir", "path", "project", "repo", "branch", "session_id", "sessionid",
	"title", "argv", "args", "prompt", "env", "environment", "task", "profile",
	"match", "effort", "permission", "sandbox", "host", "hostname", "user",
	"pid", "secret", "token", "hash", "digest", "fingerprint", "id",
}

func sampleUsageEvent() UsageEventV1 {
	return UsageEventV1{
		SchemaVersion: UsageSchemaVersion,
		TS:            "2026-08-13T19:42:17Z",
		Event:         UsageEventExecAttempt,
		Harness:       "claude",
		Model:         "opus",
		SessionMode:   UsageSessionNew,
		Posture:       UsagePostureDefault,
	}
}

func TestUsageEventV1_ExactKeysAndForbiddenFields(t *testing.T) {
	raw, err := json.Marshal(sampleUsageEvent())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]bool{
		"schema_version": true, "ts": true, "event": true, "harness": true,
		"model": true, "session_mode": true, "posture": true,
	}
	if len(decoded) != len(want) {
		t.Fatalf("encoded row has %d keys (%v), want exactly %d", len(decoded), decoded, len(want))
	}
	for key := range decoded {
		if !want[key] {
			t.Fatalf("unexpected key %q in encoded row", key)
		}
	}

	typ := reflect.TypeOf(UsageEventV1{})
	if typ.NumField() != len(want) {
		t.Fatalf("UsageEventV1 has %d fields, want exactly %d", typ.NumField(), len(want))
	}
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.ToLower(typ.Field(i).Tag.Get("json"))
		name := strings.ToLower(typ.Field(i).Name)
		for _, banned := range forbiddenUsageFields {
			if tag == banned || name == banned {
				t.Fatalf("UsageEventV1 field %s is a forbidden identity/content field", typ.Field(i).Name)
			}
		}
	}
}

func TestUsageEventV1_ExactRowShape(t *testing.T) {
	row, err := EncodeUsageEvent(sampleUsageEvent())
	if err != nil {
		t.Fatalf("EncodeUsageEvent: %v", err)
	}
	want := `{"schema_version":1,"ts":"2026-08-13T19:42:17Z","event":"exec_attempt","harness":"claude",` +
		`"model":"opus","session_mode":"new","posture":"default"}` + "\n"
	if string(row) != want {
		t.Fatalf("row =\n%s\nwant\n%s", row, want)
	}
}

func TestUsageModelLabel_OnlyHumanOutputSubstitutes(t *testing.T) {
	if got := UsageModelLabel(""); got != "(harness default)" {
		t.Fatalf("UsageModelLabel(%q) = %q, want the harness-default label", "", got)
	}
	if got := UsageModelLabel("opus"); got != "opus" {
		t.Fatalf("UsageModelLabel(\"opus\") = %q, want it unchanged", got)
	}
}

func TestEncodeUsageEvent_RejectsInvalidEnums(t *testing.T) {
	mutate := map[string]func(*UsageEventV1){
		"schema version":  func(e *UsageEventV1) { e.SchemaVersion = 2 },
		"event":           func(e *UsageEventV1) { e.Event = "session_end" },
		"harness":         func(e *UsageEventV1) { e.Harness = "aider" },
		"session mode":    func(e *UsageEventV1) { e.SessionMode = "restored" },
		"posture":         func(e *UsageEventV1) { e.Posture = "review" },
		"timestamp":       func(e *UsageEventV1) { e.TS = "yesterday" },
		"local timestamp": func(e *UsageEventV1) { e.TS = "2026-08-13T19:42:17-07:00" },
	}
	for name, apply := range mutate {
		t.Run(name, func(t *testing.T) {
			ev := sampleUsageEvent()
			apply(&ev)
			if _, err := EncodeUsageEvent(ev); !errors.Is(err, ErrUsageInvalidEvent) {
				t.Fatalf("EncodeUsageEvent error = %v, want ErrUsageInvalidEvent", err)
			}
		})
	}
}

func TestEncodeUsageEvent_OversizedModelRejectedBeforeWrite(t *testing.T) {
	ev := sampleUsageEvent()
	ev.Model = strings.Repeat("m", MaxUsageRowBytes)
	row, err := EncodeUsageEvent(ev)
	if !errors.Is(err, ErrUsageRowTooLarge) {
		t.Fatalf("EncodeUsageEvent error = %v, want ErrUsageRowTooLarge", err)
	}
	if row != nil {
		t.Fatalf("oversized encode returned %d bytes, want nil", len(row))
	}
}

func TestEncodeUsageEvent_AcceptsExactlyAtTheBound(t *testing.T) {
	ev := sampleUsageEvent()
	ev.Model = ""
	base, err := EncodeUsageEvent(ev)
	if err != nil {
		t.Fatalf("EncodeUsageEvent: %v", err)
	}
	ev.Model = strings.Repeat("m", MaxUsageRowBytes-len(base))
	row, err := EncodeUsageEvent(ev)
	if err != nil {
		t.Fatalf("EncodeUsageEvent at the bound: %v", err)
	}
	if len(row) != MaxUsageRowBytes {
		t.Fatalf("row is %d bytes, want exactly %d", len(row), MaxUsageRowBytes)
	}

	ev.Model += "m"
	if _, err := EncodeUsageEvent(ev); !errors.Is(err, ErrUsageRowTooLarge) {
		t.Fatalf("one byte over the bound = %v, want ErrUsageRowTooLarge", err)
	}
}
