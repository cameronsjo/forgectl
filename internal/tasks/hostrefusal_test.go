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
	"unicode/utf8"
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
			if utf8.ValidString(host) && got["host"] != host {
				t.Errorf("host = %q, want it as given: %q", got["host"], host)
			}
			if strings.ContainsAny(out.String(), "\x1b\r") || strings.ContainsRune(out.String(), 0x2028) {
				t.Errorf("a control or a line separator reached the line raw: %q", out.String())
			}
		})
	}
}

func TestWriteHostRefusalRecord_CapsTheHost(t *testing.T) {
	if maxRecordedHostRunes != 253 {
		t.Fatalf("maxRecordedHostRunes = %d, want 253", maxRecordedHostRunes)
	}
	for name, host := range map[string]string{
		"ASCII":      strings.Repeat("a", 5000),
		"multi-byte": strings.Repeat("é", 5000),
	} {
		var out bytes.Buffer
		if err := WriteHostRefusalRecord(&out, HostRefusalRecord{Verb: "ls", Host: host}); err != nil {
			t.Fatalf("%s: WriteHostRefusalRecord: %v", name, err)
		}
		got, _ := decodeRefusalLine(t, out.String())["host"].(string)
		if n := utf8.RuneCountInString(got); n != maxRecordedHostRunes {
			t.Errorf("%s: host is %d runes, want it cut to %d", name, n, maxRecordedHostRunes)
		}
		if !strings.HasPrefix(host, got) {
			t.Errorf("%s: the cut host is not the front of the one given: %q", name, got)
		}
	}

	var out bytes.Buffer
	exact := strings.Repeat("a", maxRecordedHostRunes)
	if err := WriteHostRefusalRecord(&out, HostRefusalRecord{Verb: "ls", Host: exact}); err != nil {
		t.Fatalf("WriteHostRefusalRecord: %v", err)
	}
	if got := decodeRefusalLine(t, out.String())["host"]; got != exact {
		t.Errorf("a host at the cap was changed: %q", got)
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
