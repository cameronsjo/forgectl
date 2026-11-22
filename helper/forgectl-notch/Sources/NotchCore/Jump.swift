import Foundation

/// `forgectl attention jump --json`'s outcome.
public enum JumpOutcome: String, Sendable {
    case ok
    case gone
    case ambiguous
    case noPane = "no-pane"
    case herdrUnavailable = "herdr-unavailable"
}

/// What a row shows after a jump.
public enum JumpResult: Equatable, Sendable {
    /// Focus moved. The row is dimmed until its source refreshes.
    case actedOn
    /// Focus did not move; show this inline for 5 s.
    case failed(String)
}

/// The row message for a jump's outcome. An outcome this build does not know
/// is a failure, never a success.
public func jumpResult(outcome raw: String?, reason: String?) -> JumpResult {
    guard let raw, let outcome = JumpOutcome(rawValue: raw) else {
        return .failed("jump failed")
    }
    let why = reason.map { clean($0, max: 80) }.flatMap { $0.isEmpty ? nil : $0 }
    switch outcome {
    case .ok: return .actedOn
    case .gone: return .failed("already gone from the queue")
    case .ambiguous: return .failed("more than one row matched")
    case .noPane: return .failed(why ?? "no terminal to go to")
    case .herdrUnavailable: return .failed(why ?? "herdr-app is not reachable")
    }
}

/// Rows Cameron acted on whose source has not refreshed yet. A key leaves when
/// it leaves the queue.
public struct ActedOn: Sendable {
    private(set) var keys: Set<String> = []

    public init() {}

    public mutating func mark(_ key: String) {
        keys.insert(key)
    }

    /// Forget keys no longer in the snapshot.
    public mutating func prune(to snapshot: Snapshot) {
        keys.formIntersection(snapshot.rows.map(\.key))
    }

    /// The dimming label for row, or nil.
    public func label(for row: QueueRow) -> String? {
        guard keys.contains(row.key) else { return nil }
        return row.source == .sweep ? "acted on · waits for next sweep" : "acted on"
    }
}

/// Whether a row offers a jump at all: it names a terminal and clicks are on.
public func canJump(_ row: QueueRow, health: Health) -> Bool {
    row.terminal != nil && health.clicksOff == nil
}
