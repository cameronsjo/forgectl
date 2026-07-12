import Foundation
import BlessHelperCore

// Thin entrypoint: JSON on stdout, diagnostics on stderr, exit code from the
// frozen contract (0 ok · 1 usage/internal · 2 auth refused · 3 label exists ·
// 4 key not found · 5 bad digest).

func writeStderr(_ message: String) {
    FileHandle.standardError.write(Data((message + "\n").utf8))
}

func readAllStdin() -> String {
    let data = FileHandle.standardInput.readDataToEndOfFile()
    return String(data: data, encoding: .utf8) ?? ""
}

do {
    let arguments = Array(CommandLine.arguments.dropFirst())
    if let output = try run(arguments: arguments, readStdin: readAllStdin) {
        print(output)
    }
    exit(0)
} catch let error as HelperError {
    writeStderr(error.message)
    exit(error.code)
} catch {
    writeStderr("internal error: \(error.localizedDescription)")
    exit(1)
}
