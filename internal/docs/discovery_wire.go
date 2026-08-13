package docs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"time"
	"unicode/utf8"
)

// wireServerInfo is the v1 record exactly as it appears on disk.
//
// Every field is TEXT, including the version number and the timestamp, and that
// is the whole point. Decoding straight into ServerInfo would let encoding/json
// accept `1.0` and `1e0` as schema version 1 and would accept a timestamp in
// any offset RFC3339 permits — so two byte sequences would name one record, and
// the filename-to-payload binding this format relies on would stop being exact.
// Holding the raw text lets the parser compare against the one canonical
// spelling and reject the rest.
//
// Token is a pointer so present-but-empty is distinguishable from absent.
// `"token": ""` is a record claiming a bearer token that cannot authenticate
// anything, which is a malformed record rather than an unprotected server.
type wireServerInfo struct {
	// SchemaVersion is held as raw bytes and validated by schemaProbe, not
	// here. json.Number would accept the STRING "1" — encoding/json unquotes
	// it and finds a valid number — so a typed version field would let a
	// record spell its version two ways.
	SchemaVersion json.RawMessage `json:"schema_version"`
	Generation    string          `json:"generation"`
	StartedAt     string          `json:"started_at"`
	Addr          string          `json:"addr"`
	Token         *string         `json:"token,omitempty"`
}

// schemaProbe is the lenient pre-pass. It reads nothing but the version.
//
// It exists because the strict decoder cannot tell "this is a v2 record" from
// "this is corrupt": DisallowUnknownFields reports a future record's new field
// as a parse failure, and the right answer for a future record is to skip it
// quietly. Reading the version first separates the two.
type schemaProbe struct {
	SchemaVersion *json.RawMessage `json:"schema_version"`
}

// encodeRecord renders a validated record as the canonical payload.
func encodeRecord(info ServerInfo) ([]byte, error) {
	payload, err := json.Marshal(struct {
		SchemaVersion uint   `json:"schema_version"`
		Generation    string `json:"generation"`
		StartedAt     string `json:"started_at"`
		Addr          string `json:"addr"`
		Token         string `json:"token,omitempty"`
	}{
		SchemaVersion: info.SchemaVersion,
		Generation:    info.Generation,
		StartedAt:     info.StartedAt.UTC().Format(time.RFC3339Nano),
		Addr:          info.Addr,
		Token:         info.Token,
	})
	if err != nil {
		return nil, fmt.Errorf("encode docs discovery record: %w", err)
	}
	payload = append(payload, '\n')
	if len(payload) > maxRecordBytes {
		return nil, errRecordTooLarge
	}
	return payload, nil
}

// parseRecord turns raw record bytes into a validated ServerInfo, or into one
// of the fixed rejection categories. It never returns anything derived from the
// bytes it read.
func parseRecord(raw []byte, localIP func(netip.Addr) bool) (ServerInfo, error) {
	if !utf8.Valid(raw) {
		return ServerInfo{}, errMalformedRecord
	}
	if err := rejectDuplicateKeys(raw); err != nil {
		return ServerInfo{}, err
	}

	// Version first, leniently: an unknown version is a different outcome
	// than a malformed record and must not be reported as one.
	var probe schemaProbe
	if err := json.Unmarshal(raw, &probe); err != nil || probe.SchemaVersion == nil {
		return ServerInfo{}, errMalformedRecord
	}
	switch version := string(bytes.TrimSpace(*probe.SchemaVersion)); {
	case version == "1":
	case looksLikeJSONNumber(version):
		// A number that is not 1 — including 1.0 and 1e0, which are the same
		// value spelled differently and so are not this version's spelling.
		return ServerInfo{}, errUnknownSchema
	default:
		// A quoted "1", a boolean, an object: not a version at all.
		return ServerInfo{}, errMalformedRecord
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var wire wireServerInfo
	if err := dec.Decode(&wire); err != nil {
		return ServerInfo{}, errMalformedRecord
	}
	// A second JSON value after the record means the file is not one record,
	// and whichever value a reader happened to stop at would be arbitrary.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return ServerInfo{}, errMalformedRecord
	}

	if wire.Generation == "" || wire.StartedAt == "" || wire.Addr == "" {
		return ServerInfo{}, errMalformedRecord
	}
	if !validGeneration(wire.Generation) {
		return ServerInfo{}, errInvalidGeneration
	}

	startedAt, err := parseCanonicalTime(wire.StartedAt)
	if err != nil {
		return ServerInfo{}, err
	}

	info := ServerInfo{
		SchemaVersion: docsSchemaVersion,
		Generation:    wire.Generation,
		StartedAt:     startedAt,
		Addr:          wire.Addr,
	}
	if wire.Token != nil {
		if !validDiscoveryToken(*wire.Token) {
			return ServerInfo{}, errInvalidToken
		}
		info.Token = *wire.Token
	}
	if err := validateServerInfo(info, localIP); err != nil {
		return ServerInfo{}, err
	}
	return info, nil
}

// looksLikeJSONNumber reports whether text is in the shape JSON reserves for a
// number, which is enough to tell "a version I do not implement" from "not a
// version at all".
func looksLikeJSONNumber(text string) bool {
	if text == "" {
		return false
	}
	c := text[0]
	return c == '-' || (c >= '0' && c <= '9')
}

// parseCanonicalTime accepts exactly the spelling the writer emits.
//
// The round-trip comparison is the check that matters: RFC3339Nano parses many
// forms that are the same instant (a "+00:00" offset, trailing zeros, a lower
// case "z"), and accepting them would mean one instant has several record texts
// while sorting compares parsed values. Requiring the canonical text keeps the
// on-disk form and the sort key in exact correspondence.
func parseCanonicalTime(text string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}, errInvalidStartedAt
	}
	if parsed.IsZero() {
		return time.Time{}, errInvalidStartedAt
	}
	if parsed.UTC().Format(time.RFC3339Nano) != text {
		return time.Time{}, errInvalidStartedAt
	}
	return parsed.UTC(), nil
}

// rejectDuplicateKeys walks the token stream looking for an object that names
// the same key twice.
//
// encoding/json silently keeps the LAST duplicate, so without this pass a
// record could carry a benign-looking `"addr"` for anything reading it casually
// and a second `"addr"` that is the one actually applied. The check has to run
// over the token stream because by the time Decode has finished, the losing
// value is gone.
func rejectDuplicateKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := walkForDuplicates(dec); err != nil {
		return err
	}
	return nil
}

// Recursion depth is bounded only by maxRecordBytes: an 8 KiB record cannot
// nest deeper than a few thousand levels, which the Go stack absorbs. Raising
// maxRecordBytes therefore requires revisiting this walk — the cap is the only
// thing standing between a hostile record and unbounded recursion here.
func walkForDuplicates(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return errMalformedRecord
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return errMalformedRecord
			}
			key, ok := keyTok.(string)
			if !ok {
				return errMalformedRecord
			}
			if _, dup := seen[key]; dup {
				return errDuplicateKeys
			}
			seen[key] = struct{}{}
			if err := walkForDuplicates(dec); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := walkForDuplicates(dec); err != nil {
				return err
			}
		}
	default:
		return errMalformedRecord
	}
	// Consume the matching closing delimiter.
	if _, err := dec.Token(); err != nil {
		return errMalformedRecord
	}
	return nil
}
