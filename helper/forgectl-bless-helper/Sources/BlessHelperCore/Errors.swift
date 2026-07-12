// A helper error carries the process exit code alongside a human-readable
// message. The exit-code contract is frozen and shared with the Go side:
//   0 ok · 1 usage/internal · 2 user cancelled/auth refused ·
//   3 label exists · 4 key not found · 5 bad digest
public struct HelperError: Error, Equatable {
    public let code: Int32
    public let message: String

    public init(code: Int32, message: String) {
        self.code = code
        self.message = message
    }

    public static func usage(_ message: String) -> HelperError {
        HelperError(code: 1, message: message)
    }

    public static func with(code: Int32, _ message: String) -> HelperError {
        HelperError(code: code, message: message)
    }
}
