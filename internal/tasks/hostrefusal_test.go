package tasks

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// decodeRefusalLine checks that out is exactly one line holding one JSON
// object, and returns it.
func decodeRefusalLine(t *testing.T, out string) map[string]any {
	t.Helper()
	if !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 {
		t.Fatalf("the record is not exactly one line: %q", out)
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(out), &line); err != nil {
		t.Fatalf("the record is not one JSON object: %v\n%s", err, out)
	}
	return line
}

func TestWriteHostRefusalRecord_IsOneLineWithTheFiveFields(t *testing.T) {
	var out bytes.Buffer
	when := time.Date(2026, 1, 2, 5, 4, 5, 0, time.FixedZone("plus two", 2*60*60))
	rec := HostRefusalRecord{Time: when, Verb: "ls", Host: "other.example", Credential: "vikunja-write"} //nolint:gosec // G101: Credential holds a keychain entry's name, not a credential
	err := WriteHostRefusalRecord(&out, rec)
	if err != nil {
		t.Fatalf("WriteHostRefusalRecord: %v", err)
	}
	got := decodeRefusalLine(t, out.String())
	want := map[string]any{ //nolint:gosec // G101: "credential" is a record field holding an entry's name
		"time":       "2026-01-02T03:04:05Z",
		"event":      "host_refused",
		"verb":       "ls",
		"host":       "other.example",
		"credential": "vikunja-write",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("record = %v, want %v", got, want)
	}
	// The reading order of a line, and `time` first: a reader of the close log
	// tells a record line from anything else by how it starts.
	if !strings.HasPrefix(out.String(), `{"time":"2026-01-02T03:04:05Z","event":"host_refused","verb":`) {
		t.Errorf("the record's fields are out of order: %s", out.String())
	}
	if HostRefusalEvent != "host_refused" {
		t.Errorf("HostRefusalEvent = %q, want host_refused", HostRefusalEvent)
	}
}

func TestWriteHostRefusalRecord_AZeroTimeMeansNow(t *testing.T) {
	var out bytes.Buffer
	before := time.Now().Add(-time.Minute)
	if err := WriteHostRefusalRecord(&out, HostRefusalRecord{Verb: "done", Host: "other.example"}); err != nil {
		t.Fatalf("WriteHostRefusalRecord: %v", err)
	}
	stamp, _ := decodeRefusalLine(t, out.String())["time"].(string)
	when, err := time.Parse(time.RFC3339, stamp)
	if err != nil || !strings.HasSuffix(stamp, "Z") {
		t.Fatalf("time = %q, want a UTC RFC3339 time (%v)", stamp, err)
	}
	if when.Before(before) {
		t.Errorf("time = %s, want the current time", stamp)
	}
}

// The host is whatever was typed after --host: it is the one field of this
// record an attacker chooses. It must not be able to start a second line, add
// a field, reach a terminal as a control sequence, or grow the file.
func TestWriteHostRefusalRecord_TheHostCannotLeaveItsField(t *testing.T) {
	for name, host := range map[string]string{
		"a line feed":       "evil.example\n{\"time\":\"x\",\"event\":\"host_refused\"}",
		"CRLF":              "evil.example\r\nsecond line",
		"a quote and brace": `evil.example","credential":"forged"}`,
		"an escape":         "evil\x1b[2J.example",
		"a line separator":  "evil" + string(rune(0x2028)) + ".example",
		"invalid UTF-8":     "evil\xff.example",
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			rec := HostRefusalRecord{Verb: "ls", Host: host, Credential: "vikunja-readonly"} //nolint:gosec // G101: Credential holds a keychain entry's name, not a credential
			if err := WriteHostRefusalRecord(&out, rec); err != nil {
				t.Fatalf("WriteHostRefusalRecord: %v", err)
			}
			got := decodeRefusalLine(t, out.String())
			keys := make([]string, 0, len(got))
			for key := range got {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			if want := []string{"credential", "event", "host", "time", "verb"}; !slices.Equal(keys, want) {
				t.Errorf("record keys = %v, want %v", keys, want)
			}
			if got["credential"] != "vikunja-readonly" || got["event"] != "host_refused" {
				t.Errorf("the host changed another field: %v", got)
			}
			// None of these is a plain hostname, so none of it is kept.
			if got["host"] != hostNotRecorded {
				t.Errorf("host = %q, want %q for a value that is not a plain hostname", got["host"], hostNotRecorded)
			}
			if strings.ContainsAny(out.String(), "\x1b\r") || strings.ContainsRune(out.String(), 0x2028) {
				t.Errorf("a control or a line separator reached the line raw: %q", out.String())
			}
		})
	}
}

// The record keeps a plain hostname and nothing else. A URL can hold a user, a
// password, or a token in its userinfo, its path, or its query, in any
// encoding, so a value that is not a plain hostname is not kept in any part.
func TestWriteHostRefusalRecord_KeepsOnlyAPlainHostname(t *testing.T) {
	token := "tk_" + strings.Repeat("ab12", 10)
	secret := "hunter2secret"
	for name, tc := range map[string]struct{ host, want string }{
		"a plain host":           {"other.example", "other.example"},
		"userinfo":               {"https://bot:" + secret + "@tasks.example/", hostNotRecorded},
		"password after the at":  {"tasks.example@bot:" + secret, hostNotRecorded},
		"a query string":         {"tasks.example?token=" + secret, hostNotRecorded},
		"a path":                 {"tasks.example/" + secret, hostNotRecorded},
		"a port":                 {"tasks.example:8443", hostNotRecorded},
		"percent-encoded":        {"bot%3A" + secret + "%40tasks.example", hostNotRecorded},
		"a token":                {"tasks.example/" + token, hostNotRecorded},
		"a token as a host name": {token, hostNotRecorded},
		"a very long value":      {strings.Repeat("a", 5000), hostNotRecorded},
		"multi-byte":             {strings.Repeat("é", 300), hostNotRecorded},
		"empty":                  {"", hostNotRecorded},
	} {
		var buf bytes.Buffer
		rec := HostRefusalRecord{Verb: "ls", Host: tc.host, Credential: "vikunja-readonly"} //nolint:gosec // G101: Credential holds a keychain entry's name, not a credential
		if err := WriteHostRefusalRecord(&buf, rec); err != nil {
			t.Fatalf("%s: WriteHostRefusalRecord: %v", name, err)
		}
		raw := buf.String()
		if strings.Contains(raw, secret) || strings.Contains(raw, token) {
			t.Errorf("%s: the line kept a credential: %s", name, raw)
		}
		if got := decodeRefusalLine(t, raw)["host"]; got != tc.want {
			t.Errorf("%s: host = %q, want %q", name, got, tc.want)
		}
		if len(raw) > 512 {
			t.Errorf("%s: the line is %d bytes; a refusal line is bounded", name, len(raw))
		}
	}
}

// The credential field is a keychain service NAME. A value that is not one is
// left out, so the record cannot be made to carry whatever was typed after
// --keychain-service — including a token typed there by mistake.
func TestWriteHostRefusalRecord_BlanksACredentialThatIsNotAServiceName(t *testing.T) {
	for _, name := range []string{"", "two words", "a;b", "a\nb", strings.Repeat("x", 65), fakeToken + "/x"} {
		var out bytes.Buffer
		if err := WriteHostRefusalRecord(&out, HostRefusalRecord{Verb: "ls", Host: "other.example", Credential: name}); err != nil {
			t.Fatalf("WriteHostRefusalRecord(%q): %v", name, err)
		}
		if got := decodeRefusalLine(t, out.String())["credential"]; got != "" {
			t.Errorf("credential %q was recorded as %q, want it left blank", name, got)
		}
		if name != "" && strings.Contains(out.String(), name) {
			t.Errorf("the refused name reached the record: %s", out.String())
		}
	}
}

type refusingWriter struct{ wrote int }

func (w *refusingWriter) Write([]byte) (int, error) {
	w.wrote++
	return 0, errors.New("sink refused")
}

func TestWriteHostRefusalRecord_ReportsAWriterItCouldNotUse(t *testing.T) {
	if err := WriteHostRefusalRecord(nil, HostRefusalRecord{Verb: "ls", Host: "other.example"}); err == nil {
		t.Error("WriteHostRefusalRecord(nil writer) = nil, want an error")
	}
	sink := &refusingWriter{}
	if err := WriteHostRefusalRecord(sink, HostRefusalRecord{Verb: "ls", Host: "other.example"}); err == nil {
		t.Error("WriteHostRefusalRecord = nil when the writer failed")
	}
	// One Write for the whole line: on a descriptor opened for append, that is
	// what keeps two processes' lines from interleaving.
	if sink.wrote != 1 {
		t.Errorf("the line was written in %d call(s), want one", sink.wrote)
	}
}

// The characters a keychain service name allows are enough to spell a token,
// so a name shaped like one is written as the empty string in both records.
func TestRecords_BlankATokenShapedCredentialSource(t *testing.T) {
	token := "tk_" + strings.Repeat("ab12", 10)

	var refusal bytes.Buffer
	if err := WriteHostRefusalRecord(&refusal, HostRefusalRecord{Verb: "ls", Host: "other.example", Credential: token}); err != nil {
		t.Fatalf("WriteHostRefusalRecord: %v", err)
	}
	var closed bytes.Buffer
	err := WriteCloseRecord(&closed, CloseRecord{
		TaskID: 1, Surface: SurfaceDone, Closer: "cli", Evidence: "owner/repo#1",
		Credential: token, Host: "tasks.example", Outcome: CloseOutcomeClosed,
	})
	if err != nil {
		t.Fatalf("WriteCloseRecord: %v", err)
	}
	for name, raw := range map[string]string{"refusal": refusal.String(), "close": closed.String()} {
		if strings.Contains(raw, token) {
			t.Errorf("%s record kept the token-shaped name: %s", name, raw)
		}
		if !strings.Contains(raw, `"credential":""`) {
			t.Errorf("%s record does not blank the credential source: %s", name, raw)
		}
	}
}
