import Foundation

/// How long the last good read may age before everything greys out.
public let readStaleAfter: TimeInterval = 90
/// How old the sweep may be before its rows grey out.
public let sweepStaleAfter: TimeInterval = 2 * 60 * 60

/// Why the app's view of the queue is degraded right now.
public struct Health: Equatable, Sendable {
    /// Set when the whole read is stale: every row greys out.
    public let stale: String?
    /// Sources whose rows grey out, each with its one-line reason.
    public let degraded: [Source: String]
    /// Set when clicks are off (no herdr-app to jump through), with why.
    public let clicksOff: String?

    /// Whether row's source is degraded or the whole read is stale.
    public func isGreyed(_ row: QueueRow) -> Bool {
        stale != nil || degraded[row.source] != nil
    }

    /// The reason lines to show, whole-read first.
    public var reasons: [String] {
        var out: [String] = []
        if let stale { out.append(stale) }
        for s in Source.allCases {
            if let r = degraded[s] { out.append(r) }
        }
        if let clicksOff { out.append(clicksOff) }
        return out
    }
}

/// The last read attempt's outcome.
public enum ReadFailure: Error, Equatable, Sendable {
    case timedOut
    case exited(Int32)
    case undecodable
    case forgectlRefused(String)
}

/// Health from the last good snapshot (nil before the first), when it was
/// read, the last attempt's failure (nil when it succeeded), and `now`.
public func health(last: Snapshot?, lastGoodAt: Date?, failure: ReadFailure?, now: Date) -> Health {
    var stale: String?
    if let f = failure {
        switch f {
        case .timedOut: stale = "forgectl did not answer in 10 s"
        case .exited(let code): stale = "forgectl attention list exited \(code)"
        case .undecodable: stale = "forgectl's reply did not parse"
        case .forgectlRefused(let why): stale = clean(why, max: Caps.detail)
        }
    }
    if let at = lastGoodAt, now.timeIntervalSince(at) > readStaleAfter {
        stale = stale ?? "last good read \(shortAge(now.timeIntervalSince(at))) ago"
    }
    guard let snap = last else {
        return Health(stale: stale ?? "no read yet", degraded: [:], clicksOff: nil)
    }
    var degraded: [Source: String] = [:]
    for s in Source.allCases {
        let state = snap.sources.state(of: s)
        if state != "ok" && state != "absent" {
            degraded[s] = "\(s.rawValue): \(state)"
        }
    }
    if degraded[.sweep] == nil, let gen = snap.sources.sweepGeneratedAt, now.timeIntervalSince(gen) > sweepStaleAfter {
        degraded[.sweep] = "sweep is \(shortAge(now.timeIntervalSince(gen))) old"
    }
    var clicksOff: String?
    // "absent" means forgectl did not ask hc (no desk row names a terminal):
    // there is nothing to click through to, so it is not a fault.
    if snap.sources.core != "ok" && snap.sources.core != "absent" {
        let why = snap.sources.coreDetail.map { ": \($0)" } ?? ""
        clicksOff = "clicks off, herdr-app not reachable\(why)"
    }
    return Health(stale: stale, degraded: degraded, clicksOff: clicksOff)
}
