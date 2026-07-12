// Hand-rolled argv parsing — no third-party dependency. The verb set is small
// and frozen; every label-bearing verb requires exactly one `--label <value>`
// (also accepts `--label=<value>`), and the label is validated up front.
public enum Command: Equatable {
    case enroll(label: String)
    case pubkey(label: String)
    case sign(label: String)
    case version
}

public func parseCommand(_ args: [String]) throws -> Command {
    guard let verb = args.first else {
        throw HelperError.usage(
            "missing verb; expected one of: enroll, pubkey, sign, version")
    }
    let rest = Array(args.dropFirst())

    switch verb {
    case "version":
        guard rest.isEmpty else {
            throw HelperError.usage("version takes no arguments")
        }
        return .version
    case "enroll":
        return .enroll(label: try labelArgument(rest, verb: verb))
    case "pubkey":
        return .pubkey(label: try labelArgument(rest, verb: verb))
    case "sign":
        return .sign(label: try labelArgument(rest, verb: verb))
    default:
        throw HelperError.usage(
            "unknown verb '\(verb)'; expected one of: enroll, pubkey, sign, version")
    }
}

private func labelArgument(_ args: [String], verb: String) throws -> String {
    var label: String?
    var index = 0
    while index < args.count {
        let token = args[index]
        if token == "--label" {
            index += 1
            guard index < args.count else {
                throw HelperError.usage("--label requires a value")
            }
            guard label == nil else {
                throw HelperError.usage("--label given more than once")
            }
            label = args[index]
        } else if token.hasPrefix("--label=") {
            guard label == nil else {
                throw HelperError.usage("--label given more than once")
            }
            label = String(token.dropFirst("--label=".count))
        } else {
            throw HelperError.usage("\(verb): unexpected argument '\(token)'")
        }
        index += 1
    }

    guard let value = label else {
        throw HelperError.usage("\(verb) requires --label <label>")
    }
    return try validateLabel(value)
}
