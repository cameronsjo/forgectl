package wire

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// fixture reads a captured herdr reply from internal/herdr/testdata, the one
// fixture set both readers of herdr's wire format test against.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	root, err := os.OpenRoot("../testdata")
	if err != nil {
		t.Fatalf("open testdata: %v", err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Errorf("close testdata: %v", err)
		}
	}()
	b, err := root.ReadFile(name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// Mutation: drop the `env.Error.Code == ""` test in DecodeError and the
// "no code" case goes red; read "message" into Code and the captured case does.
func TestDecodeError(t *testing.T) {
	got, ok := DecodeError(fixture(t, "err_workspace_not_found.json"))
	if !ok || got.Code != "workspace_not_found" || got.Message != "workspace wNOPE not found" {
		t.Fatalf("captured refusal = %+v, %v", got, ok)
	}
	for name, raw := range map[string]string{
		"no code":                `{"error":{"message":"m"}}`,
		"empty code":             `{"error":{"code":"","message":"m"}}`,
		"null error":             `{"error":null}`,
		"code not a string":      `{"error":{"code":5}}`,
		"log line first":         "warn: x\n" + `{"error":{"code":"c"}}`,
		"two objects":            `{"error":{"code":"c"}}{"error":{"code":"d"}}`,
		"a result envelope":      `{"id":"x","result":{"type":"ok"}}`,
		"empty":                  ``,
		"not json":               `not json`,
		"renamed code (fixture)": string(fixture(t, "changed_error_envelope.json")),
	} {
		if got, ok := DecodeError([]byte(raw)); ok {
			t.Errorf("%s: read as refusal %+v", name, got)
		}
	}
	if got, ok := DecodeError([]byte("  {\"error\":{\"code\":\"c\"}}\n")); !ok || got.Code != "c" {
		t.Errorf("surrounding whitespace: %+v, %v", got, ok)
	}
}

// Mutation: make DecodeResult return the zero value instead of ErrNoResult for
// a nil result, and the missing/null/renamed cases go red.
func TestDecodeResult(t *testing.T) {
	type list struct {
		Workspaces *[]struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspaces"`
	}
	got, err := DecodeResult[list](fixture(t, "workspace_list.json"))
	if err != nil || got.Workspaces == nil || len(*got.Workspaces) == 0 {
		t.Fatalf("captured listing = %+v, %v", got, err)
	}
	for name, raw := range map[string]string{
		"result missing":           `{"id":"x"}`,
		"result null":              `{"id":"x","result":null}`,
		"an error envelope":        `{"error":{"code":"c"}}`,
		"renamed result (fixture)": string(fixture(t, "changed_result_envelope.json")),
	} {
		if _, err := DecodeResult[list]([]byte(raw)); !errors.Is(err, ErrNoResult) {
			t.Errorf("%s: err = %v, want ErrNoResult", name, err)
		}
	}
	for name, raw := range map[string]string{
		"not json":      `not json`,
		"empty":         ``,
		"result scalar": `{"result":3}`,
	} {
		if _, err := DecodeResult[list]([]byte(raw)); err == nil || errors.Is(err, ErrNoResult) {
			t.Errorf("%s: err = %v, want a decode error", name, err)
		}
	}
}

// Mutation, one per rule: delete the rule from CheckOperand and its row goes
// red; raise MaxOperandLen and "65 bytes" does (the limit is pinned as a
// number, not only relative to the constant).
func TestCheckOperand(t *testing.T) {
	for name, s := range map[string]string{
		"empty":       "",
		"flag":        "--index",
		"dash":        "-x",
		"newline":     "w1:t1\nw1:t2",
		"nul":         "w1\x00",
		"tab char":    "w1\t",
		"escape":      "w1\x1b[2J",
		"c1 control":  "w1\u0085",
		"over length": strings.Repeat("a", MaxOperandLen+1),
		"65 bytes":    strings.Repeat("a", 65),
	} {
		if err := CheckOperand(s); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, s := range []string{"w1", "w1:t3", "w7D:t17", "label with spaces", "fleet", "v1.2", strings.Repeat("a", MaxOperandLen)} {
		if err := CheckOperand(s); err != nil {
			t.Errorf("%q refused: %v", s, err)
		}
	}
}

// TestCheckOperandErrorOmitsTheValue: the value can come from the environment
// or a config file, so the reason never echoes it; callers decide what to show.
func TestCheckOperandErrorOmitsTheValue(t *testing.T) {
	const v = "-secretish"
	err := CheckOperand(v)
	if err == nil || strings.Contains(err.Error(), v) {
		t.Errorf("err = %v", err)
	}
}
