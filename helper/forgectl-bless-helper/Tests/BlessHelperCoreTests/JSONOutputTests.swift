import XCTest
@testable import BlessHelperCore

final class JSONOutputTests: XCTestCase {
    func testSingleLineNoWhitespace() throws {
        let line = try jsonLine(["version": "0.0.0-dev"])
        XCTAssertEqual(line, #"{"version":"0.0.0-dev"}"#)
        XCTAssertFalse(line.contains("\n"))
    }

    func testEscapesSpecialCharacters() throws {
        let line = try jsonLine(["k": "a\"b\\c"])
        XCTAssertEqual(line, #"{"k":"a\"b\\c"}"#)
    }

    func testBase64SlashesNotEscaped() throws {
        // Base64 std uses +, /, = — the slash must survive unescaped so the Go
        // side sees the exact base64 string.
        let value = "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEjOux/0yVFv==+"
        let line = try jsonLine(["pubkey": value])
        XCTAssertEqual(line, "{\"pubkey\":\"\(value)\"}")
        XCTAssertFalse(line.contains("\\/"))
    }

    func testDeterministicKeyOrder() throws {
        let line = try jsonLine(["b": "2", "a": "1"])
        XCTAssertEqual(line, #"{"a":"1","b":"2"}"#)
    }
}
