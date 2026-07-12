import XCTest
import Foundation
import LocalAuthentication
import Security
@testable import BlessHelperCore

// The presence probe's SECURITY property — that a non-presence key signs
// non-interactively and a presence key refuses — can only be exercised against
// real Secure Enclave hardware, which CI does not have. What IS unit-testable
// here, and is the part most likely to drift across OS versions, is the ERROR
// CLASSIFICATION that decides which of those two outcomes an error represents,
// plus the no-hardware error paths (missing key). Those are covered below; the
// live hardware discrimination is verified out of band.
final class EnclavePresenceTests: XCTestCase {

    // A presence-gated key, probed non-interactively, refuses via
    // LocalAuthentication (the observed LAError -1004). That refusal is the GOOD
    // path — the gate is real — so it must classify as a presence signal.
    func testLAErrorClassifiesAsPresenceSignal() {
        let error = LAError(.notInteractive)
        XCTAssertTrue(
            Enclave.isPresenceGateSignal(error),
            "an LAError from the non-interactive probe means the presence gate is active")
    }

    // Depending on OS version the same condition can surface as a Security
    // OSStatus instead; it means the same thing.
    func testErrSecInteractionNotAllowedClassifiesAsPresenceSignal() {
        let error = NSError(
            domain: NSOSStatusErrorDomain, code: Int(errSecInteractionNotAllowed))
        XCTAssertTrue(
            Enclave.isPresenceGateSignal(error),
            "errSecInteractionNotAllowed means the key refused to sign without a human")
    }

    // Anything else at the gate is NOT proof of a presence requirement, so it
    // must fail closed (classify as NOT a presence signal → exit 6).
    func testUnrelatedErrorsDoNotClassifyAsPresenceSignal() {
        XCTAssertFalse(
            Enclave.isPresenceGateSignal(NSError(domain: "com.example.other", code: 1)))
        XCTAssertFalse(
            Enclave.isPresenceGateSignal(
                NSError(domain: NSOSStatusErrorDomain, code: Int(errSecParam))))
    }

    // The no-hardware guard: probing a label with no blob on disk is a
    // key-not-found (exit 4), reached before any Secure Enclave call.
    func testAssertPresenceGatedMissingKeyThrowsExit4() {
        let label = "definitely-absent-\(UUID().uuidString)"
        XCTAssertThrowsError(try Enclave.assertPresenceGated(label: label)) { error in
            guard let helperError = error as? HelperError else {
                return XCTFail("expected HelperError, got \(error)")
            }
            XCTAssertEqual(helperError.code, 4)
        }
    }
}
