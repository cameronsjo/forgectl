import Foundation

/// The two places the forgectl cask links its binary, in order. No PATH
/// lookup and no shell: a login item's PATH is launchd's.
public let forgectlCandidates = ["/opt/homebrew/bin/forgectl", "/usr/local/bin/forgectl"]

/// The Developer ID team forgectl's release binaries are signed by.
public let forgectlTeamID = "5H883FMW6H"

/// What the app found at one candidate path.
public struct Candidate: Equatable, Sendable {
    public let path: String
    /// The path with symlinks resolved, or nil when nothing is there.
    public let resolved: String?
    /// Whether `codesign --verify -R` accepted the resolved file for the team.
    public let teamVerified: Bool

    public init(path: String, resolved: String?, teamVerified: Bool) {
        self.path = path
        self.resolved = resolved
        self.teamVerified = teamVerified
    }
}

/// Why no forgectl was accepted.
public enum LocateError: Error, Equatable, Sendable {
    case notInstalled
    case notSigned(String)

    /// The stale-reason line.
    public var reason: String {
        switch self {
        case .notInstalled: return "forgectl not found in /opt/homebrew/bin or /usr/local/bin"
        case .notSigned(let p): return "\(clean(p, max: 120)) is not signed by forgectl's team; running nothing"
        }
    }
}

/// The forgectl to run: the first candidate that exists, resolved, if and
/// only if it verifies for the team. A first candidate that exists but fails
/// is refused outright; the app never falls through to a second, so a
/// planted binary cannot win by failing the first check.
public func chooseForgectl(_ candidates: [Candidate]) -> Result<String, LocateError> {
    guard let first = candidates.first(where: { $0.resolved != nil }), let resolved = first.resolved else {
        return .failure(.notInstalled)
    }
    guard resolved.hasPrefix("/"), first.teamVerified else {
        return .failure(.notSigned(resolved))
    }
    return .success(resolved)
}

/// The argv for a jump on key. Only a valid key ever reaches it, as one
/// `--key=K` argument.
public func jumpArguments(key: String) -> [String]? {
    guard isValidKey(key) else { return nil }
    return ["attention", "jump", "--key=\(key)", "--json"]
}
