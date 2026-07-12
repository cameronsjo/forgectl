import Foundation
import CryptoKit
import Security
import LocalAuthentication

// All Secure-Enclave-touching operations. The private key never leaves the
// enclave; we persist only its opaque `dataRepresentation` sealed blob, which
// is usable solely by this machine's enclave. The access control is baked into
// the blob at creation and enforced by the SE/coreauthd — there is deliberately
// no flag, env var, or build setting that weakens or skips the userPresence
// requirement.
public enum Enclave {

    // MARK: - Storage

    static func keyDirectory() throws -> URL {
        let base = try FileManager.default.url(
            for: .applicationSupportDirectory,
            in: .userDomainMask,
            appropriateFor: nil,
            create: false)
        return base
            .appendingPathComponent("forgectl", isDirectory: true)
            .appendingPathComponent("bless-keys", isDirectory: true)
    }

    static func keyURL(label: String) throws -> URL {
        try keyDirectory().appendingPathComponent("\(label).key", isDirectory: false)
    }

    static func writeBlob(_ data: Data, to url: URL) throws {
        let dir = url.deletingLastPathComponent()
        try FileManager.default.createDirectory(
            at: dir,
            withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700])
        // Enforce 0700 even if the directory already existed.
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o700], ofItemAtPath: dir.path)
        try data.write(to: url, options: [.atomic])
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o600], ofItemAtPath: url.path)
    }

    // MARK: - Access control

    static func makeAccessControl() throws -> SecAccessControl {
        var error: Unmanaged<CFError>?
        guard let control = SecAccessControlCreateWithFlags(
            kCFAllocatorDefault,
            kSecAttrAccessibleWhenUnlockedThisDeviceOnly,
            [.privateKeyUsage, .userPresence],
            &error)
        else {
            let detail = error?.takeRetainedValue()
            throw HelperError.with(
                code: 1,
                "failed to create access control: \(detail.map(String.init(describing:)) ?? "unknown")")
        }
        return control
    }

    // MARK: - Verbs

    /// Create a fresh SE key, persist its sealed blob, and return the public key
    /// as base64(std) of PKIX DER. Creation requires no user presence; presence
    /// is only enforced at signing time. Refuses to overwrite an existing blob.
    public static func enroll(label: String) throws -> String {
        guard SecureEnclave.isAvailable else {
            throw HelperError.with(code: 1, "Secure Enclave is not available on this machine")
        }
        let url = try keyURL(label: label)
        if FileManager.default.fileExists(atPath: url.path) {
            throw HelperError.with(
                code: 3,
                "key '\(label)' already exists at \(url.path); refusing to overwrite")
        }
        let control = try makeAccessControl()
        let key: SecureEnclave.P256.Signing.PrivateKey
        do {
            key = try SecureEnclave.P256.Signing.PrivateKey(accessControl: control)
        } catch {
            throw HelperError.with(
                code: 1, "failed to create Secure Enclave key: \(error.localizedDescription)")
        }
        try writeBlob(key.dataRepresentation, to: url)
        return key.publicKey.derRepresentation.base64EncodedString()
    }

    /// Load the sealed blob and return its public key as base64(std) of PKIX DER.
    /// Deriving the public key needs no authentication context and no presence.
    public static func pubkey(label: String) throws -> String {
        let url = try keyURL(label: label)
        guard FileManager.default.fileExists(atPath: url.path) else {
            throw HelperError.with(code: 4, "key '\(label)' not found at \(url.path)")
        }
        let blob = try Data(contentsOf: url)
        let key: SecureEnclave.P256.Signing.PrivateKey
        do {
            key = try SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: blob)
        } catch {
            throw HelperError.with(
                code: 1, "failed to load key '\(label)': \(error.localizedDescription)")
        }
        return key.publicKey.derRepresentation.base64EncodedString()
    }

    /// Sign a raw 32-byte digest with the SE key. Every signature demands human
    /// presence (Touch ID / account password); a cancel or auth failure is
    /// reported as exit 2. Returns the ASN.1 DER signature as base64(std).
    public static func sign(label: String, digest: [UInt8]) throws -> String {
        guard SecureEnclave.isAvailable else {
            throw HelperError.with(code: 1, "Secure Enclave is not available on this machine")
        }
        let url = try keyURL(label: label)
        guard FileManager.default.fileExists(atPath: url.path) else {
            throw HelperError.with(code: 4, "key '\(label)' not found at \(url.path)")
        }
        let blob = try Data(contentsOf: url)

        let context = LAContext()
        context.localizedReason = "forgectl: sign blessing with key '\(label)'"

        let key: SecureEnclave.P256.Signing.PrivateKey
        do {
            key = try SecureEnclave.P256.Signing.PrivateKey(
                dataRepresentation: blob, authenticationContext: context)
        } catch {
            throw mapSignError(error, label: label)
        }

        do {
            let signature = try key.signature(for: RawSHA256Digest(bytes: digest))
            return signature.derRepresentation.base64EncodedString()
        } catch {
            throw mapSignError(error, label: label)
        }
    }

    // Classify a failure from the presence-gated signing path WITHOUT matching
    // localized message text, so the exit-code contract holds across system
    // locales. Only unambiguous programming faults in the crypto call are
    // internal (exit 1); every other failure at this human-presence gate is
    // treated conservatively as an auth refusal (exit 2). A false "auth refused"
    // is harmless, whereas a false "internal error" would let a human declining
    // Touch ID masquerade as a bug — muddying the frozen exit-2 contract.
    static func mapSignError(_ error: Error, label: String) -> HelperError {
        // Structural/programming faults in the crypto operation are internal.
        // The digest is always exactly 32 bytes here, so these should never fire
        // in practice — but if they do, they are bugs, not human refusals.
        if let cryptoError = error as? CryptoKitError {
            switch cryptoError {
            case .incorrectKeySize, .incorrectParameterSize, .invalidParameter:
                return HelperError.with(
                    code: 1,
                    "signing failed for key '\(label)' (internal): \(error.localizedDescription)")
            default:
                break  // .authenticationFailure / .underlyingCoreCryptoError / wrap → refusal
            }
        }
        // LocalAuthentication surfaces every human-side outcome (cancel, auth
        // failure, biometry lockout, passcode-not-set, not-interactive) under its
        // own error domain — classify by domain, never by message text.
        let nsError = error as NSError
        if nsError.domain == LAError.errorDomain {
            return HelperError.with(
                code: 2,
                "authentication refused for key '\(label)': \(nsError.localizedDescription)")
        }
        // Conservative default: anything else at this presence gate (an
        // unrecognized Security-framework OSStatus, a wrapped enclave error) is
        // treated as a refusal so a locale-specific description can never demote
        // a human refusal to exit 1.
        return HelperError.with(
            code: 2,
            "authentication refused or not completed for key '\(label)': \(nsError.localizedDescription)")
    }
}
