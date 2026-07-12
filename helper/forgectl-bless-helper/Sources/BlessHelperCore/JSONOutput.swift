import Foundation

// Shape a flat string->string map into a single-line JSON object for stdout.
// `.sortedKeys` keeps output deterministic; `.withoutEscapingSlashes` leaves
// base64(std) values (which contain `/`) intact rather than emitting `\/`; the
// absence of `.prettyPrinted` keeps it on one line.
public func jsonLine(_ fields: [String: String]) throws -> String {
    let encoder = JSONEncoder()
    encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
    let data = try encoder.encode(fields)
    guard let string = String(data: data, encoding: .utf8) else {
        throw HelperError.with(code: 1, "failed to encode JSON output")
    }
    return string
}
