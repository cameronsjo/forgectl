package mail

import _ "embed"

// PiExtension is the forgectl-inbox pi extension (assets/forgectl-inbox.ts).
// It ships inside the binary so a launch can write it beside the ledger and
// load it for that one pi run; nothing installs it into pi globally.
//
//go:embed assets/forgectl-inbox.ts
var PiExtension []byte
