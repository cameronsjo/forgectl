package bless

import (
	"strings"
	"testing"
)

// echoMarker is planted in a TOML value; a decode error must never carry it
// (#738). The sidecar and the store are files an attacker may plant, so
// their field values are named, never echoed, even capped.
const echoMarker = "AKIAIOSFODNN7EXAMPLE" //nolint:gosec // G101: AWS's documented example key, a fixture asserted never to echo

func TestDecode_ErrorsNeverEchoAValue(t *testing.T) {
	long := strings.Repeat("K", 300)
	decoders := map[string]func([]byte) error{
		"DecodeStore":    func(b []byte) error { _, err := DecodeStore(b); return err },
		"DecodeEnvelope": func(b []byte) error { _, err := DecodeEnvelope(b); return err },
	}
	bodies := []string{
		"schema = " + echoMarker + "\n",
		"schema = \"" + echoMarker + "\"\n",
		"schema = 1\n" + long + " = 1\n",
		"schema = 1\nalgo = \"" + echoMarker + long + "\"\nkey_id = \"sha256:" + strings.Repeat("0", 64) + "\"\nsignature = \"\"\n",
		"schema = 1\nalgo = \"" + AlgoECDSAP256SHA256 + "\"\nkey_id = \"" + echoMarker + long + "\"\nsignature = \"\"\n",
	}
	for name, decode := range decoders {
		for _, body := range bodies {
			err := decode([]byte(body))
			if err == nil {
				continue // a body the other decoder owns
			}
			msg := err.Error()
			if strings.Contains(msg, "AKIA") || strings.Contains(msg, strings.Repeat("K", 81)) {
				t.Errorf("%s(%q) error = %q echoes a value or an uncapped key", name, body, msg)
			}
		}
	}
}
