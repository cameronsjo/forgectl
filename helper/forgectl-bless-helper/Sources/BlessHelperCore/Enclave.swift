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

    // MARK: - Presence gate

    /// A fixed dummy digest for the presence probe. Its value is irrelevant — the
    /// probe never uses the signature, only whether signing SUCCEEDS at all.
    static let probeDigest = [UInt8](repeating: 0, count: 32)

    /// Reject any key that is not presence-gated. This closes a silent
    /// root-of-trust bypass: the key-blob directory is user-writable and SE key
    /// CREATION needs no presence, so an agent can mint a key with a non-presence
    /// access control (`[.privateKeyUsage]`, no `.userPresence`), drop its sealed
    /// blob at the compiled-in label, and let the human's own `trust init`
    /// enroll→pubkey-fallback anoint the AGENT's key as the anchor — signed
    /// SILENTLY, with no Touch ID prompt ever appearing.
    ///
    /// The discriminator needs NO human interaction. Load the key under a fresh
    /// LAContext with `interactionNotAllowed = true` and try to sign a dummy
    /// digest:
    ///   - A non-presence key SIGNS with no UI → it is planted → reject (exit 6).
    ///   - A presence key CANNOT sign non-interactively; LocalAuthentication
    ///     refuses with an "interaction not allowed" error — no UI, no prompt —
    ///     which is the tell that the presence gate is real → return normally.
    ///   - Anything else at this gate is untrustworthy → fail closed (exit 6).
    ///
    /// There is deliberately no flag or env var that can skip this probe.
    public static func assertPresenceGated(label: String) throws {
        let url = try keyURL(label: label)
        guard FileManager.default.fileExists(atPath: url.path) else {
            throw HelperError.with(code: 4, "key '\(label)' not found at \(url.path)")
        }
        let blob = try Data(contentsOf: url)

        let context = LAContext()
        context.interactionNotAllowed = true

        do {
            let key = try SecureEnclave.P256.Signing.PrivateKey(
                dataRepresentation: blob, authenticationContext: context)
            _ = try key.signature(for: RawSHA256Digest(bytes: probeDigest))
        } catch {
            // Reaching here means signing did NOT succeed non-interactively. If the
            // failure is the "interaction not allowed" refusal, the presence gate
            // is real and this is the good path. Classify by domain/code, never by
            // message text, so the verdict holds across system locales.
            if isPresenceGateSignal(error) {
                return
            }
            // Any other failure at this gate: we cannot PROVE the key is
            // presence-gated, so we must not trust it.
            throw HelperError.with(
                code: 6,
                "cannot verify key '\(label)' is presence-gated: \(error.localizedDescription)")
        }

        // Signing SUCCEEDED with interaction disallowed → the key has no presence
        // requirement. This is the planted-key tell.
        throw HelperError.with(
            code: 6,
            "key '\(label)' is not presence-gated — it may have been planted; "
                + "remove \(url.path) and re-run (if you did not create a key, this may be tampering)")
    }

    /// Whether an error from the non-interactive probe is the "interaction not
    /// allowed / required" refusal — the signal that a presence gate is active.
    /// LocalAuthentication surfaces it in its own error domain (the observed
    /// LAError -1004); depending on OS version the SecKey signing op can instead
    /// surface it as the Security-framework `errSecInteractionNotAllowed`
    /// OSStatus. Both mean the same thing: the key refused to sign without a
    /// human, i.e. it is genuinely presence-gated.
    static func isPresenceGateSignal(_ error: Error) -> Bool {
        let nsError = error as NSError
        if nsError.domain == LAError.errorDomain {
            return true
        }
        if nsError.domain == NSOSStatusErrorDomain
            && nsError.code == Int(errSecInteractionNotAllowed) {
            return true
        }
        return false
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
    ///
    /// After persisting, self-check that the key we just created is presence-gated
    /// (defense in depth — proves the access control took, and that nothing
    /// swapped the blob between the write and the check).
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
        try assertPresenceGated(label: label)
        return key.publicKey.derRepresentation.base64EncodedString()
    }

    /// Load the sealed blob and return its public key as base64(std) of PKIX DER.
    /// Deriving the public key needs no authentication context and no presence —
    /// but this is the path `trust init`'s enroll→fallback and blessing both use
    /// to learn a key's identity, so it MUST first prove the key is presence-gated
    /// (see assertPresenceGated). Returning the pubkey of a planted non-presence
    /// key is exactly how that key becomes the anchor.
    public static func pubkey(label: String) throws -> String {
        let url = try keyURL(label: label)
        guard FileManager.default.fileExists(atPath: url.path) else {
            throw HelperError.with(code: 4, "key '\(label)' not found at \(url.path)")
        }
        try assertPresenceGated(label: label)
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
    ///
    /// The key's presence gate is probed FIRST (no UI for a real key — it fails
    /// the non-interactive probe fast), so a planted non-presence key is rejected
    /// here too, not only at bootstrap. Only then does the real, interactive
    /// signature run.
    public static func sign(label: String, digest: [UInt8]) throws -> String {
        guard SecureEnclave.isAvailable else {
            throw HelperError.with(code: 1, "Secure Enclave is not available on this machine")
        }
        let url = try keyURL(label: label)
        guard FileManager.default.fileExists(atPath: url.path) else {
            throw HelperError.with(code: 4, "key '\(label)' not found at \(url.path)")
        }
        try assertPresenceGated(label: label)
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
