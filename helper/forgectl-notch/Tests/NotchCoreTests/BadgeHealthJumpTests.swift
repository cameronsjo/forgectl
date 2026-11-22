import Foundation
import Testing
@testable import NotchCore

let now = Date(timeIntervalSince1970: 1_795_100_000)

@Test func badgeLeadsWithRaiseThenCountThenAge() throws {
    let s = snap(qrow(k1, kind: "worker", at: now.addingTimeInterval(-25 * 60)),
                 qrow(k2, tier: .count, kind: "pr", at: now),
                 qrow(k3, tier: .count, kind: "a", at: now),
                 qrow("1111111111111111", tier: .count, kind: "a", at: now),
                 qrow("2222222222222222", tier: .hidden, kind: "q", at: now))
    let b = try #require(badge(for: s, now: now))
    #expect(b.text == "1 blocked · 3 · 25m")
    #expect(b.glyph == "▲")
    #expect(b.accessibilityLabel == "1 blocked, 3 more to look at, oldest 25m")
}

@Test func badgeShowsNoAgeUnder15Minutes() throws {
    let b = try #require(badge(for: snap(qrow(k1, at: now.addingTimeInterval(-60))), now: now))
    #expect(b.text == "1 waiting")
}

@Test func badgeIsNilWithOnlyHiddenRows() {
    #expect(badge(for: snap(qrow(k1, tier: .hidden, kind: "q", at: now)), now: now) == nil)
    #expect(badge(for: snap(), now: now) == nil)
}

@Test(arguments: [(59.0, "0m"), (25 * 60.0, "25m"), (65 * 60.0, "1h5m"), (120 * 60.0, "2h")])
func shortAges(_ s: Double, _ want: String) {
    #expect(shortAge(s) == want)
}

@Test func staleAfter90Seconds() {
    let h = health(last: snap(), lastGoodAt: now.addingTimeInterval(-120), failure: nil, now: now)
    #expect(h.stale == "last good read 2m ago")
    #expect(h.isGreyed(qrow(k1, at: now)))
}

@Test func aFailureNamesItself() {
    let h = health(last: snap(), lastGoodAt: now, failure: .timedOut, now: now)
    #expect(h.stale == "forgectl did not answer in 10 s")
}

@Test func onlyTheDegradedSourceGreysOut() {
    var src = Sources()
    src.desk = "error"
    let h = health(last: Snapshot(rows: [], sources: src), lastGoodAt: now, failure: nil, now: now)
    #expect(h.stale == nil)
    #expect(h.isGreyed(qrow(k1, kind: "desk", at: now)))
    #expect(!h.isGreyed(qrow(k2, kind: "drain", at: now)))
    #expect(h.reasons == ["desk: error"])
}

@Test func anOldSweepGreysSweepRows() {
    let src = Sources(sweep: "ok", sweepGeneratedAt: now.addingTimeInterval(-3 * 3600))
    let h = health(last: Snapshot(rows: [], sources: src), lastGoodAt: now, failure: nil, now: now)
    #expect(h.degraded[.sweep] == "sweep is 3h old")
}

@Test func clicksOffWithoutHerdrApp() {
    let src = Sources(core: "unavailable", coreDetail: "hc: not running")
    let h = health(last: Snapshot(rows: [], sources: src), lastGoodAt: now, failure: nil, now: now)
    #expect(h.clicksOff == "clicks off, herdr-app not reachable: hc: not running")
    let r = QueueRow(key: k1, tier: .raise, kind: "desk", text: "x", at: now, origin: "own", terminal: "t7")
    #expect(!canJump(r, health: h))
    let ok = health(last: snap(), lastGoodAt: now, failure: nil, now: now)
    #expect(canJump(r, health: ok))
    #expect(!canJump(qrow(k1, at: now), health: ok))
}

@Test func jumpOutcomes() {
    #expect(jumpResult(outcome: "ok", reason: nil) == .actedOn)
    #expect(jumpResult(outcome: "gone", reason: nil) == .failed("already gone from the queue"))
    #expect(jumpResult(outcome: "no-pane", reason: "terminal t7 finished") == .failed("terminal t7 finished"))
    #expect(jumpResult(outcome: "launch-missiles", reason: nil) == .failed("jump failed"))
    #expect(jumpResult(outcome: nil, reason: nil) == .failed("jump failed"))
}

@Test func actedOnDimsUntilTheRowLeaves() {
    var a = ActedOn()
    let w = qrow(k1, kind: "worker", at: now), d = qrow(k2, kind: "desk", at: now)
    a.mark(k1)
    a.mark(k2)
    #expect(a.label(for: w) == "acted on · waits for next sweep")
    #expect(a.label(for: d) == "acted on")
    a.prune(to: snap(w))
    #expect(a.label(for: d) == nil)
    #expect(a.label(for: w) != nil)
}

@Test func forgectlIsTheFirstPresentCandidateOnlyIfSigned() {
    let ok = Candidate(path: "/opt/homebrew/bin/forgectl", resolved: "/opt/homebrew/Caskroom/forgectl/0.69.0/forgectl", teamVerified: true)
    let missing = Candidate(path: "/opt/homebrew/bin/forgectl", resolved: nil, teamVerified: false)
    let unsigned = Candidate(path: "/opt/homebrew/bin/forgectl", resolved: "/tmp/evil", teamVerified: false)
    let second = Candidate(path: "/usr/local/bin/forgectl", resolved: "/usr/local/forgectl", teamVerified: true)
    #expect(chooseForgectl([ok, second]) == .success(ok.resolved ?? ""))
    #expect(chooseForgectl([missing, second]) == .success("/usr/local/forgectl"))
    // A present-but-unsigned first candidate is refused; no fall-through.
    #expect(chooseForgectl([unsigned, second]) == .failure(.notSigned("/tmp/evil")))
    #expect(chooseForgectl([missing]) == .failure(.notInstalled))
}

@Test func jumpArgumentsCarryOnlyAValidKey() {
    #expect(jumpArguments(key: k1) == ["attention", "jump", "--key=\(k1)", "--json"])
    #expect(jumpArguments(key: "--exec=sh") == nil)
}
