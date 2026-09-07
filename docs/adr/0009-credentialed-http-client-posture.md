# 0009. Credentialed HTTP client posture: keychain-sourced, host-pinned, read-only by grant

**Status: Draft**

Date: 2026-09-07

## Context

Until `internal/tasks`, forgectl held no credential of its own. Every
authenticated remote call went out through a subprocess that owned its own
auth — `gh` for GitHub, `tea` for Gitea — so forgectl never read, stored,
formatted, or transmitted a secret. The HTTP clients that did exist
(`internal/bench/probe.go`, `internal/docs`, `internal/proxy`,
`internal/httpsrv`) are unauthenticated or serve locally.

`forgectl tasks` breaks that: it talks to a Vikunja instance over HTTPS with a
bearer token. That is a new posture for this binary, not just a new command,
and the decisions below are the ones that make it safe rather than convenient.

Three properties of the target instance shape the design:

- `auth.local.enabled` is `false` (OIDC only), so there is no password login
  and therefore no way to mint a JWT and ask the API what a token may do.
  A token cannot report its own scopes.
- The instance answers an out-of-scope **write** with `401` — the same status
  a revoked token gets on **everything**. "The write was refused" therefore
  proves nothing on its own about whether the credential is alive.
- Its hostname resolves to an RFC1918 address on the homelab LAN via
  split-horizon DNS, and to a Tailscale CGNAT address off it. The private
  address is not unique to that network.

## Decision

**1. The credential is read from the OS keychain per run, never stored.**
`ReadToken` shells `security find-generic-password -s <service> -w`. Nothing
is cached, written to disk, or read from an environment variable. The cache
file (`internal/config.TasksCachePath`) holds task, project, and label data
only — `tasks.Snapshot` has no credential field by construction.

**2. The token is a type, not a string.** `tasks.Token` holds its value behind
an unexported `func() string`, so `fmt`'s `%+v`, `slog`'s `TextHandler`, and
`encoding/json` all reach an address rather than the secret. `Header()` is the
single sanctioned reveal point and is set directly on an `*http.Request` —
the token never enters argv, which is why `curl -H` and `env -i KEY=val` were
both rejected as transports.

**3. The host is pinned by resolved address, not by name.** `classifyIP`
refuses loopback, link-local, unspecified, and multicast outright; accepts a
private-range answer (`net.IP.IsPrivate`, so IPv6 `fc00::/7` as well as the
three IPv4 blocks) *only* when this machine's own default gateway is
`HomelabGateway`; always accepts Tailscale's `100.64.0.0/10`; accepts public
addresses on TLS. The gateway check is the corroboration — without it, any
network's split-horizon DNS could point the name at a stranger's device on
the same private address and receive the bearer token.

**3a. The vetted addresses are what gets dialed.** Classifying at
construction and letting `http.Transport` resolve the name again at dial
time is two independent lookups, and a resolver that answers them
differently is *exactly* the adversary in 3 — so the check would report
success while the token went to the attacker's address. `checkHostPinning`
therefore returns the addresses it vetted and `NewClient` installs a
`DialContext` that dials only those. TLS still verifies the certificate
against the hostname, so substituting the address weakens nothing: a wrong
address now fails the handshake instead of receiving the credential.

**4. Unreachable, unauthorized, and refused are different outcomes, all the
way out.** `ErrUnreachable`, `ErrUnauthorized`, and `ErrHostRefused` are
distinct sentinels, matched with `errors.Is` and mapped to distinct process
exit codes (2, 3, and 4). A network failure may serve the cache **with its
age stated**; a `401` never may. A revoked token must fail loudly rather
than present as a stale-but-plausible board, and a pinning refusal must be
distinguishable from a malformed argument — collapsing it into the default
`1` means a script cannot alert on the security verdict.

**5. Status is asserted before the body is decoded.** Vikunja returns a JSON
*object* on error where a read returns an array, so a decoder aimed at a
slice would turn a `401` into a confident empty result. `ErrUnexpectedStatus`
exists to refuse decoding rather than to describe it.

**6. The grant is read-only, and that is a posture choice, not a limitation.**
The token carries no write scope. The board's only writer is the operator,
which is what makes usage of the board measurable as human use.

## Consequences

- Every capability claim about this token is **empirical**. The token cannot
  introspect, so `scripts/vikunja-probe-token.sh` in the homelab repo probes
  each read and each write once and grades the result. The read probes are
  load-bearing: because a scope denial and a dead credential both answer
  `401`, only a `200` on a read establishes that the credential is alive, and
  only then does a `401` on a write mean "refused for scope".
- A machine that is neither on the homelab LAN nor on the tailnet gets a
  refusal from host pinning, not a timeout. That is intended: the failure
  names the reason instead of looking like an outage.
- The keychain read is one subprocess per run. It is deliberately not cached
  in memory across commands, since each command is its own process.
- Adding a *write* verb is a separate decision with a separate review — it
  changes the blast radius from "read data that is already the operator's" to
  "mutate a store with no trash on project delete".

TODO — settle before flipping this ADR from Draft to Accepted:

- TODO: does the host-pinning policy belong in `internal/tasks`, or should it
  move to a shared `internal/net` policy layer once a second credentialed
  client exists? Recorded here as a one-client decision on purpose.
- TODO: name the token rotation and revocation trigger, and where it is
  written down.
- TODO: confirm whether `HomelabGateway` should stay a compile-time constant
  or become configuration once a second machine with a different gateway
  needs this command.
