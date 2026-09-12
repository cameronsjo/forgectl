package sops

// Test plan for the encrypted-at-path assertion (driver.go)
//
// This is the check the whole feature's "proven encrypted" claim rests on, and
// the first version of it could not be made to go red on demand: it scanned
// for the first line whose trimmed text began with `leaf + ":"`, anywhere in
// the document, so a same-named encrypted key elsewhere satisfied it.
//
//   [x] Passes when the scalar at exactly the path is ENC[
//   [x] REFUSES when the scalar at the path is cleartext, even though an
//       encrypted same-named key appears EARLIER in the document — the
//       document-order dependence that made the old check unsound
//   [x] Refuses when it appears LATER too (order must not matter either way)
//   [x] Refuses when `sops:`'s own encrypted `mac` would have satisfied a
//       leaf named `mac`
//   [x] Refuses a missing path, a non-mapping ancestor, and a non-scalar leaf
//   [x] Refuses an unparseable document
//   [x] No refusal echoes the cleartext value

import (
	"strings"
	"testing"
)

func TestAssertEncryptedAtPath(t *testing.T) {
	const secret = "s3ntinel-VALUE-77x"

	cases := []struct {
		name    string
		doc     string
		path    []string
		wantErr string
	}{
		{
			name: "encrypted at the path",
			doc: "app:\n    token: ENC[AES256_GCM,data:abc,type:str]\n" +
				"sops:\n    mac: ENC[AES256_GCM,data:def,type:str]\n",
			path: []string{"app", "token"},
		},
		{
			// The case that proved the old line scan unsound. An encrypted
			// `token` in `app` appears FIRST; the actual target is the
			// cleartext `token` under `notes_unencrypted`. The old check
			// matched app's line and passed, so the run reported success with
			// the secret sitting in plaintext.
			name: "cleartext at the path, encrypted same-named key earlier",
			doc: "app:\n    token: ENC[AES256_GCM,data:abc,type:str]\n" +
				"notes_unencrypted:\n    token: " + secret + "\n" +
				"sops:\n    mac: ENC[AES256_GCM,data:def,type:str]\n",
			path:    []string{"notes_unencrypted", "token"},
			wantErr: "written in the clear",
		},
		{
			// The mirror image. Order must not decide the verdict in either
			// direction — the old check happened to be correct here, which is
			// exactly what made the bug hard to see.
			name: "cleartext at the path, encrypted same-named key later",
			doc: "notes_unencrypted:\n    token: " + secret + "\n" +
				"app:\n    token: ENC[AES256_GCM,data:abc,type:str]\n",
			path:    []string{"notes_unencrypted", "token"},
			wantErr: "written in the clear",
		},
		{
			// sops' own metadata block always contains an encrypted `mac`, so
			// a leaf named `mac` had a guaranteed false pass available to it.
			name: "a leaf named mac must not be satisfied by sops' own mac",
			doc: "block:\n    mac: " + secret + "\n" +
				"sops:\n    mac: ENC[AES256_GCM,data:def,type:str]\n",
			path:    []string{"block", "mac"},
			wantErr: "written in the clear",
		},
		{
			name:    "the path is absent",
			doc:     "app:\n    other: ENC[AES256_GCM,data:abc,type:str]\n",
			path:    []string{"app", "token"},
			wantErr: "could not be found",
		},
		{
			name:    "an ancestor is not a mapping",
			doc:     "app: a-scalar\n",
			path:    []string{"app", "token"},
			wantErr: "could not be found",
		},
		{
			name:    "the leaf is a mapping rather than a scalar",
			doc:     "app:\n    token:\n        nested: ENC[AES256_GCM,data:abc,type:str]\n",
			path:    []string{"app", "token"},
			wantErr: "not a scalar",
		},
		{
			name:    "the document does not parse",
			doc:     "app:\n  token: [unclosed\n",
			path:    []string{"app", "token"},
			wantErr: "does not parse",
		},
		{
			// A prefix match on the VALUE would be as wrong as one on the key:
			// a value that merely mentions the marker is not ciphertext.
			name:    "a value that only mentions the marker",
			doc:     "app:\n    token: 'see ENC[AES256_GCM, for details'\n",
			path:    []string{"app", "token"},
			wantErr: "written in the clear",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := assertEncryptedAtPath([]byte(c.doc), c.path)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("assertEncryptedAtPath: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("assertEncryptedAtPath returned nil, want a refusal")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), c.wantErr)
			}
			// The refusal reports that a value is in the clear; it must not
			// carry the value while doing so.
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error %q echoed the cleartext value", err.Error())
			}
		})
	}
}
