import Foundation

// The label becomes a filename (`<label>.key`), so it must not contain path
// separators or lead with a dot. Enforce a strict allowlist and require the
// WHOLE string to match — a partial match (e.g. a trailing newline) is rejected.
private let labelPattern = "^[A-Za-z0-9][A-Za-z0-9._-]*$"

public func validateLabel(_ label: String) throws -> String {
    guard let range = label.range(of: labelPattern, options: .regularExpression),
          range == label.startIndex..<label.endIndex
    else {
        throw HelperError.usage(
            "invalid label '\(label)': must match \(labelPattern)")
    }
    return label
}
