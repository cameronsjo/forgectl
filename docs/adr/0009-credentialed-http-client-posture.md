# 0009. Credentialed HTTP client posture: keychain-sourced, host-pinned, read-only by grant

**Status: Accepted**

Date: 2026-09-07 (amended 2026-09-08 for the MCP server: §7, §8, §9)

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

**3b. The CGNAT arm is uncorroborated, and TLS is the control there.**
`100.64.0.0/10` is accepted with no gateway check, on the premise that the
tailnet name is the sanctioned off-LAN path. That premise is narrower than
the range: RFC 6598 is *shared* carrier-grade NAT space, handed out by
mobile carriers, many ISPs, and some campus networks — so a split-horizon
resolver on such a network can steer the hostname to a CGNAT address that
belongs to a stranger, and this arm accepts it where the identical
situation in RFC1918 space is refused.

This is recorded rather than fixed, deliberately. The corroboration
available — requiring a `utun` interface to hold a `100.64/10` address —
buys a defence-in-depth layer behind a control that already holds: TLS
certificate verification means the wrong server fails the handshake and
never receives the token. The cost is a second interface-enumeration
dependency in the pin's hot path, on a policy that is already the most
platform-specific thing in this package. **So the honest statement is that
for the CGNAT range the pin is not the control — TLS is**, and anyone
weakening TLS verification on this path (an `InsecureSkipVerify`, a custom
`RootCAs`, a proxy that terminates it) removes the only thing standing
there. Revisit if a second credentialed client appears, or if the pin ever
has to stand alone.

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

*Superseded in part by §8: the stdio transport's default keychain entry is
still read-only and the sentence above still describes it. A separately-minted
write credential now exists for the container transport, held by a different
identity.*

## Amendment, 2026-09-08 — `forgectl tasks mcp`

`forgectl tasks mcp` serves the same client to an MCP client over stdio, or to
a container over streamable HTTP. That adds a second credential source, a
second host-pin arm, and a write grant. Each is a separate decision.

**7. The container transport reads its token from a FILE, and there is no
environment-variable source.** `--http` requires `--token-file` and refuses at
startup naming both flags. `ReadTokenFile` refuses a missing file, a directory
(the shape a bind mount takes when its source is absent on the host), a
group- or world-readable mode, an oversized file, and a value that is not
`tk_<40+ hex>` — naming the path in every refusal and the value in none.

The reasoning is about *readers*, not about secrecy in the abstract. An
environment variable is readable through `docker inspect`, through
`/proc/<pid>/environ`, and in the rendered compose file on disk. A mounted
file is the same disclosure class on disk and closes both runtime readers. It
is a narrowing, not a solution, and the mode check is what stops it being
undone by a default umask. `os.Getenv` must never become a third source here;
adding one would silently re-open both readers with nothing red to show for it.

**8. The write grant belongs to the CREDENTIAL, not to this binary.**
`create_task` and `add_comment` exist in the tool set unconditionally. What
either can actually do is decided by the token's scope and the bot user's
project permission — two gates, neither of them here. The stdio default
(`vikunja-readonly`) therefore gets a tool error on `create_task`, and
`scripts/mcp-stdio-smoke.sh` asserts exactly that, in an order that matters: a
passing read runs first, because an out-of-scope write and a revoked token
both answer `401` and the refusal means nothing without it.

Two things make a write attributable rather than anonymous. One bot user per
agent, so board history names which agent wrote a row; and a `created-by:`
trailer appended to every `create_task` description, so a row in the UI leads
back to the call that made it without a second lookup.

`create_task` pre-reads the project and refuses the write when that read
fails. Fail-closed by decision: a project the credential cannot read is one it
must not write to, and the write's own `401` could not have told the operator
which of the two it was.

**8a. Board text is untrusted input, and the fence is a mitigation, not a
boundary.** Anything that can file a task can write text an agent will read.
Every title, description, and comment this server returns is wrapped in a
per-response `<board-text-NONCE>` fence (8 hex, `crypto/rand`), with any
occurrence of the delimiter inside the text escaped — keyed on the delimiter
*prefix*, not on this response's nonce, so a title carrying some other
response's delimiter cannot survive either. Text that cannot be safely fenced
is dropped, never returned raw.

State the limit plainly: this gives the reading agent a stated frame and stops
the text from closing that frame itself. It does not make the text safe, and
an agent that ignores the frame is not protected by it. The controls that
actually bound damage are the token's scope and the gateway's tool
authorization.

**9. `--pin-ip` is a second, weaker trust arm, bounded to the HTTP
transport.** Inside a container the default gateway is the container network's,
never the homelab's, so §3's corroboration can never succeed there and the
client could not dial the LAN address it is deployed to reach.

`--pin-ip` replaces that corroboration with an operator-supplied allow list —
and it is an INTERSECTION, not a fallback. An address is dialed only when it is
in the list *and* the base policy admits it with the private-range arm keyed on
list membership. An unlisted private address stays refused; loopback,
link-local, unspecified, and multicast stay refused; and a **public address is
refused even when listed**, which is the arm that keeps a flag added to narrow
the client from being the thing that widens it. Both call sites honour the
list — `checkHostPinning` and `pinnedDialer` — because a list applied at only
one of them re-opens the check-once gap §3a had to close. The list is never
consulted when resolution fails: that path returns `ErrUnreachable` first, so
the list can never act as a set of addresses to dial anyway.

It is weaker than §3 because the operator asserts the address rather than the
network corroborating it. It is bounded to `--http` for that reason, and the
flag is refused on stdio.

**10. A startup assertion that the host is really Vikunja.** `AssertVikunja`
requires JSON with a `version` field from `GET /api/v1/info` before the server
accepts a single tool call. The estate's own reverse proxy answers an
unmatched host with `200` and a landing page, so without this a mis-pointed
server starts cleanly, passes its healthcheck, and fails hours later as a
confusing tool error rather than a refusal to start.

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

Settled 2026-09-08, flipping this ADR from Draft to Accepted:

- **Where the host-pinning policy lives.** It stays in `internal/tasks`. There
  is still exactly one credentialed client; `--pin-ip` added an arm to the
  same policy rather than a second consumer of it. Revisit when a genuinely
  different client appears, not when this one grows a flag.
- **Rotation and revocation trigger.** Rotation is triggered by the token's
  own `expires_at` (90 days for the container credential) or by any suspected
  disclosure. The procedure is not written here, deliberately — it is
  operational and lives with the deployment, in the homelab repo's
  `docs/runbooks/vikunja-bot-user.md` § Rotating a bot token, with the
  identity and expiry ledger in the estate's Vikunja identities runbook. What
  this ADR owes it is the invariant: a rotation is not proven by the absence
  of a `401`, because a dead token and an out-of-scope one answer alike — a
  passing read is the proof.
- **`HomelabGateway` stays a compile-time constant.** The case that would have
  argued for configuration — a second machine on a different gateway — turned
  out to be the container, and `--pin-ip` answers it directly and more
  narrowly than a configurable gateway would: a configurable gateway is a
  value an attacker-influenced config could set to whatever the resolver
  answers, whereas the pin list must name the address itself.
