package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestSurfaceMergeMachine(t *testing.T) {
	cmd := newSurfaceMergeMachineCmdWith(func() (string, error) { return "sjomba.local", nil })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "machine = \"f8e7a19c22c0\"\n" {
		t.Fatalf("output %q", out.String())
	}
	cmd = newSurfaceMergeMachineCmdWith(func() (string, error) { return "sjomba.local", nil })
	out.Reset()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil || !strings.Contains(out.String(), `"machine": "f8e7a19c22c0"`) || !strings.Contains(out.String(), `"salt": "forgectl-merge-v1"`) {
		t.Fatalf("json %q, %v", out.String(), err)
	}
	cmd = newSurfaceMergeMachineCmdWith(func() (string, error) { return "", errors.New("no host") })
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err == nil {
		t.Fatal("a host-name failure printed a value")
	}
}
