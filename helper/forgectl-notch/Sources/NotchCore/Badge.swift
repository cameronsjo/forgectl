import Foundation

/// The collapsed island's badge: never colour alone, so every state has a
/// glyph and a word.
public struct Badge: Equatable, Sendable {
    public let glyph: String
    public let text: String
    /// What VoiceOver reads.
    public let accessibilityLabel: String
}

/// How old the oldest raise row must be before the badge shows its age.
public let badgeAgeAfter: TimeInterval = 15 * 60

/// The badge for a snapshot at `now`, or nil when nothing shows. Raise rows
/// lead ("1 blocked", "2 waiting"); count rows follow as a number; the age of
/// the oldest raise row follows once it passes 15 minutes. Hidden rows never
/// count. Example: `1 blocked · 3 · 25m`.
public func badge(for snapshot: Snapshot, now: Date) -> Badge? {
    let raise = snapshot.rows.filter { $0.tier == .raise }
    let count = snapshot.rows.filter { $0.tier == .count }.count
    var parts: [String] = []
    var spoken: [String] = []
    let glyph: String
    if !raise.isEmpty {
        let blocked = raise.contains { $0.kind == "worker" || $0.kind == "drain" }
        let word = blocked ? "blocked" : "waiting"
        glyph = "▲"
        parts.append("\(raise.count) \(word)")
        spoken.append("\(raise.count) \(word)")
    } else if count > 0 {
        glyph = "●"
    } else {
        return nil
    }
    if count > 0 {
        parts.append("\(count)")
        spoken.append("\(count) more to look at")
    }
    if let oldest = raise.map(\.at).min(), now.timeIntervalSince(oldest) >= badgeAgeAfter {
        let age = shortAge(now.timeIntervalSince(oldest))
        parts.append(age)
        spoken.append("oldest \(age)")
    }
    return Badge(glyph: glyph, text: parts.joined(separator: " · "), accessibilityLabel: spoken.joined(separator: ", "))
}

/// Whole minutes under an hour ("25m"), then hours and minutes ("1h5m",
/// "2h").
public func shortAge(_ seconds: TimeInterval) -> String {
    let minutes = max(0, Int(seconds) / 60)
    if minutes < 60 { return "\(minutes)m" }
    let h = minutes / 60, m = minutes % 60
    return m == 0 ? "\(h)h" : "\(h)h\(m)m"
}
