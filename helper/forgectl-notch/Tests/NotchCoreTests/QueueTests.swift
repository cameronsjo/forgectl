import Foundation
import Testing
@testable import NotchCore

func json(_ rows: String, sources: String = #"{"spool":"absent","desk":"ok","herdr":"unavailable","core":"ok","sweep":"absent","drain":"absent"}"#) -> Data {
    Data(#"{"rows":[\#(rows)],"skipped":0,"sources":\#(sources),"future":{"x":1}}"#.utf8)
}

func row(_ key: String, tier: String = "raise", kind: String = "desk", text: String = "x", at: String = "2026-11-22T12:00:00Z", terminal: String? = nil) -> String {
    let t = terminal.map { #","terminal":"\#($0)""# } ?? ""
    return #"{"key":"\#(key)","tier":"\#(tier)","kind":"\#(kind)","text":"\#(text)","at":"\#(at)","origin":"own","cost":2,"unknownField":true\#(t)}"#
}

let k1 = "0123456789abcdef"
let k2 = "fedcba9876543210"
let k3 = "00000000000000aa"

@Test func decodesRowsAndIgnoresUnknownFields() throws {
    let s = try decodeQueue(json(row(k1, terminal: "t148")))
    #expect(s.rows.count == 1)
    #expect(s.rows[0].key == k1)
    #expect(s.rows[0].tier == .raise)
    #expect(s.rows[0].terminal == "t148")
    #expect(s.sources.core == "ok")
}

@Test(arguments: ["0123456789ABCDEF", "0123", "0123456789abcdefg", "../../../etc/pass", ""])
func dropsMalformedKeys(_ key: String) throws {
    let s = try decodeQueue(json(row(key)))
    #expect(s.rows.isEmpty)
    #expect(s.dropped == 1)
}

@Test func keepsTheFirstRowOfARepeatedKey() throws {
    let s = try decodeQueue(json([row(k1, text: "first"), row(k1, text: "second")].joined(separator: ",")))
    #expect(s.rows.map(\.text) == ["first"])
    #expect(s.dropped == 1)
}

@Test(arguments: ["t", "x7", "T7", "t7/../sh", "rohan/t7", "t1234567890123456"])
func dropsABadTerminalButKeepsTheRow(_ terminal: String) throws {
    let s = try decodeQueue(json(row(k1, terminal: terminal)))
    #expect(s.rows.count == 1)
    #expect(s.rows[0].terminal == nil)
}

@Test func unknownTierReadsAsHidden() throws {
    let s = try decodeQueue(json(row(k1, tier: "shout")))
    #expect(s.rows[0].tier == .hidden)
}

@Test func cleansAndCutsTextAgain() throws {
    let long = String(repeating: "a", count: 300)
    let s = try decodeQueue(json(row(k1, text: #"bad\u202Ebidi\u0007bell\u200Bzw  \n  ok"#) + "," + row(k2, text: long)))
    #expect(s.rows[0].text == "badbidibellzw ok")
    #expect(s.rows[1].text.count == Caps.text)
    #expect(s.rows[1].text.hasSuffix("…"))
}

@Test func aBodyThatIsNotJSONIsAnError() {
    #expect(throws: DecodeError.notJSON) { try decodeQueue(Data("not json".utf8)) }
    #expect(throws: DecodeError.missingRows) { try decodeQueue(Data(#"{"sources":{}}"#.utf8)) }
}

@Test func rowSourceFollowsKind() throws {
    let s = try decodeQueue(json([row(k1, kind: "desk"), row(k2, kind: "worker"), row(k3, kind: "drain")].joined(separator: ",")))
    #expect(s.rows.map(\.source) == [.desk, .sweep, .drain])
}
