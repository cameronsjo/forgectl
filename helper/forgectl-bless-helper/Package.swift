// swift-tools-version:5.9
import PackageDescription

// The single executable is `forgectl-bless-helper`. The pure logic (argument
// parsing, label validation, digest decoding, JSON shaping) lives in the
// `BlessHelperCore` library so it can be unit-tested on machines with no
// Secure Enclave (CI runners). The Secure-Enclave-touching code compiles
// against system frameworks on any macOS but only functions on real hardware.
let package = Package(
    name: "forgectl-bless-helper",
    platforms: [.macOS(.v13)],
    targets: [
        .target(name: "BlessHelperCore"),
        .executableTarget(
            name: "forgectl-bless-helper",
            dependencies: ["BlessHelperCore"]
        ),
        .testTarget(
            name: "BlessHelperCoreTests",
            dependencies: ["BlessHelperCore"]
        ),
    ]
)
