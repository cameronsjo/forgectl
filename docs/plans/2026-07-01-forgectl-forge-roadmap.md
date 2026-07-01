# forgectl — Roadmap (2026-07-01)

## Context

Grow forgectl from a tmux-UX helper into the **workbench forge** — the operations layer beside
cadence — made composable by a signed **workflow DSL** with a kanban **board** + **cockpit**,
headlined by a clean-room **`pr` reviewer** as the first marquee workflow. Spine is
**platform-first** (per the epic's original "platform spine"; the later
`cameronsjo/claude-configurations#202` re-scope elevated `pr` to flagship, which here becomes the
marquee *workflow* atop the platform). `launch` (#2, gateway auth + posture) already shipped in
v0.5.0. Provenance: epic #1, sub-epic #3, umbrella `cameronsjo/claude-configurations#206`, spec
`cameronsjo/claude-configurations#202`.

## Critical path (the spine)

#9 → #12 → #10 → #29

The spine is the **platform** — a workflow DSL (#9), the board that feeds it (#12), the signing
that makes executable workflows trustworthy (#10), and the flagship `pr` reviewer (#29) as the
first marquee workflow proving it. Omitting any one leaves forgectl a toolbox, not a forge — no
workaround makes it a *composable platform*. The individual commands are all supporting: each is
a nicer version of something you can still do by hand with `gh`/`git`/`tmux`. The scariest piece
is **#9 (the DSL)** — novel grammar + execution-model design — so it leads and gets a **design
spike in Now** to push the risk uphill before #10/#16 commit to its shape.

## Now

| # | Item | Scope | Depends on | ~ |
|---|------|-------|-----------|---|
| #9 | workflow DSL — **design spike** | Prototype the DSL grammar + execution model over the shipped `launch` primitive: file format, step model, how steps compose existing commands through `internal/exec` Runner. Output is a design note + walking-skeleton executor — **not** the full command. De-risks the scariest work before signing (#10) and the dogfood (#16) lock to its shape. | #2 (done) | ~ |
| #4 | `forgectl clean` | Reclaim dev cruft across projects. Independent quick-win. | — | — |
| #5 | `forgectl branch` | Prune stale/orphaned branches, local + remote. Independent quick-win. | — | — |
| #7 | `forgectl ghostty` | Theme/keybind integration — the smallest item, no deps. | — | — |
| #34 | chore: CHANGELOG + deferred findings | Optional CHANGELOG + wire the deferred cross-host `projects` review findings. Cleanup from a shipped module. | — | — |

## Next

| # | Item | Depends on | ~ |
|---|------|-----------|---|
| #12 | `forgectl board` — kanban work surface | #9 | ~ |
| #10 | sign & attest DSL files | #9 | ~ |
| #29 | `pr <ref>` clean-room worktree review (flagship) | #20, #19, #2 | ~ |
| #19 | `net` — cached internal-network reachability | — | — |
| #20 | `quarantine` — hide CLAUDE.md/AGENTS.md | — | — |
| #16 | beads → kanban migration (first real DSL workflow, dogfoods #9) | #9, #12 | — |
| #6 | `forgectl kickoff` — absorb intros orchestration | #2, #12 | — |
| #8 | `forgectl cmux` — workspace domain + session orchestration | #2 | — |
| #28 | `proj clone` — canonical {host}/{org}/{repo} layout | — | — |
| #30 | `pr local` — offline clean-room review | #29 | — |
| #31 | `pr prs`/`dash`/`pick` + reviewed-dimming | #29 | — |
| #32 | `pr poll` — auto-review daemon + LaunchAgent + tray | #29, #19 | — |

## Later

- **#15** — attest workflow **outputs** (signed review artifacts) — power-up atop signing (#10).
- **#17** — signed workflow **registry** (shareable batteries) — the "distribute the platform" ambition; waits on the DSL + signing proving out.
- **#13** — `forgectl status` cockpit — read-path composition; cheap once the others exist and there's state to observe.
- **#14** — `forgectl audit` (prompt-injection surface + secret hygiene) — needs quarantine (#20); security posture, not blocking.
- **#18** — `forgectl doctor` & `upgrade` — ecosystem health + safe self-update; boundary vs the `update` command (#25).
- **#21–#27** — the utility absorptions (`proxy`, `k8s`, `docker`, `pip`, `update`, `y`, `mcp`) — haziest / most optional; `proxy`/`k8s`/`mcp` branch on `net` (#19).
- **`cameronsjo/claude-configurations#196`** — `forgectl env` fail-closed C/R/U/D (ADR-0026), implements here; relates to `proxy` (#21).

## Verification / done

- One `docs/plans/2026-07-01-forgectl-forge-roadmap.md` exists with a 4-item spine (#9 → #12 → #10 → #29), Now fully detailed, Next/Later coarse.
- Spine is ~4 of ~29 items (~14%) — well under the ~60% smell line.
- **Tell-the-story test:** "forgectl is a tmux helper. First, ship a few clean utility wins (clean, branch, ghostty) and **design the workflow DSL** — the composition layer that turns the toolbox into a platform [Now]. Then build the **kanban board** as the live work surface, **sign the DSL** so executable workflows are trustworthy, and stand up the flagship **`pr` clean-room reviewer** as the first marquee workflow — dogfooding the DSL via the beads→kanban migration [Next]. Finally, layer on power-ups: attested outputs, a shareable registry, the status cockpit, audit, doctor, and the long tail of utility absorptions [Later]." Flows start-to-finish on the chart alone.

## Risks / open items

- **DSL shape is the keystone risk** — if the #9 spike lands on a bad grammar/execution model, #10 signing and #16 dogfood churn. That's exactly why the spike is Now.
- **Chicken-and-egg on usefulness** — the DSL only pays off once there are commands + a board to compose; if command absorption (#6/#8/#21–#28) all slips to Later, the platform has little to orchestrate. Keep at least the board (#12) and one real workflow (#16/#29) close behind #9.
- **`net`/`quarantine` classification** — treated as Next supporting primitives (enable the flagship #29), not spine. If a clean-room review proves impossible without them, they promote.
