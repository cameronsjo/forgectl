import Foundation
import CryptoKit

// A pass-through Digest: CryptoKit's `signature(for: Data)` would re-hash the
// bytes, but the generic `signature(for: D) where D: Digest` signs the digest
// bytes directly. This lets the helper sign a 32-byte digest handed to it by
// the Go side without ever seeing the plaintext.
struct RawSHA256Digest: Digest {
    static var byteCount: Int { 32 }
    let bytes: [UInt8]

    func withUnsafeBytes<R>(_ body: (UnsafeRawBufferPointer) throws -> R) rethrows -> R {
        try bytes.withUnsafeBytes(body)
    }

    func makeIterator() -> Array<UInt8>.Iterator { bytes.makeIterator() }
    func hash(into hasher: inout Hasher) { hasher.combine(bytes) }

    static func == (lhs: RawSHA256Digest, rhs: RawSHA256Digest) -> Bool {
        lhs.bytes == rhs.bytes
    }

    var description: String { "RawSHA256Digest" }
}

// Decode base64(std) stdin into exactly 32 raw digest bytes. Outer whitespace
// and newlines are trimmed; anything that is not valid base64 or is not exactly
// 32 bytes is a bad-digest error (exit 5).
public func decodeDigest(_ raw: String) throws -> [UInt8] {
    let trimmed = raw.trimmingCharacters(in: .whitespacesAndNewlines)
    guard let data = Data(base64Encoded: trimmed) else {
        throw HelperError.with(code: 5, "stdin is not valid base64")
    }
    guard data.count == 32 else {
        throw HelperError.with(
            code: 5, "digest must be exactly 32 bytes, got \(data.count)")
    }
    return [UInt8](data)
}
