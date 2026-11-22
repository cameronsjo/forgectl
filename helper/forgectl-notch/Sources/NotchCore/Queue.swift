import Foundation

/// A surface tier, as `attention list --json` reports it. An unknown value
/// reads as hidden: a newer forgectl can add a tier without this build
/// raising on it.
public enum Tier: String, Sendable {
    case raise
    case count
    case hidden
}

/// The source a row came from, for per-source staleness.
public enum Source: String, Sendable, CaseIterable {
    case spool
    case desk
    case sweep
    case drain
}

/// One row of the queue, after the app's own checks.
public struct QueueRow: Equatable, Sendable {
    public let key: String
    public let tier: Tier
    public let kind: String
    public let text: String
    public let at: Date
    public let origin: String
    /// The herdr-app terminal a jump can go to, when forgectl names one.
    public let terminal: String?

    public init(key: String, tier: Tier, kind: String, text: String, at: Date, origin: String, terminal: String?) {
        self.key = key
        self.tier = tier
        self.kind = kind
        self.text = text
        self.at = at
        self.origin = origin
        self.terminal = terminal
    }

    /// The source this row belongs to.
    public var source: Source {
        switch kind {
        case "desk": return .desk
        case "worker", "pr": return .sweep
        case "drain": return .drain
        default: return .spool
        }
    }
}

/// What `attention list --json` said about each source.
public struct Sources: Equatable, Sendable {
    public var spool: String
    public var desk: String
    public var sweep: String
    public var drain: String
    public var herdr: String
    public var core: String
    public var coreDetail: String?
    public var sweepGeneratedAt: Date?

    public init(spool: String = "ok", desk: String = "ok", sweep: String = "absent", drain: String = "absent",
                herdr: String = "unavailable", core: String = "ok", coreDetail: String? = nil, sweepGeneratedAt: Date? = nil) {
        self.spool = spool
        self.desk = desk
        self.sweep = sweep
        self.drain = drain
        self.herdr = herdr
        self.core = core
        self.coreDetail = coreDetail
        self.sweepGeneratedAt = sweepGeneratedAt
    }

    /// The reported state of source.
    public func state(of source: Source) -> String {
        switch source {
        case .spool: return spool
        case .desk: return desk
        case .sweep: return sweep
        case .drain: return drain
        }
    }
}

/// One good read of the queue.
public struct Snapshot: Equatable, Sendable {
    public let rows: [QueueRow]
    public let sources: Sources
    /// Rows dropped by the app's own checks (a bad key, a duplicate key).
    public let dropped: Int

    public init(rows: [QueueRow], sources: Sources, dropped: Int = 0) {
        self.rows = rows
        self.sources = sources
        self.dropped = dropped
    }
}

/// Why a read failed. A failed read changes nothing the app has decided.
public enum DecodeError: Error, Equatable {
    case notJSON
    case missingRows
}

/// Caps the app applies again, whatever forgectl already did.
public enum Caps {
    public static let text = 200
    public static let detail = 200
    public static let terminal = 16
    public static let maxRows = 500
}

private struct WireList: Decodable {
    let rows: [WireRow]?
    let sources: WireSources?
}

private struct WireRow: Decodable {
    let key: String?
    let tier: String?
    let kind: String?
    let text: String?
    let at: String?
    let origin: String?
    let terminal: String?
}

private struct WireSources: Decodable {
    let spool: String?
    let desk: String?
    let sweep: String?
    let drain: String?
    let herdr: String?
    let core: String?
    let coreDetail: String?
    let sweepGeneratedAt: String?
}

/// Decodes `forgectl attention list --json` as untrusted input: unknown
/// fields are ignored, a row with a malformed key is dropped, a repeated key
/// keeps its first row, every string is cleaned and cut again, and a body
/// that does not parse or has no rows list is an error (the read is stale).
public func decodeQueue(_ data: Data) throws -> Snapshot {
    let wire: WireList
    do {
        wire = try JSONDecoder().decode(WireList.self, from: data)
    } catch {
        throw DecodeError.notJSON
    }
    guard let wireRows = wire.rows else { throw DecodeError.missingRows }
    var rows: [QueueRow] = []
    var seen = Set<String>()
    var dropped = 0
    for w in wireRows.prefix(Caps.maxRows) {
        guard let key = w.key, isValidKey(key), !seen.contains(key), let kind = w.kind, let at = w.at.flatMap(parseTime) else {
            dropped += 1
            continue
        }
        seen.insert(key)
        let terminal = w.terminal.flatMap { isValidTerminal($0) ? $0 : nil }
        rows.append(QueueRow(
            key: key,
            tier: Tier(rawValue: w.tier ?? "") ?? .hidden,
            kind: clean(kind, max: 16),
            text: clean(w.text ?? "", max: Caps.text),
            at: at,
            origin: clean(w.origin ?? "", max: 16),
            terminal: terminal
        ))
    }
    dropped += max(0, wireRows.count - Caps.maxRows)
    let s = wire.sources
    let sources = Sources(
        spool: clean(s?.spool ?? "absent", max: 16),
        desk: clean(s?.desk ?? "absent", max: 16),
        sweep: clean(s?.sweep ?? "absent", max: 16),
        drain: clean(s?.drain ?? "absent", max: 16),
        herdr: clean(s?.herdr ?? "unavailable", max: 16),
        core: clean(s?.core ?? "unavailable", max: 16),
        coreDetail: s?.coreDetail.map { clean($0, max: Caps.detail) },
        sweepGeneratedAt: s?.sweepGeneratedAt.flatMap(parseTime)
    )
    return Snapshot(rows: rows, sources: sources, dropped: dropped)
}

/// A row key: exactly 16 lowercase hex characters.
public func isValidKey(_ s: String) -> Bool {
    s.utf8.count == 16 && s.utf8.allSatisfy { (48...57).contains($0) || (97...102).contains($0) }
}

/// A local hc terminal address: "t", a digit, then lowercase letters or
/// digits, at most 16 bytes (forgectl's hcadapter.ValidAddress).
public func isValidTerminal(_ s: String) -> Bool {
    let b = Array(s.utf8)
    guard b.count >= 2, b.count <= Caps.terminal, b[0] == 116, (48...57).contains(b[1]) else { return false }
    return b.dropFirst(2).allSatisfy { (48...57).contains($0) || (97...122).contains($0) }
}

/// Makes untrusted text one plain line, as forgectl's attention.Clean does:
/// tab, newline, return and other whitespace runs become one space; control,
/// format (bidi, zero-width, tag) and other invisible scalars are dropped; the
/// result is cut to max characters, the last an ellipsis when cut.
public func clean(_ s: String, max: Int) -> String {
    var out = ""
    var space = false
    for scalar in s.unicodeScalars {
        let p = scalar.properties
        if p.isWhitespace {
            space = true
            continue
        }
        switch p.generalCategory {
        case .control, .format, .lineSeparator, .paragraphSeparator, .privateUse, .unassigned, .surrogate:
            continue
        default:
            break
        }
        if space && !out.isEmpty {
            out.unicodeScalars.append(" ")
        }
        space = false
        out.unicodeScalars.append(scalar)
    }
    if max > 0 && out.count > max {
        var cut = String(out.prefix(max - 1))
        while cut.hasSuffix(" ") { cut.removeLast() }
        out = cut + "…"
    }
    return out
}

func parseTime(_ s: String) -> Date? {
    let f = ISO8601DateFormatter()
    f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    if let d = f.date(from: s) { return d }
    f.formatOptions = [.withInternetDateTime]
    return f.date(from: s)
}
