package mail

import (
	"bytes"
	"testing"
)

// The extension and PiAdapter speak one protocol, so the shipped copy must
// name the same environment variable and request types the adapter uses.
func TestPiExtensionEmbedded(t *testing.T) {
	for _, want := range []string{EnvInbox, EnvWorker, `"deliver"`, `"state"`, "--harness", `retry: true`} {
		if !bytes.Contains(PiExtension, []byte(want)) {
			t.Errorf("embedded pi extension does not mention %s", want)
		}
	}
}

// Spike S3: on pi 1.0.4 a bare sendUserMessage during a run fails after the
// extension has answered ok, and agent_end comes before pi is done with the
// run. The shipped extension must name a mode on every send and call the
// worker idle only at agent_settled.
func TestPiExtensionClosesTheIdleRace(t *testing.T) {
	if bytes.Contains(PiExtension, []byte("sendUserMessage(text)")) {
		t.Error("extension still sends without a mode")
	}
	if bytes.Contains(PiExtension, []byte(`pi.on("agent_end"`)) {
		t.Error("extension still marks idle at agent_end")
	}
	if !bytes.Contains(PiExtension, []byte(`pi.on("agent_settled"`)) {
		t.Error("extension does not wait for agent_settled")
	}
}
