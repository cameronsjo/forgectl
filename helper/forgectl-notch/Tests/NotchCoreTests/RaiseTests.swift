import Foundation
import Testing
@testable import NotchCore

let launch = Date(timeIntervalSince1970: 1_795_000_000)

func qrow(_ key: String, tier: Tier = .raise, kind: String = "desk", at: Date) -> QueueRow {
    QueueRow(key: key, tier: tier, kind: kind, text: "t-\(key.suffix(2))", at: at, origin: "own", terminal: nil)
}

func snap(_ rows: QueueRow...) -> Snapshot { Snapshot(rows: rows, sources: Sources()) }

@Test func firstReadRaisesOnlyRowsAfterLaunch() {
    var t = RaiseTracker(launchedAt: launch)
    let old = qrow(k1, at: launch.addingTimeInterval(-600))
    let new = qrow(k2, at: launch.addingTimeInterval(5))
    let d = t.ingest(snap(old, new), now: launch.addingTimeInterval(10))
    #expect(d.raised.map(\.key) == [k2])
    // The old one never raises later either.
    let d2 = t.ingest(snap(old, new), now: launch.addingTimeInterval(40))
    #expect(d2.raised.isEmpty)
}

@Test func aKeyRaisesOncePerRun() {
    var t = RaiseTracker(launchedAt: launch)
    _ = t.ingest(snap(), now: launch)
    let r = qrow(k1, at: launch.addingTimeInterval(60))
    #expect(t.ingest(snap(r), now: launch.addingTimeInterval(61)).raised.count == 1)
    #expect(t.ingest(snap(r), now: launch.addingTimeInterval(91)).raised.isEmpty)
    // Even after it leaves and comes back.
    _ = t.ingest(snap(), now: launch.addingTimeInterval(120))
    #expect(t.ingest(snap(r), now: launch.addingTimeInterval(150)).raised.isEmpty)
}

@Test func newRowsInOneReadShareOneRaise() {
    var t = RaiseTracker(launchedAt: launch)
    _ = t.ingest(snap(), now: launch)
    let a = qrow(k1, at: launch.addingTimeInterval(60)), b = qrow(k2, kind: "worker", at: launch.addingTimeInterval(61))
    let d = t.ingest(snap(a, b), now: launch.addingTimeInterval(62))
    #expect(d.raised.count == 2)
    #expect(d.announcement == "2 waits need you")
}

@Test(arguments: [Tier.count, Tier.hidden])
func onlyRaiseTierRaises(_ tier: Tier) {
    var t = RaiseTracker(launchedAt: launch)
    _ = t.ingest(snap(), now: launch)
    let q = qrow(k1, tier: tier, kind: "q", at: launch.addingTimeInterval(60))
    #expect(t.ingest(snap(q), now: launch.addingTimeInterval(61)).raised.isEmpty)
}

// A failed read raises nothing and changes nothing: the row raised before
// the failures does not raise again after them, and a failure before the
// first good read does not use up the first-read rule.
@Test func aFailedReadLeavesTheRaisedSetAlone() {
    var t = RaiseTracker(launchedAt: launch)
    #expect(t.ingest(.failure(.timedOut), now: launch).raised.isEmpty)
    let old = qrow(k3, at: launch.addingTimeInterval(-60))
    #expect(t.ingest(.success(snap(old)), now: launch.addingTimeInterval(5)).raised.isEmpty)
    let r = qrow(k1, at: launch.addingTimeInterval(60))
    #expect(t.ingest(.success(snap(old, r)), now: launch.addingTimeInterval(61)).raised.count == 1)
    for f in [ReadFailure.timedOut, .exited(1), .undecodable] {
        #expect(t.ingest(.failure(f), now: launch.addingTimeInterval(120)).raised.isEmpty)
    }
    #expect(t.ingest(.success(snap(old, r)), now: launch.addingTimeInterval(300)).raised.isEmpty)
}

@Test func snoozeHoldsRaisesAndReleasesThemOnce() {
    var t = RaiseTracker(launchedAt: launch)
    _ = t.ingest(snap(), now: launch)
    t.snooze(until: launch.addingTimeInterval(1800))
    let a = qrow(k1, at: launch.addingTimeInterval(60)), b = qrow(k2, at: launch.addingTimeInterval(70))
    #expect(t.ingest(snap(a, b), now: launch.addingTimeInterval(100)).raised.isEmpty)
    // b leaves during the snooze; only a is still waiting at expiry.
    let d = t.ingest(snap(a), now: launch.addingTimeInterval(1900))
    #expect(d.raised.map(\.key) == [k1])
    #expect(t.ingest(snap(a), now: launch.addingTimeInterval(1930)).raised.isEmpty)
}
