// swift-tools-version:6.2
import PackageDescription

// Forgectl Notch shows forgectl's attention queue at the top of every screen.
// The pure logic (decoding `attention list --json`, deciding what raises,
// the badge, staleness, jump messages, the forgectl path decision) lives in
// `NotchCore`, unit-tested with no UI and no forgectl on the machine. The app
// shell (DynamicNotchKit island, FSEvents, menus) is added on top of it.
let package = Package(
    name: "forgectl-notch",
    platforms: [.macOS(.v14)],
    targets: [
        .target(name: "NotchCore"),
        .testTarget(
            name: "NotchCoreTests",
            dependencies: ["NotchCore"]
        ),
    ]
)
