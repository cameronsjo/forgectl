import XCTest
import Foundation
@testable import BlessHelperCore

// Cover the run() dispatch spine: per-verb routing and the lazy-stdin contract
// (only `sign` consumes stdin). Verbs that reach the Secure Enclave (enroll, and
// pubkey/sign on an existing key) are exercised through the CLI in the live
// verification, not here — these unit tests stay hermetic by routing only to
// paths that never touch the enclave (version; a missing-key pubkey; a sign that
// fails digest decode before the enclave is consulted).
final class RunnerTests: XCTestCase {
    func testVersionRoutesWithoutReadingStdin() throws {
        var reads = 0
        let output = try BlessHelperCore.run(arguments: ["version"], readStdin: {
            reads += 1
            return ""
        })
        XCTAssertEqual(output, #"{"version":"0.0.0-dev"}"#)
        XCTAssertEqual(reads, 0, "version must not read stdin")
    }

    func testUnknownVerbThrowsUsage() {
        assertHelperError(code: 1) {
            _ = try BlessHelperCore.run(arguments: ["frobnicate"], readStdin: { "" })
        }
    }

    func testMissingVerbThrowsUsage() {
        assertHelperError(code: 1) {
            _ = try BlessHelperCore.run(arguments: [], readStdin: { "" })
        }
    }

    func testSignRoutesAndConsumesStdinOnce() {
        var reads = 0
        assertHelperError(code: 5) {
            _ = try BlessHelperCore.run(arguments: ["sign", "--label", "any"], readStdin: {
                reads += 1
                return "not-base64"
            })
        }
        XCTAssertEqual(reads, 1, "sign must read stdin exactly once")
    }

    func testPubkeyRoutesToMissingKeyWithoutReadingStdin() {
        var reads = 0
        let label = "nonexistent-\(UUID().uuidString)"
        assertHelperError(code: 4) {
            _ = try BlessHelperCore.run(arguments: ["pubkey", "--label", label], readStdin: {
                reads += 1
                return ""
            })
        }
        XCTAssertEqual(reads, 0, "pubkey must not read stdin")
    }

    private func assertHelperError(
        code: Int32, _ body: () throws -> Void,
        file: StaticString = #filePath, line: UInt = #line
    ) {
        XCTAssertThrowsError(try body(), file: file, line: line) { error in
            guard let helperError = error as? HelperError else {
                return XCTFail("expected HelperError, got \(error)", file: file, line: line)
            }
            XCTAssertEqual(helperError.code, code, file: file, line: line)
        }
    }
}
