## ADDED Requirements

### Requirement: Record an ask
`desk ask` SHALL record an ask with a title, optional markdown steps, a bucket (`now`, `soon`, `optional` or `coming`), links, who asked, an optional notify address, and the response kinds it allows (all four by default). It SHALL print `id=`, `bucket=` and `watch=` lines and exit 0 only when the ask is recorded.

#### Scenario: Notify self
- **WHEN** an agent passes `--notify self` inside an hc terminal
- **THEN** the ask records `$HC_TERMINAL_ID` as its notify address

### Requirement: Bounded ask text
`desk ask` MUST refuse a title that is empty, longer than 200 characters, or holds a control or format character; steps longer than 16 KiB or holding a control character other than newline and tab; and a link that is not an absolute `http` or `https` URL with a host and no user info.

#### Scenario: Hostile link
- **WHEN** an agent passes `--link javascript:alert(1)`
- **THEN** nothing is recorded and the exit code is 2

### Requirement: Append-only asks log
Asks, responses and notify outcomes SHALL be appended as one JSON record per line to `asks/log.jsonl` in the desk directory and never rewritten. The directory SHALL be mode 0700 and the log 0600, and the log MUST be opened without following a symlink. A writer SHALL hold an exclusive lock on the log across reading the current state, checking the change against it, and appending. A reader MUST skip a final line with no newline and SHALL skip, and count, a line that is not a well-formed record.

#### Scenario: Two answers at once
- **WHEN** two front ends answer the same open ask at the same moment
- **THEN** exactly one response is appended and the other is refused as already answered

#### Scenario: Symlinked log
- **WHEN** `asks/log.jsonl` is a symlink
- **THEN** every write is refused and the symlink's target is unchanged

### Requirement: Respond to an ask
A response SHALL be one of the kinds the ask allows. `done`, `reply` (non-empty text, at most 4000 characters) and `link` (an `http` or `https` URL) SHALL answer the ask; after that every response MUST be refused. `snooze` SHALL hide the ask until a time in the future at most 30 days away, after which it is open again.

#### Scenario: Disallowed kind
- **WHEN** an ask allows only `done` and the operator sends a reply
- **THEN** the reply is refused and nothing is appended

### Requirement: Notify only the asker
An ask MAY record a notify address only for the asker's own hc terminal (`$HC_TERMINAL_ID`); `desk ask` MUST refuse any other.

#### Scenario: Another agent's terminal
- **WHEN** an agent in terminal `t9z` passes `--notify t1k`
- **THEN** nothing is recorded and the exit code is 2

### Requirement: Notify line is data
The notify line SHALL say who answered and MUST quote every free-text field (reply, link, ask title) as data. It MUST carry no shell metacharacter.

#### Scenario: Hostile title
- **WHEN** an ask title holds a closing quote and an instruction
- **THEN** the title stays inside one quoted span and the line names the answerer

### Requirement: Notify the asking agent
When a response is recorded and the ask has a notify address, forgectl SHALL read that terminal and type one line with `hc send`, without `--force`, only when it is live, runs an agent harness hc recognizes, and is at its prompt. The outcome SHALL be appended to the log as a notify record. A failed or skipped notify MUST NOT undo the response.

#### Scenario: Terminal gone
- **WHEN** the notify address names no terminal
- **THEN** the response stands, the notify record says it failed and why, and the front end shows the failure

#### Scenario: Agent mid-turn
- **WHEN** the asking agent's terminal reports `working`
- **THEN** nothing is typed and the notify is recorded as refused

### Requirement: Response origin
Each response SHALL record its origin: `operator` when made in `desk ui` or `desk serve`, `agent` otherwise, `desk respond` included. A caller MUST NOT be able to set it. The notify line for an `agent` response MUST say it is not confirmed as the operator.

#### Scenario: Shell answer
- **WHEN** a process runs `desk respond ASK-ID done`
- **THEN** the response records origin `agent`

### Requirement: Open asks per asker
One asker MUST NOT hold more than 20 open or snoozed asks; `desk ask` SHALL refuse the next with an error naming the asker and the limit.

#### Scenario: Twenty-first ask
- **WHEN** an asker with 20 open asks asks again
- **THEN** nothing is recorded and the error names the limit

### Requirement: Watch for responses
`desk watch --responses ASK-ID` SHALL print each of that ask's responses as a JSON line and exit 0 once the ask is answered, including when it was answered before the watch started; it SHALL exit 1 when no such ask is in the log. With no ASK-ID it SHALL print every response as it arrives. On its deadline it SHALL exit 75 after printing `resume=` with a `--since` log offset that continues where it stopped. With no terminal on stdin and no `--deadline`, the deadline SHALL be 540 seconds.

#### Scenario: Snooze then reply
- **WHEN** a watched ask is snoozed and later answered with a reply
- **THEN** the watch prints both responses and exits 0 after the reply

### Requirement: Desk TUI
`desk ui` SHALL show open asks by bucket, then snoozed and recently answered asks, and the desk's queued scripts read-only with their sha256. It SHALL answer the selected ask with `d` (done, confirmed), `r` (reply), `l` (link) and `s` (snooze), offering only the kinds the ask allows, and refresh as the log changes. Every agent string MUST pass through the terminal-safe renderer.

#### Scenario: Escape sequence in a title
- **WHEN** an ask title holds an ESC byte
- **THEN** the TUI draws it escaped and no raw ESC reaches the terminal

### Requirement: Desk web page access
`desk serve` SHALL serve the same view as `desk ui` on a loopback address only, refusing any other bind, and MUST NOT offer to run a queued script. It MUST require a session established by POSTing the per-install token from `asks/web.token`, carried in the URL fragment. It MUST refuse a request whose Host is not loopback or a name given with `--allow-host`.

#### Scenario: No session
- **WHEN** a request without a session cookie asks for `/`
- **THEN** no ask text is returned

#### Scenario: Rebound host
- **WHEN** a request arrives with `Host: attacker.example`
- **THEN** it is refused before any handler runs

### Requirement: Desk web page writes
Every POST that changes state MUST carry the session cookie, a CSRF token bound to the session, and a same-origin fetch site or origin.

#### Scenario: Cross-site POST
- **WHEN** a POST to `/respond` arrives with `Sec-Fetch-Site: cross-site` or a wrong CSRF token
- **THEN** it is refused with 403 and nothing is appended

### Requirement: Desk web page renders agent text inert
Responses SHALL carry a Content-Security-Policy that forbids inline script and framing. Ask text MUST render inert: markdown with raw HTML off, and links limited to `http` and `https`.

#### Scenario: Script tag in steps
- **WHEN** an ask's steps contain `<script>` or an `onerror` attribute
- **THEN** the page shows them as text or drops them, and no script element or event handler reaches the page
