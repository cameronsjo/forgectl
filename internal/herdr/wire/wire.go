// Package wire decodes herdr's JSON reply envelopes and checks the operands
// forgectl passes to herdr. It is the one reader of herdr's wire format, shared
// by internal/herdr (the client) and internal/surface/herdradapter (the
// launcher), so a change to herdr's envelope is one edit here and one test set
// (cameronsjo/forgectl#722).
//
// It takes raw bytes and imports only the standard library and
// internal/termsafe's rune classifiers: the two callers read herdr through
// different runners (exec.Runner's string output and the sensitive runner's
// exec.BoundedOutput), and truncation, redaction and error presentation stay
// with each caller.
//
// Envelope keys match by exact name. encoding/json matches a struct field
// case-insensitively, so {"ERROR":{"CODE":"c"}} would read as a refusal and
// {"Result":…} as a result; a renamed envelope must fail closed instead
// (#999). Each envelope is therefore read as a map of raw members first, and
// only the exactly named member is decoded.
package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Refusal is herdr's structured refusal, {"error":{"code","message"}}. Code is
// herdr's own vocabulary (workspace_not_found, server_not_running, ...); match
// on it, never on Message. Message is herdr's raw text: a caller that keeps or
// renders it redacts it first.
type Refusal struct {
	Code    string
	Message string
}

// DecodeError returns the refusal in raw when raw is exactly one JSON object
// whose "error" member carries a non-empty string code. Anything else returns
// false: log lines before the JSON, a second object, an envelope with no code,
// or a result envelope. Surrounding whitespace is allowed.
//
// "error", "code" and "message" match only by exact name: a case-folded
// "ERROR" or "CODE" is not a refusal, and a folded "MESSAGE" is never read in
// place of "message".
func DecodeError(raw []byte) (Refusal, bool) {
	errRaw, ok, err := member(raw, "error")
	if err != nil || !ok {
		return Refusal{}, false
	}
	codeRaw, ok, err := member(errRaw, "code")
	if err != nil || !ok {
		return Refusal{}, false
	}
	var r Refusal
	if err := json.Unmarshal(codeRaw, &r.Code); err != nil || r.Code == "" {
		return Refusal{}, false
	}
	msgRaw, ok, err := member(errRaw, "message")
	if err != nil {
		return Refusal{}, false
	}
	if ok {
		if err := json.Unmarshal(msgRaw, &r.Message); err != nil {
			return Refusal{}, false
		}
	}
	return r, true
}

// member returns the member of the JSON object raw named exactly key. ok is
// false when raw is null or has no such member, and when the member is null.
// err is set when raw is not one JSON object (or null).
func member(raw []byte, key string) (json.RawMessage, bool, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false, err
	}
	v, ok := obj[key]
	if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
		return nil, false, nil
	}
	return v, true, nil
}

// ErrNoResult is returned by DecodeResult for a reply that is readable JSON
// but has no "result" member, or a null one. A renamed envelope lands here, and
// must not read as an empty result.
var ErrNoResult = errors.New("reply has no result")

// DecodeResult decodes the "result" member of herdr's {"id","result"} reply
// into T. The envelope's id is ignored and unknown fields are tolerated: herdr
// adds fields freely, and a strict decoder would turn every upgrade into an
// outage. A missing or null result returns ErrNoResult. Members of T that must
// be present belong behind pointers, so the caller can tell absent from empty.
//
// "result" matches only by exact name, so a case-folded "Result" is
// ErrNoResult. When T is a struct, a key of the result object that matches one
// of T's own JSON field names only under case folding is refused (a renamed
// "Workspaces" must not fill, or override, "workspaces"). Keys nested deeper
// than T's own fields are left to encoding/json.
func DecodeResult[T any](raw []byte) (T, error) {
	var zero T
	resRaw, ok, err := member(raw, "result")
	if err != nil {
		return zero, fmt.Errorf("decode reply: %w", err)
	}
	if !ok {
		return zero, ErrNoResult
	}
	if err := checkFoldedKeys(resRaw, reflect.TypeFor[T]()); err != nil {
		return zero, err
	}
	var out T
	if err := json.Unmarshal(resRaw, &out); err != nil {
		return zero, fmt.Errorf("decode reply: %w", err)
	}
	return out, nil
}

// errFoldedKey is checkFoldedKeys' refusal. It never carries the key: the
// reply is herdr's raw text.
var errFoldedKey = errors.New("decode reply: a result key differs from an expected one only in case")

// checkFoldedKeys refuses a key of the JSON object raw that encoding/json
// would match to one of struct type t's fields by case folding but that is
// not that field's exact name. raw that is not an object, and t that is not a
// struct, pass: the typed decode judges them.
func checkFoldedKeys(raw []byte, t reflect.Type) error {
	if t.Kind() != reflect.Struct {
		return nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	names := jsonFieldNames(t)
	for key := range obj {
		if names[key] {
			continue
		}
		for name := range names {
			if strings.EqualFold(key, name) {
				return errFoldedKey
			}
		}
	}
	return nil
}

// jsonFieldNames is the set of exact JSON names encoding/json gives t's
// exported, non-embedded fields: the tag's name, or the field's own name when
// the tag gives none. A field tagged "-" has none. Not covered: an embedded
// struct's promoted fields are skipped, so a folded key for one passes
// unchecked; no T decoded here embeds one.
func jsonFieldNames(t reflect.Type) map[string]bool {
	names := make(map[string]bool, t.NumField())
	for f := range t.Fields() {
		if !f.IsExported() || f.Anonymous {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		names[name] = true
	}
	return names
}

// MaxOperandLen bounds an operand in bytes. Real herdr ids are a dozen bytes
// and session names are short; the bound stops a runaway value reaching an
// argv. It is the stricter of the two limits the client and adapter kept
// before they shared this check.
const MaxOperandLen = 64

// The reasons CheckOperand refuses a value. None of them carries the value: it
// can come from the environment or a config file, so the caller decides
// whether and how to show it.
var (
	ErrEmptyOperand     = errors.New("is empty")
	ErrFlagOperand      = errors.New("starts with '-'")
	ErrLongOperand      = fmt.Errorf("is over %d bytes", MaxOperandLen)
	ErrControlOperand   = errors.New("has a control character")
	ErrInvisibleOperand = errors.New("has an invisible character")
	ErrEncodingOperand  = errors.New("is not valid UTF-8")
)

// CheckOperand reports whether s is safe as one herdr operand: non-empty, not
// read as a flag (no leading '-'), at most MaxOperandLen bytes, valid UTF-8,
// and free of control, bidi and invisible characters. The rune rules start
// from the hub picker's (termsafe.IsUnsafeTerminalRune and
// termsafe.IsInvisibleRune, #946/#967, #999) and admit operandJoiners on top:
// no rune reaches an argv as anything but data, but a label carrying a bidi
// override or a zero-width space would reach herdr's UI, or look like a
// different label than it is. It is the floor every operand
// meets; a caller with a narrower shape (a session name's charset) checks that
// on top. It applies no charset itself, because ids carry ':' and labels carry
// spaces.
func CheckOperand(s string) error {
	switch {
	case s == "":
		return ErrEmptyOperand
	case strings.HasPrefix(s, "-"):
		return ErrFlagOperand
	case len(s) > MaxOperandLen:
		return ErrLongOperand
	case !utf8.ValidString(s):
		return ErrEncodingOperand
	case strings.IndexFunc(s, termsafe.IsUnsafeTerminalRune) >= 0:
		return ErrControlOperand
	case strings.IndexFunc(s, refusedInvisible) >= 0:
		return ErrInvisibleOperand
	}
	return nil
}

// operandJoiners are the invisible runes a real herdr label carries, so
// CheckOperand admits them though the hub picker refuses them. ZWJ (U+200D)
// builds emoji sequences (man, ZWJ, laptop) and Indic conjuncts (Sinhala
// "shri"); ZWNJ (U+200C) is ordinary Persian and Indic spelling; VS15 and
// VS16 (U+FE0E, U+FE0F) choose text or emoji presentation (a red heart with
// VS16). Refusing them would leave an existing workspace with such a label
// impossible to target. None is a bidi control. The list lives here, not in
// termsafe, so the picker's predicate is unchanged.
var operandJoiners = map[rune]bool{'\u200c': true, '\u200d': true, '\ufe0e': true, '\ufe0f': true}

// refusedInvisible is termsafe.IsInvisibleRune less operandJoiners.
func refusedInvisible(r rune) bool {
	return termsafe.IsInvisibleRune(r) && !operandJoiners[r]
}
