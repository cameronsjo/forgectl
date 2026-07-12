import XCTest
@testable import BlessHelperCore

final class ArgumentsTests: XCTestCase {
    func testVersion() throws {
        XCTAssertEqual(try parseCommand(["version"]), .version)
    }

    func testVersionRejectsExtraArgs() {
        assertUsage { try parseCommand(["version", "--label", "x"]) }
    }

    func testEnrollWithLabel() throws {
        XCTAssertEqual(try parseCommand(["enroll", "--label", "agent"]), .enroll(label: "agent"))
    }

    func testPubkeyWithLabel() throws {
        XCTAssertEqual(try parseCommand(["pubkey", "--label", "agent"]), .pubkey(label: "agent"))
    }

    func testSignWithLabel() throws {
        XCTAssertEqual(try parseCommand(["sign", "--label", "agent"]), .sign(label: "agent"))
    }

    func testLabelEqualsForm() throws {
        XCTAssertEqual(try parseCommand(["enroll", "--label=agent"]), .enroll(label: "agent"))
    }

    func testMissingVerb() {
        assertUsage { try parseCommand([]) }
    }

    func testUnknownVerb() {
        assertUsage { try parseCommand(["frobnicate", "--label", "x"]) }
    }

    func testMissingLabel() {
        assertUsage { try parseCommand(["enroll"]) }
    }

    func testLabelFlagWithoutValue() {
        assertUsage { try parseCommand(["enroll", "--label"]) }
    }

    func testDuplicateLabel() {
        assertUsage { try parseCommand(["enroll", "--label", "a", "--label", "b"]) }
    }

    func testUnexpectedArgument() {
        assertUsage { try parseCommand(["enroll", "--label", "a", "extra"]) }
    }

    func testInvalidLabelRejectedDuringParse() {
        assertUsage { try parseCommand(["enroll", "--label", "../evil"]) }
    }

    private func assertUsage(
        _ body: () throws -> Command, file: StaticString = #filePath, line: UInt = #line
    ) {
        XCTAssertThrowsError(try body(), file: file, line: line) { error in
            guard let helperError = error as? HelperError else {
                return XCTFail("expected HelperError, got \(error)", file: file, line: line)
            }
            XCTAssertEqual(helperError.code, 1, file: file, line: line)
        }
    }
}
