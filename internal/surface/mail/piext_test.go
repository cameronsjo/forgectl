package mail

import (
	"bytes"
	"testing"
)

// The extension and PiAdapter speak one protocol, so the shipped copy must
// name the same environment variable and request types the adapter uses.
func TestPiExtensionEmbedded(t *testing.T) {
	for _, want := range []string{EnvInbox, EnvWorker, `"deliver"`, `"state"`, "--harness"} {
		if !bytes.Contains(PiExtension, []byte(want)) {
			t.Errorf("embedded pi extension does not mention %s", want)
		}
	}
}
