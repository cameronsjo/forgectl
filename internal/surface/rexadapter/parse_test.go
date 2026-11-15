// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package rexadapter

import (
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// TestParsersReadTheRecordedReplies: each parser reads the reply rex printed
// in the measurement (testdata, ids redacted).
func TestParsersReadTheRecordedReplies(t *testing.T) {
	s, err := parseWhoami([]byte(fixture(t, "whoami.json")))
	if err != nil || s.Revision != APIRevision || s.Version != "0.1.0-preview.19" ||
		s.InstanceID != "00000000-0000-4000-8000-000000000001" || !strings.HasPrefix(s.Endpoint, localEndpointPrefix) {
		t.Errorf("whoami = %+v, %v", s, err)
	}
	c, err := parseNew([]byte(fixture(t, "new.json")))
	if err != nil || c.Session != fxSession || c.Block != fxBlock {
		t.Errorf("new = %+v, %v", c, err)
	}
	if ss, err := parseSessions([]byte(fixture(t, "ls_empty.json"))); err != nil || len(ss) != 0 || ss == nil {
		t.Errorf("empty ls = %v, %v", ss, err)
	}
	if ss, err := parseSessions([]byte(fixture(t, "ls_one.json"))); err != nil || len(ss) != 1 || ss[0].ID != fxSession || ss[0].Label != "forgectl-measure-b5yoa" {
		t.Errorf("ls = %v, %v", ss, err)
	}
	if bs, err := parseBlocks([]byte(fixture(t, "block_ls_one.json"))); err != nil || len(bs) != 1 || bs[0].ID != fxBlock || bs[0].Label != "shell" {
		t.Errorf("block ls = %v, %v", bs, err)
	}
	if bs, err := parseBlocks([]byte(fixture(t, "block_ls_three.json"))); err != nil || len(bs) != 3 || bs[1].Label != "owner-probe" {
		t.Errorf("block ls of three = %v, %v", bs, err)
	}
	for name, want := range map[string]process{
		"process_running.json": {Running: true},
		"process_exit3.json":   {ExitCode: 3},
		"process_exit7.json":   {ExitCode: 7},
	} {
		if p, err := parseProcess([]byte(fixture(t, name))); err != nil || p != want {
			t.Errorf("%s = %+v, %v; want %+v", name, p, err, want)
		}
	}
}

// TestParsersFailClosed: a reply missing a field the parser needs, with a
// value of the wrong shape, or with more than one JSON value is an error,
// never a default.
func TestParsersFailClosed(t *testing.T) {
	who := fixture(t, "whoami.json")
	for name, raw := range map[string]string{
		"whoami without instance_id":   strings.Replace(who, `"instance_id"`, `"x"`, 1),
		"whoami without host_id":       strings.Replace(who, `"host_id"`, `"x"`, 1),
		"whoami without endpoint":      strings.Replace(who, `"endpoint"`, `"x"`, 1),
		"whoami without rex_version":   strings.Replace(who, `"rex_version"`, `"x"`, 1),
		"whoami revision as a string":  strings.Replace(who, `"api_revision": 14`, `"api_revision": "14"`, 1),
		"whoami with a control char":   strings.Replace(who, `server.sock`, `server\u001b.sock`, 1),
		"whoami instance with a space": strings.Replace(who, `"00000000-0000-4000-8000-000000000001"`, `"a b"`, 1),
		"two values":                   who + who,
		"empty":                        ``,
	} {
		if _, err := parseWhoami([]byte(raw)); err == nil {
			t.Errorf("parseWhoami accepted %s", name)
		}
	}
	for name, raw := range map[string]string{
		"no session":      `{"initial_windows":[{"block_ids":["` + fxBlock + `"]}]}`,
		"two windows":     `{"session_id":"` + fxSession + `","initial_windows":[{"block_ids":["` + fxBlock + `"]},{"block_ids":["` + fxBlock2 + `"]}]}`,
		"two blocks":      `{"session_id":"` + fxSession + `","initial_windows":[{"block_ids":["` + fxBlock + `","` + fxBlock2 + `"]}]}`,
		"no blocks":       `{"session_id":"` + fxSession + `","initial_windows":[{"block_ids":[]}]}`,
		"block is window": `{"session_id":"` + fxSession + `","initial_windows":[{"block_ids":["window:01a1db33-db13-7000-8000-000000000004"]}]}`,
	} {
		if _, err := parseNew([]byte(raw)); err == nil {
			t.Errorf("parseNew accepted %s", name)
		}
	}
	for name, raw := range map[string]string{
		"no sessions key": `{}`,
		"null sessions":   `{"sessions":null}`,
		"row without id":  `{"sessions":[{"label":"x"}]}`,
		"row bad id":      `{"sessions":[{"session_id":"x","label":"x"}]}`,
		"row no label":    `{"sessions":[{"session_id":"` + fxSession + `"}]}`,
	} {
		if _, err := parseSessions([]byte(raw)); err == nil {
			t.Errorf("parseSessions accepted %s", name)
		}
	}
	for name, raw := range map[string]string{
		"no blocks key":  `{}`,
		"null blocks":    `{"blocks":null}`,
		"row without id": `{"blocks":[{"label":"x"}]}`,
		"row no label":   `{"blocks":[{"block_id":"` + fxBlock + `"}]}`,
		"label too long": `{"blocks":[{"block_id":"` + fxBlock + `","label":"` + strings.Repeat("x", 129) + `"}]}`,
		"one bad row":    `{"blocks":[{"block_id":"` + fxBlock + `","label":"a"},{"block_id":"block:x","label":"b"}]}`,
	} {
		if _, err := parseBlocks([]byte(raw)); err == nil {
			t.Errorf("parseBlocks accepted %s", name)
		}
	}
	for name, raw := range map[string]string{
		"no last_exit":     `{"home":"/x"}`,
		"exit no code":     `{"last_exit":{}}`,
		"code as a string": `{"last_exit":{"exit_code":"3"}}`,
		"last_exit string": `{"last_exit":"running"}`,
		"not an object":    `[]`,
	} {
		if _, err := parseProcess([]byte(raw)); err == nil {
			t.Errorf("parseProcess accepted %s", name)
		}
	}
}

// TestNotFoundNeedsTheExactLineAndExitCode is the classification rule: only
// rc 3 with the one recorded not-found line, for exactly the id asked about,
// is absence.
func TestNotFoundNeedsTheExactLineAndExitCode(t *testing.T) {
	res := func(stderr string, code int) exec.SensitiveResult {
		return exec.SensitiveResult{Stderr: exec.BoundedOutputForTest([]byte(stderr), exec.OutputComplete), ExitCode: code}
	}
	recorded := fixture(t, "stderr_session_not_found.txt") // Error: session target "nosuchsession" not found
	if !notFound(res(recorded, 3), targetSession, "nosuchsession") {
		t.Fatal("the recorded not-found reply is not read as not found")
	}
	if notFound(res(recorded, 1), targetSession, "nosuchsession") {
		t.Error("rc 1 read as not found")
	}
	if notFound(res(recorded, 3), targetBlock, "nosuchsession") {
		t.Error("a session not-found read as a block not-found")
	}
	if notFound(res(recorded, 3), targetSession, "nosuch") {
		t.Error("a not-found for another target matched")
	}
	if notFound(res(fixture(t, "stderr_ambiguous.txt"), 3), targetBlock, "quick") {
		t.Error("an ambiguous target read as not found")
	}
	if notFound(res(recorded+"Error: and more\n", 3), targetSession, "nosuchsession") {
		t.Error("a second stderr line was ignored")
	}
	truncated := exec.SensitiveResult{Stderr: exec.BoundedOutputForTest([]byte(recorded), exec.OutputOverflowed), ExitCode: 3}
	if notFound(truncated, targetSession, "nosuchsession") {
		t.Error("a truncated stderr read as not found")
	}
}
