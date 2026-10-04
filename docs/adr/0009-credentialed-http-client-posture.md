# 0009. Credentialed HTTP client posture: keychain-sourced, host-pinned, written to only by separate grant

**Status: Accepted**

Date: 2026-09-07 (amended 2026-09-08 for the MCP server: §7, §8, §9; amended
2026-10-02 for closing a task: §12 to §17)

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

*Amended by §13: there are now two keychain entries, one per grant, and the
write verb never falls back to the read one. The keychain tool is run by
absolute path, so a `PATH` entry cannot supply a different token.*

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

*Amended by §14: "accepts public addresses on TLS" was, on its own, an
acceptance of any host a command line named. A keychain credential now goes
only to the default host or a host the user listed.*

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

"TLS is the control" depends on the system trust store: this client uses the
system roots. On a machine that carries an inspection root installed by device
management, a network device can present a certificate the handshake accepts
for this hostname. Neither working machine has been checked for one. With a
write credential in play, pinning the issuing CA for this client is the
strengthening to reach for first.

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

*Superseded for `forgectl tasks done` by §13: the CLI now has one write verb,
which reads its own keychain entry. The read verbs and the default read entry
are unchanged, and the sentence above still describes them. "The board's only
writer is the operator" stopped being true with §8 and is further from true
now: an agent can finish a task as well as file one.*

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

*Amended 2026-10-02: there are three write tools. `complete_task` is the
first that UPDATES a row, and its pre-read lives inside the client method, not
in the handler, so the CLI verb cannot skip it. The pre-read is also checked,
not only made (§12). The `created-by:` trailer now goes through the same
sanitizer as the `closed-by:` one (§15); before that, a client name holding a
line break could make the trailer span lines.*

**8a. Board text is untrusted input, and the fence is a mitigation, not a
boundary.** Anything that can file a task can write text an agent will read.
Every title, description, and comment this server returns is wrapped in a
per-response `<board-text-NONCE>` fence (8 hex, `crypto/rand`), with any
occurrence of the delimiter inside the text escaped — keyed on the delimiter
*prefix*, not on this response's nonce, so a title carrying some other
response's delimiter cannot survive either. Text that cannot be safely fenced
is dropped, never returned raw. A read tool's `structuredContent` sits outside
the fence, so it never carries board text: only ids, enums, counts, bools, and
server-canonicalised timestamps.

State the limit plainly: this gives the reading agent a stated frame and stops
the text from closing that frame itself. It does not make the text safe, and
an agent that ignores the frame is not protected by it. The controls that
actually bound damage are the token's scope and the gateway's tool
authorization.

*Amended by §16: an agent can now change an existing row, not only add one.*

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

**`--pin-ip` is REQUIRED with `--http`, not merely accepted there** — a security
review's finding, and the reasoning is worth keeping because the hole was
opened by *omitting* a flag rather than by setting one. With an empty list the
policy falls back to §3, and inside a container §3 has no live acceptance arm
left except "public: accepted": there is no `route` binary, so the gateway
lookup returns `""` and the private arm can never pass. A poisoned resolver
answering with an attacker-controlled public address would then be admitted,
with TLS as the sole remaining control — exactly the posture the pin exists to
replace, reached by leaving an argument out.

**11. The HTTP listener has no authentication of its own, and that is stated
rather than implied.** Cross-origin protection is applied explicitly (the SDK's
own guard is conditional on an environment variable and on a loopback bind, so
the default is no check at all on a process holding a write credential). It
stops a browser being walked into calling `create_task`; it stops nothing else.

Anything that can open a TCP connection to the port reaches every tool. The
access control is therefore entirely **network position and the gateway**: a
two-member network with the gateway as the only other peer, and the gateway's
own per-tool authorization. A deployment that publishes this port to a LAN has
no boundary left, and nothing in this binary would report that.

*Amended 2026-10-02: the listener now exposes an update as well as two
creates. The gateway authorizes by raw tool name and denies an unlisted tool,
so `complete_task` needs its own rule there, granted per consumer. Adding it
to the existing write rule would give it to every current write holder.*

**10. A startup assertion that the host is really Vikunja.** `AssertVikunja`
requires JSON with a `version` field from `GET /api/v1/info` before the server
accepts a single tool call. The estate's own reverse proxy answers an
unmatched host with `200` and a landing page, so without this a mis-pointed
server starts cleanly, passes its healthcheck, and fails hours later as a
confusing tool error rather than a refusal to start.

## Amendment, 2026-10-02 — closing a task

`forgectl tasks done <id>` and the MCP tool `complete_task` mark one task done.
Both go through `Client.CompleteTask`. This is the write verb the Consequences
section said would need its own decision and its own review; the decision is
recorded there.

**12. An update echoes the task the server returned, with two keys replaced.**
`CompleteTask` reads the task, copies the returned object value for value, sets
`done` and `description`, and posts it back. It does not build the body from
`tasks.Task`: that type models ten fields, and what the server does with a
field left out of an update is not something to find out on a live board.
Values stay raw JSON from read to write, so a number that does not fit a
`float64` is not rewritten on the way through.

The pre-read is checked before it is trusted as a request body. The object
must be a task with the id that was asked for, a boolean `done`, and a string
(or absent) `description`, within a size cap. Anything else is refused with
nothing written.

Three outcomes are refusals by decision:

- **A task that is already done is never written.** A second trailer on a done
  task would record a close that did not happen. This is also what makes the
  call safe to repeat.
- **A repeating task is refused**, whether it repeats by interval or by mode.
  Done on a repeating task means this occurrence, and what happens next
  belongs to the repeat rule.
- **A description that cannot take the trailer within the send limit is
  refused.** The description is never cut to make room, and a close is never
  sent without its record.

After an accepted update, the read-back decides the outcome, not the update's
own response. A proxy can answer `200` for a write the server never applied.
When the update was sent and nothing read afterwards shows the task done, the
result is "not confirmed", worded so the caller reads the task before
retrying. That error deliberately carries neither `ErrUnreachable` nor
`ErrUnauthorized`: one may be answered from cache and the other means a dead
credential, and neither is true of a write whose outcome is unknown.

Two edges of that rule, stated. An update answered with a `3xx` or `4xx` is
reported as refused and the task as unchanged, with no read-back: the status
is taken at its word there, so a proxy that answered `4xx` for a request the
server applied would produce a false "unchanged". And every other failure of
the update — a `5xx`, a timeout, and also a connection that was never made —
is reported as not confirmed. For a connection that was never made that is
the cautious answer, not the exact one. The one failure reported as not sent
is a dial the address pin refused, which keeps its own exit code.

**A lost update is accepted.** An edit made in the web UI between the pre-read
and the update is overwritten. The window is two requests wide. Two sessions
closing the same task at once is the same case: the second trailer replaces
the first, and both name real evidence.

**13. The CLI verb reads a second keychain entry and never falls back.**
`done` reads `--write-keychain-service` (default `vikunja-write`). It refuses
an explicit `--keychain-service`, and an absent write entry is an error that
names the flag — it does not try the read entry. Reusing the one flag would
teach every caller to pass the write entry to the read verbs too. The MCP
server is unchanged: it holds the token it was started with, so whoever can
open a task over MCP can close one over MCP with the same credential, and a
read-only token gets a tool error on `complete_task` as it does on
`create_task`.

The entry `done` reads holds a bot-user token scoped to task read and update
and shared only the projects that bot may close in. It is never a token minted
on the operator's own account. Any process running as the same user can read a
keychain entry, so **the token's scope and its project share are the boundary,
not this binary's checks.**

**14. A keychain credential goes only to the default host or a host the user
listed.** Until this amendment, `--host` on any `tasks` verb sent the named
keychain entry to whatever host was named, and §3 accepts any public address
on TLS. That was true of the read token from the start. It was tolerable for a
credential that can only read the operator's own data, and it is not tolerable
for a write credential on the same machine: one command in an agent's
permitted set would hand the token to a host of an attacker's choosing.

The rule has two enforcement points. Every `tasks` verb that reads the
keychain checks the host before the keychain read and exits `4` on a refusal.
And the token itself remembers which hosts it may go to: `ReadToken` takes the
allowed set, and `NewClient` refuses to build a client that would send a
keychain token anywhere else, so a caller that skips the CLI check still
cannot. A host is a plain hostname, compared lower-cased and exactly, against
the default host and the `allowed_hosts` list in the `[tasks]` section of the
user's config file. No repository-local or project config file can add one.

A refusal is recorded: one line in the close-record file names the verb, the
host that was asked for, and the keychain entry's name. Without it the attempt
would be blocked and then forgotten, and text on the board that told an agent
to send a credential elsewhere would still be there for the next agent. `done`
applies the rule before it checks its own arguments, so a refused host is
recorded even when the evidence or the id is also wrong. A command the
argument parser itself rejects — no id, an unknown flag — never reaches the
rule and leaves no line. The line keeps the host only when it is a plain
hostname; anything else is written as a fixed marker, because a URL can hold
a password or a token in more places than a filter can name. The refusal on
stderr still shows the value as typed. If the file cannot be written the
refusal stands, and one line on stderr says the record is missing. The lines
are not bounded in number: a caller that loops on a refused host grows the
file by one line per call.

Three limits, stated. The config path follows the user's home directory, and a
process that can write that file can add a host. This turns a one-command
exfiltration into a two-step one. **It is not a boundary against a process
running as the user** — the same argument this ADR makes against a
configurable gateway. And the list is one list for every keychain entry: a
host listed for any reason can be sent whichever keychain token a command
names, the write token included. With the default empty list a keychain token
only ever reaches the default host.

`mcp --http` is outside this rule. It reads `--token-file`, has no user config
in a container, and stays bounded by the required `--pin-ip` list (§9), which
refuses a public address even when listed.

**15. The trailer is provenance. It is not a record, and nothing may read it
to decide anything.** A close appends one line to the description:
`closed-by: <closer> via forgectl tasks <mcp|done> <UTC time> — <evidence>`.
Closer and evidence are caller text written onto a shared board, so both go
through one sanitizer, which the `created-by:` trailer now shares. The closer
is reduced to an allowlist that holds neither `:` nor `—`, with the word `via`
dropped, so a name cannot spell out trailer fields of its own. Evidence is
refused, not repaired, when it is not one line of visible text, is over its
limit, or holds a Vikunja API token (`tk_` and hex). That check knows one token
shape. A key or header of any other kind pasted into evidence is written to
the board, so evidence must never hold a credential. The CLI help says so; the
MCP tool's description does not, and the guidance agents follow has to.

The description is editable by anyone the project is shared with. So a
`closed-by:` line already on a task is never a reason to skip writing this
call's line: a matching last line is replaced, with or without a line break
after it. That keeps a retry from stacking trailers without letting planted
text stand in for the record.

Because the trailer can be rewritten, each surface has a record that is not on
the board:

| Surface | Record | Who can alter it |
|---|---|---|
| MCP over HTTP | the container log and the gateway's own log | whoever administers the deployment |
| MCP over stdio | stderr of the server process, and the close-record file | the same user |
| CLI `done` | stderr, and the close-record file | the same user |

One JSON line is written for every call that sent an update — closed, not
confirmed, refused, unauthorized — and for every refusal by the session cap.
It names the task, the closer, the evidence, the credential's source (never
the token), and the outcome. It does not go through the global logger, which
discards everything unless `log_level` is set. The close-record file sits in
the forgectl config directory; the same user can erase it, and it outlives the
session, which stderr does not. The line is written when the call returns, so
a process killed between the update and the return has sent an update and
left no line. The file is refused when it is a symlink, is not a regular file,
or is readable by group or other; the record then goes to stderr only and the
caller is told. That check guards against an accident, not against a process
running as the user.

On the HTTP transport the closer the container sees may be the gateway's
client name, not the end consumer's. The gateway log is what names the
consumer. On the CLI the closer is self-declared and defaults to `cli`, and
the credential source in the record is the keychain entry's name, which
defaults to the same entry for every caller. **So with default flags, two
harnesses on one machine write identical records and close as the same bot
user.** A harness that wants to be told apart names itself with `--closer`,
and the operator who wants the board to tell them apart gives each harness its
own bot and its own entry.

**16. An agent can now change an existing row, and the cap is a brake.**
§8a's argument covered rows an agent adds. `complete_task` changes rows that
already exist, so text an agent reads can now ask it to close something. Three
things bound that. The tool description tells an agent to close only a task
whose id it holds from its own work, never one found by title or in a list.
The call takes one id. And `complete_task` sends at most ten updates per MCP
session, counted under a lock, with a call that arrives without a session
counted in one shared bucket.

The cap counts updates sent, so a refused or unconfirmed update spends a slot
and an already-done task does not. It is a brake, not a boundary: a client
that opens a new session gets a new budget, and on a gateway that holds one
upstream session for every consumer the ten are shared. A slot is held from
before the pre-read, so more than ten calls in flight at once can be refused
before ten updates were sent. The CLI verb has no brake at all: each call is
its own process. What a credential may close at all is decided by the projects
its bot user is shared.

**17. What a wrong close costs, and who undoes it.** A close changes the done
state, its timestamps, the board column, and the description's last line.
There is no `reopen` verb: the operator reopens a task in the web UI. The
result of every close names the task's project and title so a wrong close is
visible to the caller at once.

*Not yet measured against the live instance:* that a close changes nothing
outside those keys, how the trailer renders, and what a reopen restores. A
probe behind the `liveprobe` build tag measures them. If it shows a close
alters more than this section says, `reopen` is owed.

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
- **That decision was taken on 2026-10-02 for one verb: marking a task done**
  (§12 to §17). The operator delegated the ruling to the session that built
  it, and the merge of that build is his confirmation of it. The verb does not
  create, delete, move, or retitle; each of those is still a separate decision.
  Security reviews bound it, before the build and after it; the first one's
  findings shaped §12 to §16.
  The first was an automated security review (Claude Opus) of the control at
  commit `5e6055f`: 1 Critical, 9 Important. The second, of the build at
  `92c2427`, found no path that sends a keychain token off the allowed hosts
  and no new path for the token into any output: 0 Critical, 3 Important. The
  later passes, of the fixes at `5458a4c`, `00f6831`, and `6cf8e4c`, found 0
  Critical and 0 Important each. Every Critical
  and Important is fixed in code or stated above as a limit, except one that
  cannot be closed by review: **the live probe has not run** (§17). No human
  has reviewed this build.

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
