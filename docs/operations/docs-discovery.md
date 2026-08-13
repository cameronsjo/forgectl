# Docs server discovery — operations

How `forgectl docs open` finds a running `forgectl docs serve`, what upgrading
and rolling back do, and how to clear the state by hand when a crash leaves
residue behind.

## The state on disk

Two locations, one of them legacy:

| Path | Written by | Read by |
|---|---|---|
| `<config>/forgectl/docs-servers/<32 hex>.json` | current servers | current clients |
| `<config>/forgectl/docs-server.json` | servers before this change | current clients, as a fallback |

`<config>` is `~/Library/Application Support` on macOS and `~/.config` on Linux.

The directory is created `0700` and every record `0600`, because a record
carries the server's bearer token when one is in use.

Each running server publishes exactly one record, named for a 128-bit random
**generation** it minted at startup, and removes exactly that name when it
stops. No forgectl code path removes a record it did not write.

### Why a directory instead of one file

The single `docs-server.json` was shared mutable state. Two overlapping servers
both wrote it, and whichever stopped first deleted the pathname — taking the
other server's discoverability with it, even though that server was still
running and serving (forgectl#277).

A pathname cannot be owned if more than one process writes it. A record named
for a value only one process will ever hold can be.

## Selection

`docs open` picks a server like this:

1. Open the `docs-servers` directory. Missing is fine — that is the state
   before any current server has run, and the legacy record is consulted next.
2. Read at most 257 entries. At 257 it stops with an overload error rather than
   considering an arbitrary subset.
3. Parse the entries whose names are exactly 32 lowercase hex characters plus
   `.json`. Everything else is ignored. Malformed, oversized, foreign-owned,
   and future-schema records are skipped and counted.
4. Stop with an overload error at the 65th valid record.
5. Rank by start time, newest first, breaking ties by generation.
6. Ask each candidate, in rank order, whether it is still serving the
   generation its record names. The first one that answers wins.
7. If none answers, fall back to the legacy record.

Selection walks ranks **in order**, so which server wins never depends on which
probe finished first.

### What the freshness check proves, and what it does not

Each server answers `GET /.well-known/forgectl-docs` with `204` and its current
generation in the `X-Forgectl-Docs-Generation` header. The endpoint sits behind
the Host allowlist and the cross-site gate but ahead of bearer authentication,
requires no credential, and returns no body.

This is evidence of **freshness**, not identity. The generation is not a secret,
the answer is replayable, and a different listener can occupy the address
between the probe and the next request. It rules out the realistic failure —
a stale record pointing at a port the OS has since handed to something else —
and nothing stronger.

Two consequences:

- A **protected** current server is re-probed immediately before its token is
  sent. That narrows the window to roughly one round trip. It is not atomic.
- A **protected legacy** server is never sent its token at all, because it has
  no freshness endpoint and therefore no way to be checked first. `docs open`
  prints the URL and tells you to restart the server.

Discovery traffic uses a dedicated HTTP client with proxies disabled, redirects
refused, no cookie jar, and hard caps on connect time, headers, and body size.
Plain HTTP over a LAN remains observable to a network peer; if the reader is
reachable over Tailscale, the confidentiality comes from Tailscale.

## Upgrading

| Client | Server | Result |
|---|---|---|
| new | new | full discovery |
| new | old, unprotected | works through the legacy fallback |
| new | old, protected | found, but steering needs a restart; no token is sent |
| old | new | not discoverable — old clients only read `docs-server.json` |

Old and new servers can run at the same time. An old server's shutdown touches
only `docs-server.json` and cannot affect a current server's record.

The clean cutover is: upgrade the client, stop the old server, start a new one.

## Rolling back

1. Stop every current `docs serve` first, so each removes its own record.
2. Reinstall the older binary and start it. It recreates `docs-server.json`.
3. Leave `docs-servers/` alone, or remove it once no current server is running.
   Old binaries never look at it; leftover records are inert data.

Never automate deleting the legacy record, and never have a current server
write one. Both recreate the original cross-process deletion race.

## Clearing residue by hand

A server killed with `SIGKILL` cannot run its cleanup, so it leaves one record
behind. That is harmless: the record is skipped at the freshness check and
cannot mask a live sibling. Nothing collects it automatically, deliberately —
an automatic collector that deleted a temporarily stalled server's record would
turn a slow server into an undiscoverable one.

Residue only matters if it accumulates past the caps. When it does, `docs open`
fails with a message naming this fix:

```sh
# 1. Confirm nothing is serving.
pgrep -fl 'forgectl docs serve'

# 2. Remove the directory. macOS:
rm -rf ~/Library/Application\ Support/forgectl/docs-servers
# Linux:
rm -rf ~/.config/forgectl/docs-servers

# 3. Start a server again.
forgectl docs serve
```

Removing individual records is fine too. Do it only while the server that owns
the record is stopped: removing a live server's record does not stop the server,
it just makes it undiscoverable until restart.

Hidden `.tmp-*` files are publisher temps. Readers ignore them. One left behind
means a publisher died mid-write; deleting it is safe.

## When a server serves but is not discoverable

`docs serve` prints one warning and keeps serving when it cannot publish. The
server is fine; only `docs open` is affected. Reach it through the URL it
printed. Causes:

- **The bound address cannot be published.** The listener's address does not
  normalize to something a local reader can connect to — a scoped IPv6 address,
  or a non-TCP listener. Such a server exposes no freshness endpoint at all.
- **The discovery directory is unavailable.** No config directory could be
  resolved.
- **Publication failed.** A read-only filesystem, a directory this user does not
  own, or one that is group- or world-accessible.
- **Generation collisions were exhausted.** Eight in a row, which for 128-bit
  random names means something other than chance is producing the names.

A server that fails its own startup freshness probe is a different case: that is
a startup **failure** and the command exits, because a server that cannot answer
for its own generation is not serving correctly.

## Security boundary

The ownership guarantees here cover cooperating `forgectl` processes plus
exclusion of other OS users, through a `0700` directory and `0600` records.

They do **not** cover a hostile process running as the same user. Such a process
can already read bearer tokens out of that state and change the directory. The
lease is pathname ownership under cooperation, not a claim that a filename is an
inode identity against same-user tampering.

On macOS and Linux, records are opened through the pinned directory descriptor
and refused unless they are regular, single-linked, owned by this user, and not
group- or world-accessible. Other platforms keep forgectl compiling and broadly
working, but make no equivalent claim: mode bits are not Windows ACLs, and no
Windows runtime support is asserted.
