#!/usr/bin/env python3
"""Verify the visible Projects surface of an existing named Herdr session.

The probe never starts a server or mutates a workspace.  It first performs a
read-only ``workspace list`` through the named session, then attaches Herdr's
native client to a 160x45 forkpty and reconstructs the final VT screen.  The
assertions are made against that reconstructed screen, rather than against
API labels or escape-sequence payloads.
"""

from __future__ import annotations

import argparse
import codecs
import contextlib
import errno
import fcntl
import json
import os
import pathlib
import re
import select
import signal
import struct
import subprocess
import sys
import termios
import time
import unicodedata

ROWS = 45
COLS = 160
PROBE_TIMEOUT = 24.0
SETTLE_TIME = 1.0
PREFLIGHT_TIMEOUT = 12.0
MAX_CAPTURE = 16 * 1024 * 1024
SENSITIVE_ENVIRONMENT = {
    "OPENAI_API_KEY", "OPENAI_API_BASE", "ANTHROPIC_API_KEY", "GEMINI_API_KEY",
    "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "GH_TOKEN",
    "GITHUB_TOKEN", "SSH_AUTH_SOCK", "FORGECTL_LEDGER", "FORGECTL_INBOX",
    "FORGECTL_RUN_ID", "CODEX_HOME", "PI_CODING_AGENT_DIR",
}

BACKGROUND_REPLY = b"\x1b]11;rgb:1c1c/1c1c/1c1c\x1b\\"
FOREGROUND_REPLY = b"\x1b]10;rgb:d0d0/d0d0/d0d0\x1b\\"
PALETTE_REPLY = b"rgb:1c1c/1c1c/1c1c"

STATIC_QUERIES = (
    (b"\x1b]11;?", BACKGROUND_REPLY),
    (b"\x1b]10;?", FOREGROUND_REPLY),
    (b"\x1b[6n", b"\x1b[1;1R"),
    (b"\x1b[5n", b"\x1b[0n"),
    (b"\x1b[c", b"\x1b[?1;2c"),
    (b"\x1b[>c", b"\x1b[>0;276;0c"),
)

OSC4_QUERY = re.compile(rb"\x1b\]4;([^;\x07\x1b\\]+);\?(?:\x07|\x1b\\)")


class ProbeError(Exception):
    pass


def _ensure_absolute(value: str, name: str) -> pathlib.Path:
    path = pathlib.Path(value)
    if not path.is_absolute():
        raise ProbeError(f"--{name} must be an absolute path")
    return path


def _ensure_executable(path: pathlib.Path) -> pathlib.Path:
    try:
        path.stat()
    except OSError as error:
        raise ProbeError(f"--herdr is not executable: {path}: {error}") from error
    if not path.is_file() or not os.access(path, os.X_OK):
        raise ProbeError(f"--herdr is not a regular executable: {path}")
    return path.resolve()


def _validate_session(session: str) -> str:
    if not session or len(session) > 64 or session in {"default", "fleet"}:
        raise ProbeError("--session must name a non-global Herdr session")
    if re.fullmatch(r"[A-Za-z0-9._-]+", session) is None:
        raise ProbeError("--session contains unsupported characters")
    return session


def _private_directory(path: pathlib.Path, create: bool) -> pathlib.Path:
    if path.exists() and path.is_symlink():
        raise ProbeError(f"refusing symlinked output path: {path}")
    if create:
        path.mkdir(mode=0o700, parents=True, exist_ok=True)
    if not path.is_dir():
        raise ProbeError(f"output parent is not a directory: {path}")
    mode = path.stat().st_mode
    if mode & 0o077:
        raise ProbeError(f"output directory must be private (0700 or stricter): {path}")
    return path


def artifact_paths(output: str) -> dict[str, pathlib.Path]:
    requested = _ensure_absolute(output, "output")
    if requested.exists() and requested.is_symlink():
        raise ProbeError(f"refusing symlinked output path: {requested}")
    if requested.exists() and requested.is_dir():
        directory = _private_directory(requested, False)
        stem = directory / "render"
        return {"json": stem.with_suffix(".json"), "raw": stem.with_suffix(".raw"), "text": stem.with_suffix(".txt")}
    if requested.suffix.lower() == ".json":
        _private_directory(requested.parent, False)
        stem = requested.with_suffix("")
        return {"json": requested, "raw": stem.with_suffix(".raw"), "text": stem.with_suffix(".txt")}
    directory = _private_directory(requested, True)
    stem = directory / "render"
    return {"json": stem.with_suffix(".json"), "raw": stem.with_suffix(".raw"), "text": stem.with_suffix(".txt")}


def _write_private(path: pathlib.Path, data: bytes) -> None:
    if path.exists() and path.is_symlink():
        raise ProbeError(f"refusing symlinked receipt: {path}")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(path, flags, 0o600)
    except OSError as error:
        raise ProbeError(f"could not open private receipt {path}: {error}") from error
    try:
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(data)
    except Exception:
        with contextlib.suppress(OSError):
            os.close(descriptor)
        raise
    os.chmod(path, 0o600)


def _json_from_stdout(stdout: bytes, label: str) -> dict:
    text = stdout.decode("utf-8", "replace").strip()
    if not text:
        raise ProbeError(f"{label} returned no JSON")
    try:
        document = json.loads(text)
    except json.JSONDecodeError as error:
        raise ProbeError(f"{label} returned invalid JSON: {text[-600:]}") from error
    if not isinstance(document, dict) or not isinstance(document.get("result"), dict):
        raise ProbeError(f"{label} did not return a result object")
    return document


def _probe_environment() -> dict[str, str]:
    environment = os.environ.copy()
    for key in SENSITIVE_ENVIRONMENT:
        environment.pop(key, None)
    environment.pop("HERDR_SESSION", None)
    environment.pop("HERDR_SOCKET_PATH", None)
    environment.pop("HERDR_CLIENT_SOCKET_PATH", None)
    return environment


def preflight(herdr: pathlib.Path, session: str) -> dict:
    command = [str(herdr), "--session", session, "workspace", "list"]
    environment = _probe_environment()
    try:
        result = subprocess.run(command, env=environment, capture_output=True, timeout=PREFLIGHT_TIMEOUT, check=False)
    except (OSError, subprocess.TimeoutExpired) as error:
        raise ProbeError(f"named-session workspace list failed: {error}") from error
    if result.returncode != 0:
        stderr = result.stderr.decode("utf-8", "replace").strip()
        raise ProbeError(f"named-session workspace list exited {result.returncode}: {stderr[-800:]}")
    document = _json_from_stdout(result.stdout, "named-session workspace list")
    workspaces = document["result"].get("workspaces")
    if not isinstance(workspaces, list):
        raise ProbeError("named-session workspace list omitted workspaces")
    return {"command": command, "workspace_count": len(workspaces)}


class QueryResponder:
    def __init__(self, fd: int):
        self.fd = fd
        self.tail = b""
        self.replies = 0

    def _write(self, reply: bytes) -> None:
        try:
            os.write(self.fd, reply)
            self.replies += 1
        except OSError as error:
            if error.errno not in {errno.EBADF, errno.EIO, errno.EPIPE}:
                raise

    def feed(self, chunk: bytes) -> None:
        old_tail_length = len(self.tail)
        scan = self.tail + chunk
        for query, reply in STATIC_QUERIES:
            offset = 0
            while True:
                start = scan.find(query, offset)
                if start < 0:
                    break
                if start + len(query) > old_tail_length:
                    self._write(reply)
                offset = start + 1
        for match in OSC4_QUERY.finditer(scan):
            if match.end() > old_tail_length:
                index = match.group(1)
                self._write(b"\x1b]4;" + index + b";" + PALETTE_REPLY + b"\x1b\\")
        keep = max(max(len(query) for query, _ in STATIC_QUERIES), 32)
        self.tail = scan[-keep:]


def answer_queries(fd: int, chunk: bytes) -> None:
    """Answer terminal capability queries found in one output chunk."""
    QueryResponder(fd).feed(chunk)


class VTScreen:
    def __init__(self, rows: int = ROWS, cols: int = COLS):
        self.rows = rows
        self.cols = cols
        self.cells = [[" "] * cols for _ in range(rows)]
        self.row = 0
        self.col = 0
        self.top = 0
        self.bottom = rows - 1
        self.saved = (0, 0)
        self.primary = None
        self.utf8 = codecs.getincrementaldecoder("utf-8")("replace")
        self.state = "normal"
        self.sequence = bytearray()
        self.osc = bytearray()
        self.unsupported = []

    def _clear(self) -> None:
        self.cells = [[" "] * self.cols for _ in range(self.rows)]
        self.row = 0
        self.col = 0

    def _scroll_up(self, count: int = 1) -> None:
        for _ in range(max(1, count)):
            self.cells[self.top:self.bottom + 1] = self.cells[self.top + 1:self.bottom + 1] + [[" "] * self.cols]

    def _scroll_down(self, count: int = 1) -> None:
        for _ in range(max(1, count)):
            self.cells[self.top:self.bottom + 1] = [[" "] * self.cols] + self.cells[self.top:self.bottom]

    def _line_feed(self) -> None:
        if self.row == self.bottom:
            self._scroll_up()
        else:
            self.row = min(self.rows - 1, self.row + 1)

    def _put(self, character: str) -> None:
        if not character:
            return
        if self.col >= self.cols:
            self.col = 0
            self._line_feed()
        width = 0 if unicodedata.combining(character) else (2 if unicodedata.east_asian_width(character) in "WF" else 1)
        if width == 0:
            if self.col and self.cells[self.row][self.col - 1] != " ":
                self.cells[self.row][self.col - 1] += character
            return
        self.cells[self.row][self.col] = character
        if width == 2 and self.col + 1 < self.cols:
            self.cells[self.row][self.col + 1] = " "
        self.col += width

    def _erase_line(self, start: int, end: int) -> None:
        for index in range(max(0, start), min(self.cols, end)):
            self.cells[self.row][index] = " "

    def _erase_display(self, mode: int) -> None:
        if mode == 2 or mode == 3:
            self.cells = [[" "] * self.cols for _ in range(self.rows)]
        elif mode == 1:
            for index in range(self.row):
                self.cells[index] = [" "] * self.cols
            self._erase_line(0, self.col + 1)
        else:
            self._erase_line(self.col, self.cols)
            for index in range(self.row + 1, self.rows):
                self.cells[index] = [" "] * self.cols

    @staticmethod
    def _params(parameter: str) -> tuple[str, list[int]]:
        private = parameter[:1] if parameter[:1] in "?>!" else ""
        body = parameter[1:] if private else parameter
        values = []
        for value in body.split(";") if body else []:
            try:
                values.append(int(value) if value else 0)
            except ValueError:
                values.append(0)
        return private, values

    @staticmethod
    def _value(values: list[int], index: int, default: int = 1) -> int:
        value = values[index] if index < len(values) else default
        return default if value == 0 else value

    def _csi(self, sequence: bytes) -> None:
        final = chr(sequence[-1])
        body = sequence[:-1].decode("ascii", "replace")
        parameter = body.split(" ", 1)[0]
        private, values = self._params(parameter)
        if final in "ABCD":
            amount = self._value(values, 0)
            if final == "A": self.row = max(self.top, self.row - amount)
            if final == "B": self.row = min(self.bottom, self.row + amount)
            if final == "C": self.col = min(self.cols, self.col + amount)
            if final == "D": self.col = max(0, self.col - amount)
        elif final in "EF":
            amount = self._value(values, 0)
            self.row = min(self.bottom, self.row + amount) if final == "E" else max(self.top, self.row - amount)
            self.col = 0
        elif final in "G`":
            self.col = max(0, min(self.cols - 1, self._value(values, 0) - 1))
        elif final == "d":
            self.row = max(self.top, min(self.bottom, self._value(values, 0) - 1))
        elif final in "Hf":
            self.row = max(self.top, min(self.bottom, self._value(values, 0) - 1))
            self.col = max(0, min(self.cols - 1, self._value(values, 1) - 1))
        elif final == "J":
            self._erase_display(values[0] if values else 0)
        elif final == "K":
            mode = values[0] if values else 0
            if mode == 1: self._erase_line(0, self.col + 1)
            elif mode == 2: self._erase_line(0, self.cols)
            else: self._erase_line(self.col, self.cols)
        elif final == "m":
            return
        elif final == "r":
            top = (values[0] if values else 1) - 1
            bottom = (values[1] if len(values) > 1 and values[1] else self.rows) - 1
            if 0 <= top < bottom < self.rows:
                self.top, self.bottom = top, bottom
                self.row, self.col = top, 0
        elif final in "su":
            if final == "s": self.saved = (self.row, self.col)
            else: self.row, self.col = self.saved
        elif final in "hl" and private == "?":
            modes = values or [0]
            if 1049 in modes or 47 in modes or 1047 in modes:
                if final == "h" and self.primary is None:
                    self.primary = (self.cells, self.row, self.col, self.top, self.bottom)
                    self._clear()
                elif final == "l" and self.primary is not None:
                    self.cells, self.row, self.col, self.top, self.bottom = self.primary
                    self.primary = None
        elif final == "S":
            self._scroll_up(self._value(values, 0))
        elif final == "T":
            self._scroll_down(self._value(values, 0))
        elif final == "L":
            count = self._value(values, 0)
            for _ in range(count):
                if self.top <= self.row <= self.bottom:
                    self.cells[self.row:self.bottom + 1] = [[" "] * self.cols] + self.cells[self.row:self.bottom]
        elif final == "M":
            count = self._value(values, 0)
            for _ in range(count):
                if self.top <= self.row <= self.bottom:
                    self.cells[self.row:self.bottom + 1] = self.cells[self.row + 1:self.bottom + 1] + [[" "] * self.cols]
        elif final in "@PX":
            count = self._value(values, 0)
            if final == "@":
                row = self.cells[self.row]
                self.cells[self.row] = row[:self.col] + [" "] * count + row[self.col:self.cols - count]
            elif final == "P":
                row = self.cells[self.row]
                self.cells[self.row] = row[:self.col] + row[self.col + count:] + [" "] * count
            else:
                self._erase_line(self.col, self.col + count)
        elif final == "g" or final in "ctn":
            return
        else:
            self.unsupported.append("CSI " + body + final)

    def _control(self, value: int) -> None:
        if value == 0x0A or value == 0x0B or value == 0x0C:
            self._line_feed()
        elif value == 0x0D:
            self.col = 0
        elif value == 0x08:
            self.col = max(0, self.col - 1)
        elif value == 0x09:
            self.col = min(self.cols - 1, ((self.col // 8) + 1) * 8)

    def _print_bytes(self, value: int) -> None:
        decoded = self.utf8.decode(bytes([value]), final=False)
        for character in decoded:
            self._put(character)

    def feed(self, data: bytes) -> None:
        for value in data:
            if self.state == "normal":
                if value == 0x1B:
                    self.utf8.reset()
                    self.state = "esc"
                elif value < 0x20 or value == 0x7F:
                    self.utf8.reset()
                    self._control(value)
                else:
                    self._print_bytes(value)
            elif self.state == "esc":
                if value == ord("["):
                    self.sequence.clear(); self.state = "csi"
                elif value == ord("]"):
                    self.osc.clear(); self.state = "osc"
                elif value in (ord("P"), ord("^"), ord("_"), ord("X")):
                    self.state = "string"
                elif value == ord("7"):
                    self.saved = (self.row, self.col); self.state = "normal"
                elif value == ord("8"):
                    self.row, self.col = self.saved; self.state = "normal"
                elif value == ord("D"):
                    self._line_feed(); self.state = "normal"
                elif value == ord("M"):
                    if self.row == self.top: self._scroll_down()
                    else: self.row = max(self.top, self.row - 1)
                    self.state = "normal"
                elif value == ord("E"):
                    self.col = 0; self._line_feed(); self.state = "normal"
                elif value == ord("c"):
                    self._clear(); self.top, self.bottom = 0, self.rows - 1; self.state = "normal"
                elif value in b"()*+,-./#%":
                    self.state = "esc_intermediate"
                else:
                    self.unsupported.append("ESC " + chr(value)); self.state = "normal"
            elif self.state == "esc_intermediate":
                self.state = "normal"
            elif self.state == "csi":
                self.sequence.append(value)
                if 0x40 <= value <= 0x7E:
                    self._csi(bytes(self.sequence)); self.sequence.clear(); self.state = "normal"
            elif self.state == "osc":
                if value == 0x07:
                    self.osc.clear(); self.state = "normal"
                elif value == 0x1B:
                    self.state = "osc_esc"
                else:
                    self.osc.append(value)
            elif self.state == "osc_esc":
                self.osc.clear()
                self.state = "normal" if value == ord("\\") else "osc"
            elif self.state == "string":
                if value == 0x1B:
                    self.state = "string_esc"
            elif self.state == "string_esc":
                self.state = "normal" if value == ord("\\") else "string"

    def text(self) -> str:
        return "\n".join("".join(row).rstrip() for row in self.cells).rstrip() + "\n"


def _kill_and_reap(pid: int, fd: int) -> tuple[int | None, int | None, bool]:
    status = None
    reaped = False
    with contextlib.suppress(OSError):
        os.close(fd)
    for sig in (signal.SIGTERM, signal.SIGKILL):
        try:
            os.killpg(os.getpgid(pid), sig)
        except (ProcessLookupError, PermissionError):
            pass
        deadline = time.monotonic() + (1.5 if sig == signal.SIGTERM else 2.0)
        while time.monotonic() < deadline:
            try:
                waited, wait_status = os.waitpid(pid, os.WNOHANG)
            except ChildProcessError:
                waited = -1
                break
            if waited == pid:
                status = wait_status
                reaped = True
                break
            time.sleep(0.05)
        if reaped:
            break
    if not reaped:
        with contextlib.suppress(ChildProcessError):
            waited, status = os.waitpid(pid, os.WNOHANG)
            reaped = waited == pid
    if not reaped:
        return None, None, False
    if os.WIFEXITED(status):
        return os.WEXITSTATUS(status), None, True
    if os.WIFSIGNALED(status):
        return None, os.WTERMSIG(status), True
    return None, None, True


def _assertions(screen: str, project: str, thread: str | None, content: str | None) -> dict[str, bool]:
    lines = screen.splitlines()
    separators = [line.find("│") for line in lines[1:] if 10 <= line.find("│") < COLS // 2]
    boundary = max(set(separators), key=separators.count) if separators else -1
    sidebar = [line[:boundary] for line in lines[1:]] if boundary >= 0 and separators.count(boundary) >= 3 else []

    def exact_visible(value: str, region: list[str]) -> bool:
        pattern = re.compile(r"(?<![\w])" + re.escape(value) + r"(?![\w])", re.UNICODE)
        return any(pattern.search(line) is not None for line in region)

    checks = {"project": exact_visible(project, sidebar)}
    if thread is not None:
        checks["thread"] = exact_visible(thread, lines[:1])
    if content is not None:
        checks["content"] = exact_visible(content, [line[boundary + 1:] for line in lines[1:]] if sidebar else [])
    return checks


def _validate_expectation(value: str | None, name: str, required: bool = False) -> str | None:
    if value is None:
        if required:
            raise ProbeError(f"--{name} must not be empty")
        return None
    if not value or any(ord(character) < 0x20 or ord(character) == 0x7F for character in value):
        raise ProbeError(f"--{name} must be non-empty visible text")
    return value


def capture(herdr: pathlib.Path, session: str, project: str, thread: str | None, content: str | None) -> tuple[bytes, str, dict, int | None, int | None, int, list[str]]:
    environment = _probe_environment()
    environment.update({"TERM": "xterm-256color", "COLORTERM": "truecolor"})
    command = [str(herdr), "--session", session, "client"]
    pid, fd = os.forkpty()
    if pid == 0:
        os.environ.clear()
        os.environ.update(environment)
        try:
            fcntl.ioctl(0, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))
            os.execvpe(command[0], command, environment)
        except OSError as error:
            os.write(2, f"probe child exec failed: {error}\n".encode())
            os._exit(127)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))
    responder = QueryResponder(fd)
    screen = VTScreen()
    raw = bytearray()
    started = time.monotonic()
    matched_at = None
    checks = _assertions("", project, thread, content)
    try:
        while time.monotonic() - started < PROBE_TIMEOUT:
            wait = min(0.25, PROBE_TIMEOUT - (time.monotonic() - started))
            ready, _, _ = select.select([fd], [], [], max(0.0, wait))
            if ready:
                try:
                    chunk = os.read(fd, 65536)
                except OSError as error:
                    if error.errno in {errno.EIO, errno.EBADF}:
                        break
                    raise
                if not chunk:
                    break
                if len(raw) + len(chunk) > MAX_CAPTURE:
                    raise ProbeError("native client output exceeded the capture limit")
                raw.extend(chunk)
                responder.feed(chunk)
                screen.feed(chunk)
            checks = _assertions(screen.text(), project, thread, content)
            if all(checks.values()):
                if matched_at is None:
                    matched_at = time.monotonic()
                elif time.monotonic() - matched_at >= SETTLE_TIME:
                    break
            else:
                matched_at = None
    finally:
        exit_code, exit_signal, reaped = _kill_and_reap(pid, fd)
    if not reaped:
        raise ProbeError("native client process was not confirmed reaped")
    checks = _assertions(screen.text(), project, thread, content)
    return bytes(raw), screen.text(), checks, exit_code, exit_signal, responder.replies, screen.unsupported[:32]


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--herdr", required=True, help="absolute Herdr executable")
    parser.add_argument("--session", required=True, help="explicit existing named Herdr session")
    parser.add_argument("--output", required=True, help="absolute private JSON receipt or directory")
    parser.add_argument("--expect-project", required=True, help="exact project label expected on the rendered screen")
    parser.add_argument("--expect-thread", help="exact thread label expected on the rendered screen")
    parser.add_argument("--expect-content", help="optional exact content expected on the rendered screen")
    return parser.parse_args(argv)


def run(args: argparse.Namespace) -> int:
    paths = artifact_paths(args.output)
    report = {"ok": False, "session": args.session, "artifacts": {key: str(value) for key, value in paths.items()}}
    try:
        herdr = _ensure_executable(_ensure_absolute(args.herdr, "herdr"))
        session = _validate_session(args.session)
        project = _validate_expectation(args.expect_project, "expect-project", True)
        thread = _validate_expectation(args.expect_thread, "expect-thread")
        content = _validate_expectation(args.expect_content, "expect-content")
        assert project is not None
        report["preflight"] = preflight(herdr, session)
        raw, text, checks, exit_code, exit_signal, replies, unsupported = capture(herdr, session, project, thread, content)
        _write_private(paths["raw"], raw)
        _write_private(paths["text"], text.encode("utf-8"))
        report.update({"ok": all(checks.values()), "assertions": checks, "screen": {"rows": ROWS, "cols": COLS, "captured_bytes": len(raw), "query_replies": replies, "unsupported_sequences": unsupported}, "child": {"exit_code": exit_code, "signal": exit_signal}})
        report["error"] = None if report["ok"] else "one or more exact labels were absent from the reconstructed screen"
    except (ProbeError, OSError, ValueError, AssertionError) as error:
        report["error"] = str(error)
        text = ""
    _write_private(paths["json"], (json.dumps(report, indent=2, sort_keys=True) + "\n").encode("utf-8"))
    if report["ok"]:
        print(json.dumps(report, sort_keys=True), flush=True)
        return 0
    print(json.dumps(report, sort_keys=True), file=sys.stderr, flush=True)
    if text:
        print(text[-3000:], file=sys.stderr, flush=True)
    return 1


if __name__ == "__main__":
    try:
        sys.exit(run(parse_args(sys.argv[1:])))
    except ProbeError as error:
        print(f"probe-project-render: {error}", file=sys.stderr, flush=True)
        sys.exit(1)
