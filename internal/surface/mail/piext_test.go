package mail

import (
	"bytes"
	"testing"
)

// The extension and PiAdapter speak one protocol, so the shipped copy must
// name the same environment variable and request types the adapter uses.
func TestPiExtensionEmbedded(t *testing.T) {
	for _, want := range []string{EnvInbox, EnvWorker, EnvCoordinator, EnvLedger, EnvRunID, EnvDrainWorker, `"deliver"`, `"receipt"`, `"state"`, "--harness", `retry: true`, "accepted", "unconfirmed", "message_start", "session_start", "getEntries", "getSessionId", "session_id", "run_id", "identity-bound pi inbox requires protocol version 2", "identityError"} {
		if !bytes.Contains(PiExtension, []byte(want)) {
			t.Errorf("embedded pi extension does not mention %s", want)
		}
	}
}

func TestPiExtensionKeepsReceiptAndSocketOwnershipState(t *testing.T) {
	for _, want := range []string{"FORGECTL_INBOX_RECEIPTS", "receiptPath", "messageDigest", "ownedSocket", "statSync", "runId", "runMatches", "receipt belongs to a different pi run or session", "accepted.sessionId"} {
		if !bytes.Contains(PiExtension, []byte(want)) {
			t.Errorf("embedded pi extension does not mention %s", want)
		}
	}
	if !bytes.Contains(PiExtension, []byte("inbox path already exists; register a fresh socket path for this process")) {
		t.Error("extension does not fail closed on an existing socket")
	}
}

func TestPiExtensionRejectsIdentityMismatchBeforeDelivery(t *testing.T) {
	for _, want := range []string{"req.v !== 1 && req.v !== 2", "const identityFailure = identityError(req)", "if (identityFailure)", "pi.sendUserMessage(text"} {
		if !bytes.Contains(PiExtension, []byte(want)) {
			t.Errorf("embedded pi extension does not enforce %s", want)
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
	if !bytes.Contains(PiExtension, []byte("current pi session identity is unavailable")) {
		t.Error("extension does not require a current session identity")
	}
}
