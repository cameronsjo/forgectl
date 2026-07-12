import XCTest
import Foundation
@testable import BlessHelperCore

// Exercises the key-blob persistence path only — no Secure Enclave, so these
// run anywhere (including CI, where no SE key can be minted).
//
// The invariant under test is what makes concurrent `enroll` safe: blob creation
// is EXCLUSIVE. Without it, two enrolls both mint a key, both pass the existence
// check, and the second atomically replaces the first's blob — leaving the loser
// holding a public key that was enrolled (or even anchored) while every later
// signature comes from the winner's key.
final class EnclaveBlobTests: XCTestCase {
    private func tempKeyURL() throws -> URL {
        let dir = URL(fileURLWithPath: NSTemporaryDirectory(), isDirectory: true)
            .appendingPathComponent("forgectl-blob-tests-\(UUID().uuidString)", isDirectory: true)
        addTeardownBlock { try? FileManager.default.removeItem(at: dir) }
        return dir.appendingPathComponent("bless.key", isDirectory: false)
    }

    func testCreatesBlobWith0600InA0700Directory() throws {
        let url = try tempKeyURL()
        let blob = Data([0x01, 0x02, 0x03, 0x04])

        try Enclave.createBlobExclusively(blob, at: url, label: "l")

        XCTAssertEqual(try Data(contentsOf: url), blob)

        let fileMode = try FileManager.default.attributesOfItem(atPath: url.path)[.posixPermissions] as? NSNumber
        XCTAssertEqual(fileMode?.int16Value, 0o600)

        let dirPath = url.deletingLastPathComponent().path
        let dirMode = try FileManager.default.attributesOfItem(atPath: dirPath)[.posixPermissions] as? NSNumber
        XCTAssertEqual(dirMode?.int16Value, 0o700)
    }

    // The race pin: a second creation over the same path must lose with exit 3,
    // never silently replace the first blob.
    func testSecondCreationLosesWithLabelExists() throws {
        let url = try tempKeyURL()
        let first = Data([0xAA, 0xBB])
        try Enclave.createBlobExclusively(first, at: url, label: "mylabel")

        XCTAssertThrowsError(
            try Enclave.createBlobExclusively(Data([0xCC, 0xDD]), at: url, label: "mylabel")
        ) { error in
            guard let helperError = error as? HelperError else {
                return XCTFail("expected a HelperError, got \(error)")
            }
            XCTAssertEqual(helperError.code, 3, "a losing enroll must report label-exists (exit 3)")
        }

        // The winner's blob is untouched — this is the whole point.
        XCTAssertEqual(try Data(contentsOf: url), first)
    }

    // O_EXCL also refuses to follow a symlink at the target, so the blob cannot
    // be redirected into an attacker-chosen file.
    func testRefusesToWriteThroughASymlink() throws {
        let url = try tempKeyURL()
        let dir = url.deletingLastPathComponent()
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)

        let decoy = dir.appendingPathComponent("decoy", isDirectory: false)
        try Data([0x00]).write(to: decoy)
        try FileManager.default.createSymbolicLink(at: url, withDestinationURL: decoy)

        XCTAssertThrowsError(try Enclave.createBlobExclusively(Data([0xEE]), at: url, label: "l"))
        XCTAssertEqual(try Data(contentsOf: decoy), Data([0x00]), "the symlink target must not be written")
    }
}
