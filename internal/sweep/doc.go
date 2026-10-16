// Package sweep builds the chief-of-staff ledger (schema 2) that
// `forgectl sweep` prints and writes to $XDG_STATE_HOME/cadence/sweep.json.
//
// The ledger answers one question: of the work the sweep's workers started,
// what waits on the operator? It joins four sources, each injectable so the
// package runs offline: GitHub (open and merged PRs), the workers' brief
// directories, `herdr agent list`, and the desk's waiting items.
//
// Every claim in the ledger is a claim. A review marker is text any session
// under the operator's gh identity can post, so a PR is `marker-clean`, never
// `ready`: the reviewer allowlist is a claim filter, not an authenticity
// check. Text from GitHub and from brief files is untrusted; it is stripped of
// control, bidi and invisible characters and cut before it enters the ledger.
package sweep
