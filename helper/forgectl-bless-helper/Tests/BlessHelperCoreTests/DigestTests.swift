import XCTest
import Foundation
@testable import BlessHelperCore

final class DigestTests: XCTestCase {
    private func base64Of(byteCount: Int) -> String {
        Data(repeating: 0xAB, count: byteCount).base64EncodedString()
    }

    func testValid32ByteDigest() throws {
        let bytes = try decodeDigest(base64Of(byteCount: 32))
        XCTAssertEqual(bytes.count, 32)
        XCTAssertTrue(bytes.allSatisfy { $0 == 0xAB })
    }

    func testTrimsSurroundingWhitespaceAndNewline() throws {
        let padded = "  \n" + base64Of(byteCount: 32) + "\n\t "
        XCTAssertEqual(try decodeDigest(padded).count, 32)
    }

    func testRejectsNonBase64() {
        assertBadDigest("not-base64!!!")
    }

    func testRejectsShortDigest() {
        assertBadDigest(base64Of(byteCount: 16))
    }

    func testRejectsLongDigest() {
        assertBadDigest(base64Of(byteCount: 64))
    }

    func testRejectsEmpty() {
        assertBadDigest("")
    }

    private func assertBadDigest(
        _ input: String, file: StaticString = #filePath, line: UInt = #line
    ) {
        XCTAssertThrowsError(try decodeDigest(input), file: file, line: line) { error in
            guard let helperError = error as? HelperError else {
                return XCTFail("expected HelperError, got \(error)", file: file, line: line)
            }
            XCTAssertEqual(helperError.code, 5, file: file, line: line)
        }
    }
}
