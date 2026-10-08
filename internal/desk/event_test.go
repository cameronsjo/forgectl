// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package desk

import (
	"maps"
	"testing"
)

// Every event shape the supervisor and the batch runner write parses back
// into the fields it was written with.
func TestParseEventReadsEveryShapeTheDeskWrites(t *testing.T) {
	for _, tc := range []struct {
		line string
		key  string
		want map[string]string
	}{
		{"RUN-START id=03-x pid=41 steps=3 jobs=4", EventRunStart, map[string]string{"id": "03-x", "pid": "41", "steps": "3", "jobs": "4"}},
		{"RUN-START id=03-x pid=41", EventRunStart, map[string]string{"id": "03-x", "pid": "41"}},
		{"STEP-START id=b deps=a,c", EventStepStart, map[string]string{"id": "b", "deps": "a,c"}},
		{"STEP-START id=a deps=", EventStepStart, map[string]string{"id": "a", "deps": ""}},
		{"STEP-END id=a rc=0 dur=1.5 reason=ok outputs=k1,k2 log=/d/done/03-x.d/steps/a.log", EventStepEnd,
			map[string]string{"id": "a", "rc": "0", "dur": "1.5", "reason": "ok", "outputs": "k1,k2", "log": "/d/done/03-x.d/steps/a.log"}},
		{"STEP-END id=a rc=0 dur=1.5 reason=ok outputs= log=/My Desk/done/03-x.d/steps/a.log", EventStepEnd,
			map[string]string{"id": "a", "rc": "0", "dur": "1.5", "reason": "ok", "outputs": "", "log": "/My Desk/done/03-x.d/steps/a.log"}},
		{"STEP-END id=p rc=124 dur=1.0 reason=timeout outputs= log=private", EventStepEnd,
			map[string]string{"id": "p", "rc": "124", "dur": "1.0", "reason": "timeout", "outputs": "", "log": "private"}},
		{"STEP-SKIP id=c reason=dep-failed:a", EventStepSkip, map[string]string{"id": "c", "reason": "dep-failed:a"}},
		{"STEP-WARN id=a msg=STEP_OUT line 2 is not key=value; dropped", EventStepWarn,
			map[string]string{"id": "a", "msg": "STEP_OUT line 2 is not key=value; dropped"}},
		{"RUN-END rc=1 reason=failed ok=2 failed=1 skipped=1", EventRunEnd,
			map[string]string{"rc": "1", "reason": "failed", "ok": "2", "failed": "1", "skipped": "1"}},
		{"RUN-END rc=0 reason=ok", EventRunEnd, map[string]string{"rc": "0", "reason": "ok"}},
		{"RUN-LOST id=03-x pid=41", EventRunLost, map[string]string{"id": "03-x", "pid": "41"}},
	} {
		ev, ok := ParseEvent(tc.line)
		if !ok || ev.Key != tc.key || !maps.Equal(ev.Fields, tc.want) {
			t.Errorf("ParseEvent(%q) = %+v, %v; want %s %v", tc.line, ev, ok, tc.key, tc.want)
		}
	}
}

// A hostile value stays inside its own field: msg= takes the rest of the
// line, so text that looks like more fields never becomes one, and a repeated
// key cannot replace the first.
func TestParseEventKeepsAHostileValueInItsField(t *testing.T) {
	ev, ok := ParseEvent("STEP-WARN id=a msg=x rc=0 reason=ok log=/etc/passwd \x1b[2J")
	if !ok {
		t.Fatal("not parsed")
	}
	if want := "x rc=0 reason=ok log=/etc/passwd \x1b[2J"; ev.Fields["msg"] != want {
		t.Errorf("msg = %q, want %q", ev.Fields["msg"], want)
	}
	for _, k := range []string{"rc", "reason", "log"} {
		if _, found := ev.Fields[k]; found {
			t.Errorf("text inside msg became field %s", k)
		}
	}
	if ev, _ := ParseEvent("RUN-END rc=0 reason=ok rc=1"); ev.Fields["rc"] != "0" {
		t.Errorf("rc = %q; a repeated key replaced the first", ev.Fields["rc"])
	}
	if ev, _ := ParseEvent("STEP-SKIP id=a reason=x=y"); ev.Fields["reason"] != "x=y" {
		t.Errorf("reason = %q; a value holding '=' was split", ev.Fields["reason"])
	}
}

func TestParseEventRejectsWhatTheDeskNeverWrites(t *testing.T) {
	for _, line := range []string{
		"",
		"RUN-STARTED id=a",
		"run-end rc=0",
		"RUN-END rc=0 reason ok",
		"RUN-END rc=0  reason=ok",
		"RUN-END =0",
		"[a] RUN-END rc=0 reason=ok",
	} {
		if ev, ok := ParseEvent(line); ok {
			t.Errorf("ParseEvent(%q) = %+v; want not ok", line, ev)
		}
	}
}
