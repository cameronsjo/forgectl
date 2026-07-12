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

    /// Create the key blob EXCLUSIVELY: O_CREAT|O_EXCL means exactly one caller
    /// can win, so two concurrent `enroll` runs can never both mint a key and
    /// have the second silently replace the first's blob. That race is not
    /// cosmetic — the loser still receives a public key back (and may see it
    /// enrolled or anchored), while every later signature comes from the winner's
    /// key, so an enrolled public key would quietly stop matching the key that
    /// actually signs. The loser now gets the existing-label error (exit 3)
    /// instead, exactly as if the key had already existed.
    ///
    /// O_EXCL also refuses to follow a symlink at the target path, so the blob
    /// cannot be redirected. Mode is 0600 (fchmod, not the umask-filtered open
    /// mode); the directory stays 0700.
    static func createBlobExclusively(_ data: Data, at url: URL, label: String) throws {
        let dir = url.deletingLastPathComponent()
        try FileManager.default.createDirectory(
            at: dir,
            withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700])
        // Enforce 0700 even if the directory already existed.
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o700], ofItemAtPath: dir.path)

        let fd = url.path.withCString { open($0, O_CREAT | O_EXCL | O_WRONLY, 0o600) }
        if fd < 0 {
            let code = errno
            if code == EEXIST {
                throw HelperError.with(
                    code: 3,
                    "key '\(label)' already exists at \(url.path); refusing to overwrite")
            }
            throw HelperError.with(
                code: 1,
                "failed to create key blob at \(url.path): \(String(cString: strerror(code)))")
        }
        defer { close(fd) }

        // The open mode is filtered by umask; force exactly 0600 on the fd we own.
        if fchmod(fd, 0o600) != 0 {
            throw HelperError.with(
                code: 1,
                "failed to set permissions on key blob at \(url.path): \(String(cString: strerror(errno)))")
        }

        try data.withUnsafeBytes { (buffer: UnsafeRawBufferPointer) in
            guard let base = buffer.baseAddress, buffer.count > 0 else {
                throw HelperError.with(code: 1, "refusing to persist an empty key blob")
            }
            var written = 0
            while written < buffer.count {
                let n = write(fd, base.advanced(by: written), buffer.count - written)
                if n < 0 {
                    if errno == EINTR { continue }
                    throw HelperError.with(
                        code: 1,
                        "failed to write key blob at \(url.path): \(String(cString: strerror(errno)))")
                }
                written += n
            }
        }
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
    ///
    /// The existence check below is only a fast, legible path to the exit-3
    /// error; the guarantee lives in createBlobExclusively, whose O_EXCL create
    /// is what makes concurrent enrolls safe. A losing racer's freshly minted
    /// enclave key is simply never persisted, so it can never sign anything.
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
        try createBlobExclusively(key.dataRepresentation, at: url, label: label)
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
