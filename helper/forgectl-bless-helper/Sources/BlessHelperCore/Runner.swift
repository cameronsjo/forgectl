// The dispatch spine: parse argv, run the verb, return the single JSON line to
// print on stdout (or nil for verbs that print nothing). `readStdin` is lazy —
// only `sign` consumes it, so `version`/`enroll`/`pubkey` never block on input.
public func run(arguments: [String], readStdin: () -> String) throws -> String? {
    switch try parseCommand(arguments) {
    case .version:
        return try jsonLine(["version": helperVersion])
    case .enroll(let label):
        return try jsonLine(["pubkey": try Enclave.enroll(label: label)])
    case .pubkey(let label):
        return try jsonLine(["pubkey": try Enclave.pubkey(label: label)])
    case .sign(let label):
        let digest = try decodeDigest(readStdin())
        return try jsonLine(["signature": try Enclave.sign(label: label, digest: digest)])
    }
}
