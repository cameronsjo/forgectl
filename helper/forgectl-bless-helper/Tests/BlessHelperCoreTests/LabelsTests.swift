import XCTest
@testable import BlessHelperCore

final class LabelsTests: XCTestCase {
    func testValidLabels() throws {
        for label in ["a", "A", "0", "agent", "spike-agent-test", "my.key_1-2", "Z9"] {
            XCTAssertEqual(try validateLabel(label), label)
        }
    }

    func testRejectsLeadingDot() {
        assertRejected(".hidden")
    }

    func testRejectsLeadingDash() {
        assertRejected("-flag")
    }

    func testRejectsPathSeparators() {
        assertRejected("../evil")
        assertRejected("a/b")
        assertRejected("a\\b")
    }

    func testRejectsEmpty() {
        assertRejected("")
    }

    func testRejectsSpaces() {
        assertRejected("a b")
    }

    func testRejectsTrailingNewline() {
        assertRejected("agent\n")
    }

    func testRejectsEmbeddedNewline() {
        assertRejected("agent\nevil")
    }

    private func assertRejected(
        _ label: String, file: StaticString = #filePath, line: UInt = #line
    ) {
        XCTAssertThrowsError(try validateLabel(label), file: file, line: line) { error in
            guard let helperError = error as? HelperError else {
                return XCTFail("expected HelperError, got \(error)", file: file, line: line)
            }
            XCTAssertEqual(helperError.code, 1, file: file, line: line)
        }
    }
}
