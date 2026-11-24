import Foundation

/// What one read asks the island to do.
public struct RaiseDecision: Equatable, Sendable {
    /// The rows this read raises, in queue order. All of them share one
    /// expand and one announcement; empty means stay collapsed.
    public let raised: [QueueRow]

    /// The one accessibility announcement for this read, or nil.
    public var announcement: String? {
        switch raised.count {
        case 0: return nil
        case 1: return "Needs you: \(raised[0].text)"
        default: return "\(raised.count) waits need you"
        }
    }
}

/// Decides which raise-tier rows expand the island. The rules:
///
/// - a key raises at most once per app run;
/// - on the first good read after launch, only rows whose `at` is after
///   launch raise, so a restart does not replay every wait;
/// - only a good read changes the set of keys already raised;
/// - while snoozed, new raise rows are held, and at the first good read after
///   the snooze ends those still in the queue raise once together.
public struct RaiseTracker: Sendable {
    public let launchedAt: Date
    private(set) var seen: Set<String> = []
    private(set) var firstReadDone = false
    private(set) var snoozedUntil: Date?
    private(set) var heldDuringSnooze: Set<String> = []

    public init(launchedAt: Date) {
        self.launchedAt = launchedAt
    }

    /// Snooze every raise until `until`.
    public mutating func snooze(until: Date) {
        snoozedUntil = until
    }

    /// Whether raises are held at `now`.
    public func isSnoozed(at now: Date) -> Bool {
        guard let until = snoozedUntil else { return false }
        return now < until
    }

    /// Ingests one read attempt at `now` and returns what it raises. A failed
    /// read raises nothing and changes nothing: not the keys already raised,
    /// not the first-read rule, not a held snooze.
    public mutating func ingest(_ read: Result<Snapshot, ReadFailure>, now: Date) -> RaiseDecision {
        guard case .success(let snapshot) = read else {
            return RaiseDecision(raised: [])
        }
        return ingest(snapshot, now: now)
    }

    /// Ingests one good read at `now` and returns what it raises.
    public mutating func ingest(_ snapshot: Snapshot, now: Date) -> RaiseDecision {
        let raiseRows = snapshot.rows.filter { $0.tier == .raise }
        let present = Set(raiseRows.map(\.key))
        var fresh = raiseRows.filter { !seen.contains($0.key) }
        if !firstReadDone {
            fresh = fresh.filter { $0.at > launchedAt }
            firstReadDone = true
        }
        seen.formUnion(present)

        if isSnoozed(at: now) {
            heldDuringSnooze.formUnion(fresh.map(\.key))
            return RaiseDecision(raised: [])
        }
        var keys = Set(fresh.map(\.key))
        if snoozedUntil != nil {
            // The snooze has ended: raise what it held, if still waiting.
            keys.formUnion(heldDuringSnooze.intersection(present))
            heldDuringSnooze = []
            snoozedUntil = nil
        }
        return RaiseDecision(raised: raiseRows.filter { keys.contains($0.key) })
    }
}
