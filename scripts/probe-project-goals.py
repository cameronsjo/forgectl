#!/usr/bin/env python3
"""Probe the Projects Goals form over a real pty without starting a goal."""

from __future__ import annotations

import argparse
import contextlib
import errno
import fcntl
import importlib.util
import json
import os
import pathlib
import re
import select
import struct
import subprocess
import sys
import termios
import time


class ProbeError(Exception):
    pass


def _render_probe_module():
    path = pathlib.Path(__file__).with_name("probe-project-render.py")
    spec = importlib.util.spec_from_file_location("probe_project_render", path)
    if spec is None or spec.loader is None:
        raise ProbeError(f"could not load PTY helpers from {path}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


RENDER_PROBE = _render_probe_module()
QueryResponder = RENDER_PROBE.QueryResponder
VTScreen = RENDER_PROBE.VTScreen

MAX_CAPTURE = 16 * 1024 * 1024
PROBE_TIMEOUT = 24.0
RED = "\x1b[31m"
RESET = "\x1b[0m"


def _absolute(value: str, name: str) -> pathlib.Path:
    path = pathlib.Path(value)
    if not path.is_absolute():
        raise ProbeError(f"--{name} must be an absolute path")
    return path


def _executable(value: str) -> pathlib.Path:
    path = _absolute(value, "forgectl")
    try:
        path.stat()
    except OSError as error:
        raise ProbeError(f"--forgectl is not executable: {path}: {error}") from error
    if not path.is_file() or not os.access(path, os.X_OK):
        raise ProbeError(f"--forgectl is not a regular executable: {path}")
    return path.resolve()


def _directory(value: str, name: str, require_existing: bool = True) -> pathlib.Path:
    path = _absolute(value, name)
    if path.is_symlink():
        raise ProbeError(f"refusing symlinked --{name}: {path}")
    if not path.exists():
        if require_existing:
            raise ProbeError(f"--{name} does not exist: {path}")
        return path
    if not path.is_dir():
        raise ProbeError(f"--{name} is not a directory: {path}")
    return path.resolve()


def _private_directory(value: str) -> pathlib.Path:
    path = _absolute(value, "output")
    if path.exists() and path.is_symlink():
        raise ProbeError(f"refusing symlinked output path: {path}")
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    if not path.is_dir() or path.stat().st_mode & 0o077:
        raise ProbeError(f"output directory must be private: {path}")
    return path


def _write_private(path: pathlib.Path, data: bytes) -> None:
    if path.exists() and path.is_symlink():
        raise ProbeError(f"refusing symlinked receipt: {path}")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    descriptor = os.open(path, flags, 0o600)
    try:
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(data)
    except Exception:
        with contextlib.suppress(OSError):
            os.close(descriptor)
        raise
    os.chmod(path, 0o600)


def _environment() -> dict[str, str]:
    environment = RENDER_PROBE._probe_environment()
    environment.update({"TERM": "xterm-256color", "COLORTERM": "truecolor"})
    return environment


def _exact_visible(screen: str, value: str) -> bool:
    pattern = re.compile(r"(?<![\w])" + re.escape(value) + r"(?![\w])", re.UNICODE)
    return pattern.search(screen) is not None


def _send_all(fd: int, data: bytes) -> None:
    view = memoryview(data)
    while view:
        try:
            written = os.write(fd, view)
        except OSError as error:
            raise ProbeError(f"pty input failed: {error}") from error
        view = view[written:]


class PTYSession:
    def __init__(self, executable: pathlib.Path, root: pathlib.Path, model: str, repository: pathlib.Path, rows: int, cols: int):
        self.executable = executable
        self.root = root
        self.model = model
        self.repository = repository
        self.rows = rows
        self.cols = cols
        self.pid: int | None = None
        self.fd: int | None = None
        self.responder = None
        self.screen = VTScreen(rows, cols)
        self.raw = bytearray()

    def start(self) -> None:
        command = [str(self.executable), "projects", "goals", "--root", str(self.root), "--model", self.model]
        pid, fd = os.forkpty()
        self.pid, self.fd = pid, fd
        if pid == 0:
            environment = _environment()
            try:
                os.chdir(self.repository)
                fcntl.ioctl(0, termios.TIOCSWINSZ, struct.pack("HHHH", self.rows, self.cols, 0, 0))
                os.execvpe(command[0], command, environment)
            except OSError as error:
                os.write(2, f"probe child exec failed: {error}\n".encode())
                os._exit(127)
        fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", self.rows, self.cols, 0, 0))
        self.responder = QueryResponder(fd)

    def _read_once(self, timeout: float) -> bool:
        if self.fd is None:
            return False
        try:
            ready, _, _ = select.select([self.fd], [], [], max(0.0, timeout))
        except InterruptedError:
            return True
        if not ready:
            return False
        try:
            chunk = os.read(self.fd, 65536)
        except OSError as error:
            if error.errno in {errno.EIO, errno.EBADF, errno.EPIPE}:
                return False
            raise ProbeError(f"pty output failed: {error}") from error
        if not chunk:
            return False
        if len(self.raw) + len(chunk) > MAX_CAPTURE:
            raise ProbeError("goal TUI output exceeded the capture limit")
        self.raw.extend(chunk)
        self.responder.feed(chunk)
        self.screen.feed(chunk)
        return True

    def wait_for(self, predicate, label: str, timeout: float = PROBE_TIMEOUT) -> str:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            text = self.screen.text()
            if predicate(text):
                return text
            self._read_once(min(0.25, max(0.0, deadline - time.monotonic())))
        raise ProbeError(f"timed out waiting for {label}; last screen:\n{self.screen.text()[-3000:]}")

    def drain(self, duration: float) -> str:
        deadline = time.monotonic() + duration
        while time.monotonic() < deadline:
            self._read_once(min(0.1, max(0.0, deadline - time.monotonic())))
        return self.screen.text()

    def send(self, data: bytes) -> None:
        if self.fd is None:
            raise ProbeError("pty session is not running")
        _send_all(self.fd, data)

    def close(self) -> dict[str, int | bool | None]:
        if self.pid is None or self.fd is None:
            return {"reaped": True, "exit_code": None, "signal": None}
        exit_code, exit_signal, reaped = RENDER_PROBE._kill_and_reap(self.pid, self.fd)
        self.pid, self.fd = None, None
        if not reaped:
            raise ProbeError("owned goal TUI process group was not confirmed reaped")
        return {"reaped": reaped, "exit_code": exit_code, "signal": exit_signal}


def _fixture_args(args: argparse.Namespace) -> dict[str, str]:
    acceptance = args.acceptance or [
        "The requested objective is independently reviewed.",
        "The source checkout remains isolated.",
    ]
    scopes = args.scope or ["src"]
    return {
        "repository": str(args.repository),
        "title": args.title,
        "objective": args.objective,
        "scope": ", ".join(scopes),
        "acceptance": "\n".join(acceptance),
        "checks": args.checks,
        "model": args.model,
        "duration": str(args.duration),
    }


def _assert_dashboard(screen: str, negative: str) -> dict[str, bool]:
    checks = {
        "header": _exact_visible(screen, "Projects · Goals"),
        "empty": _exact_visible(screen, "No goals yet."),
        "new_goal": _exact_visible(screen, "New goal"),
        "negative_absent": not _exact_visible(screen, negative),
    }
    return checks


def _visible_wrapped(screen: str, value: str) -> bool:
    normalized_screen = re.sub(r"\s+", " ", screen).strip()
    normalized_value = re.sub(r"\s+", " ", value).strip()
    if _exact_visible(normalized_screen, normalized_value):
        return True
    characters = [r"\s+" if character.isspace() else re.escape(character) for character in normalized_value]
    pattern = r"(?<!\w)" + r"\s*".join(characters) + r"(?!\w)"
    return re.search(pattern, screen) is not None


def _focused(screen: str, label: str) -> bool:
    return re.search(r"(?m)^\s*>\s+(?:\[\s*)?" + re.escape(label) + r"(?=[:\s\]]|$)", screen) is not None


def _focus(session: PTYSession, label: str) -> str:
    for _ in range(16):
        screen = session.drain(0.08)
        if _focused(screen, label):
            return screen
        session.send(b"\t")
    raise ProbeError(f"could not focus {label}; last screen:\n{session.screen.text()}")


def _type_lines(session: PTYSession, value: str) -> None:
    for index, line in enumerate(value.split("\n")):
        if index:
            session.send(b"\r")
        session.send(line.encode())
        session.drain(0.05)


def _fill(session: PTYSession, label: str, value: str) -> None:
    _focus(session, label)
    session.send(b"\x15")
    _type_lines(session, value)
    session.send(b"\t")


def _scan(session: PTYSession, linewise: bool = False) -> tuple[str, bool]:
    screens = [session.drain(0.08)]
    for _ in range(16):
        session.send(b"\x1b[5~")
        screens.append(session.drain(0.025))
    for _ in range(256 if linewise else 32):
        session.send(b"\x1b[B" if linewise else b"\x1b[6~")
        screens.append(session.drain(0.025))
    return "\n".join(screens), len(set(screens)) > 1


def _registry_ids(root: pathlib.Path) -> list[str]:
    goals = root / "goals"
    if not goals.exists():
        return []
    return sorted(path.name for path in goals.iterdir() if path.is_dir())


def _run_setup(executable: pathlib.Path, root: pathlib.Path, repository: pathlib.Path, args: argparse.Namespace, rows: int, cols: int, output: pathlib.Path) -> dict:
    values = _fixture_args(args)
    session = PTYSession(executable, root, args.model, repository, rows, cols)
    before_ids = _registry_ids(root)
    try:
        session.start()
        dashboard = session.wait_for(lambda text: "No goals yet." in text, "empty dashboard")
        dashboard_checks = _assert_dashboard(dashboard, args.negative_expected_label)
        if not all(dashboard_checks.values()):
            if not dashboard_checks["negative_absent"]:
                raise ProbeError("negative expected label appeared in reconstructed screen")
            raise ProbeError(f"dashboard assertions failed: {dashboard_checks}")
        session.send(b"\r")
        session.wait_for(lambda text: _focused(text, "Repository"), "single setup form")
        _focus(session, "Preset")
        for _ in range(4):
            if "[Docs]" in session.drain(0.08):
                break
            session.send(b"\x1b[C")
        session.wait_for(lambda text: "[Docs]" in text, "Docs preset")
        _focus(session, "Goal")
        _type_lines(session, values["objective"])
        session.send(b"\r")
        _type_lines(session, "q d v c stay ordinary goal text")
        session.send(b"\t")
        _focus(session, "Prepare goal")
        session.send(b"\r")
        minimal_review = session.wait_for(lambda text: _focused(text, "Edit"), "prepared approval with safe default")
        minimal_scan, _ = _scan(session)
        minimal_review += "\n" + minimal_scan
        _focus(session, "Edit")
        session.send(b"\r")
        _focus(session, "Advanced")
        session.send(b"\r")
        session.wait_for(lambda text: "Advanced: expanded" in text, "inline advanced settings")
        for label, field in (("Title", "title"), ("Allowed scope", "scope"), ("Acceptance", "acceptance"), ("Checks", "checks"), ("OpenAI model", "model"), ("Duration (minutes)", "duration")):
            _fill(session, label, values[field])
        _focus(session, "Prepare goal")
        session.send(b"\r")
        review_before = session.wait_for(lambda text: _focused(text, "Edit"), "custom approval")
        combined, scrolled = _scan(session)
        rendered_checks = [json.dumps(check, separators=(",", ":")) for check in json.loads(values["checks"])]
        checks = {
            "single_form": "step 1/8" not in minimal_review,
            "docs_preset": _visible_wrapped(minimal_review, "docs"),
            "ordinary_multiline_text": _visible_wrapped(minimal_review, "q d v c stay ordinary goal text"),
            "safe_approval_focus": _focused(review_before, "Edit"),
            "review": _exact_visible(combined, "Approval"),
            "title": _exact_visible(combined, values["title"]),
            "scope": _exact_visible(combined, values["scope"]),
            "model": _exact_visible(combined, values["model"]),
            "duration": _exact_visible(combined, values["duration"] + " minutes"),
            "objective": _visible_wrapped(combined, values["objective"]),
            "acceptance": all(_visible_wrapped(combined, line) for line in values["acceptance"].splitlines() if line),
            "checks": all(_visible_wrapped(combined, check) for check in rendered_checks),
            "scrolled": scrolled,
            "negative_absent": not _exact_visible(combined, args.negative_expected_label),
        }
        _focus(session, "Edit")
        session.send(b"\r")
        edit_screen = session.wait_for(lambda text: _focused(text, "Prepare goal"), "edit invalidates approval")
        checks.update({
            "approval_invalidated": "[ Start ]" not in edit_screen,
            "negative_absent": checks["negative_absent"] and not _exact_visible(edit_screen, args.negative_expected_label),
            "no_run_created": before_ids == _registry_ids(root),
        })
        process = session.close()
        checks.update({"captured": bool(session.raw), "reaped": bool(process.get("reaped"))})
        raw_path = output / f"{rows}x{cols}-setup.raw"
        screen_path = output / f"{rows}x{cols}-review.txt"
        _write_private(raw_path, bytes(session.raw))
        _write_private(screen_path, (minimal_review + "\n" + combined).encode())
        return {"size": {"rows": rows, "cols": cols}, "dashboard": dashboard, "minimal_review": minimal_review, "review": combined, "assertions": checks, "process": process, "artifacts": {"raw": str(raw_path), "review": str(screen_path)}}
    finally:
        if session.pid is not None:
            session.close()


def _selected_goal_prefix(screen: str) -> str | None:
    match = re.search(r"(?m)^\s*>\s+\S+\s+(goal-[a-f0-9]{13})(?:\s|$)", screen)
    return match.group(1) if match else None


def _detail_matches_goal(screen: str, prefix: str) -> bool:
    return re.search(r"(?m)^\s*Goal:\s+" + re.escape(prefix) + r"[a-f0-9]{11}\b", screen) is not None


def _goal_json(executable: pathlib.Path, root: pathlib.Path, goal_id: str, operation: str, *options: str) -> dict:
    command = [str(executable), "projects", "goal", operation, goal_id, "--root", str(root), "--json", *options]
    result = subprocess.run(command, capture_output=True, text=True, timeout=15, check=False, env=_environment())
    if result.returncode != 0:
        raise ProbeError(f"goal {operation} failed ({result.returncode}): {result.stderr}")
    value = json.loads(result.stdout)
    if not isinstance(value, dict):
        raise ProbeError(f"goal {operation} did not return an object")
    return value


def _run_inspect(executable: pathlib.Path, root: pathlib.Path, repository: pathlib.Path, args: argparse.Namespace, rows: int, cols: int, output: pathlib.Path) -> dict:
    session = PTYSession(executable, root, args.model, repository, rows, cols)
    process = None
    try:
        session.start()
        dashboard = session.wait_for(lambda text: _selected_goal_prefix(text) is not None, "first retained goal")
        selected = _selected_goal_prefix(dashboard)
        if selected is None:
            raise ProbeError("no selected goal in reconstructed dashboard")
        session.send(b"\r")
        detail = session.wait_for(lambda text: _exact_visible(text, "Status") and _detail_matches_goal(text, selected), "selected goal detail")
        goal_id = re.search(r"(?m)^\s*Goal:\s+(goal-[a-f0-9]{24})\b", detail).group(1)
        state = _goal_json(executable, root, goal_id, "status")
        if state.get("id") != goal_id or not isinstance(state.get("checks"), list) or not state["checks"]:
            raise ProbeError("retained goal status has no matching identity/check receipts")
        expected_outputs = []
        for index in range(len(state["checks"])):
            receipt = _goal_json(executable, root, goal_id, "artifact", "--kind", "check", "--index", str(index))
            if receipt.get("goal_id") != goal_id or receipt.get("kind") != "check" or not isinstance(receipt.get("content"), str):
                raise ProbeError("check artifact identity/content mismatch")
            expected_outputs.append(receipt["content"])
        artifacts = {"detail": detail}
        arrow_scrolled = False
        for key, label, heading in ((b"2", "patch", "Evidence · Diff"), (b"3", "check", "Evidence · Checks"), (b"4", "json", "Details · JSON")):
            session.send(key)
            screen = session.wait_for(lambda text, expected=heading: _exact_visible(text, expected), f"{label} evidence tab")
            if label == "patch":
                loaded = session.wait_for(lambda text: "diff --git" in text, "loaded diff content")
                session.send(b"\x1b[B")
                arrow_scrolled = session.drain(0.1) != loaded
            scanned, _ = _scan(session, linewise=label == "check")
            artifacts[label] = screen + "\n" + scanned
        process = session.close()
        assertions = {
            "detail": _exact_visible(detail, "Status") and _detail_matches_goal(detail, selected),
            "patch_content": _visible_wrapped(artifacts["patch"], "diff --git"),
            "arrow_scrolled": arrow_scrolled,
            "all_check_outputs": all(all(_visible_wrapped(artifacts["check"], line) for line in receipt.splitlines() if line.strip()) for receipt in expected_outputs),
            "all_check_headings": all(_visible_wrapped(artifacts["check"], f"Check {index + 1}") for index in range(len(expected_outputs))),
            "json_version": re.search(r'"version"\s*:\s*1\b', artifacts["json"]) is not None,
            "stable_identity": all(_detail_matches_goal(text, selected) and "Status:" in text for text in artifacts.values()),
            "negative_absent": all(not _exact_visible(text, args.negative_expected_label) for text in artifacts.values()),
            "captured": bool(session.raw),
            "reaped": bool(process.get("reaped")),
        }
        paths = {}
        for label, text in artifacts.items():
            path = output / f"{rows}x{cols}-inspect-{label}.txt"
            _write_private(path, text.encode())
            paths[label] = str(path)
        return {"size": {"rows": rows, "cols": cols}, "dashboard": dashboard, "artifacts": artifacts, "assertions": assertions, "process": process, "receipt_artifacts": paths}
    finally:
        if session.pid is not None:
            process = session.close()


def _validate_text(value: str, name: str) -> str:
    if not value or any(ord(character) < 0x20 and character not in "\n\t" for character in value) or any(ord(character) == 0x7F for character in value):
        raise ProbeError(f"--{name} must be visible text")
    return value


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--forgectl", required=True, help="absolute compiled forgectl executable")
    parser.add_argument("--root", required=True, help="absolute private goal registry root")
    parser.add_argument("--repository", required=True, help="absolute Git checkout supplied to the form")
    parser.add_argument("--model", required=True, help="exact model supplied to projects goals")
    parser.add_argument("--output", required=True, help="absolute private artifact directory")
    parser.add_argument("--title", default="Acceptance Goal")
    parser.add_argument("--objective", default="Exercise the bounded self-service goal flow.")
    parser.add_argument("--scope", action="append", default=None, help="literal allowed path; repeat for multiple paths")
    parser.add_argument("--acceptance", action="append", default=None, help="acceptance criterion; repeat for multiple criteria")
    parser.add_argument("--checks", default='[["git","diff","--check"],["go","test","./internal/surface/goal"]]', help="JSON argument arrays for verification")
    parser.add_argument("--duration", type=int, default=5)
    parser.add_argument("--negative-expected-label", default="Native goal started", help="label that must remain absent; presence is reported in red")
    parser.add_argument("--inspect", action="store_true", help="read the first retained goal detail and patch/check/JSON artifacts")
    return parser.parse_args(argv)


def run(args: argparse.Namespace) -> int:
    report = {"ok": False, "started_native_goal": False, "checks": {}, "error": None}
    output = None
    report_path = None
    try:
        executable = _executable(args.forgectl)
        root = _directory(args.root, "root", False)
        repository = _directory(args.repository, "repository")
        args.model = _validate_text(args.model, "model")
        args.title = _validate_text(args.title, "title")
        args.objective = _validate_text(args.objective, "objective")
        args.negative_expected_label = _validate_text(args.negative_expected_label, "negative-expected-label")
        if args.duration < 1 or args.duration > 30:
            raise ProbeError("--duration must be between 1 and 30")
        json.loads(args.checks)
        output = _private_directory(args.output)
        run_id = f"{time.time_ns()}-{os.getpid()}"
        run_dir = output / run_id
        run_dir.mkdir(mode=0o700)
        report_path = run_dir / "report.json"
        if args.inspect:
            wide = _run_inspect(executable, root, repository, args, 24, 100, run_dir)
            narrow = _run_inspect(executable, root, repository, args, 16, 50, run_dir)
            report["inspect"] = {"wide": wide, "narrow": narrow}
            checks = {"wide": wide["assertions"], "narrow": narrow["assertions"]}
        else:
            wide = _run_setup(executable, root, repository, args, 24, 100, run_dir)
            narrow = _run_setup(executable, root, repository, args, 16, 50, run_dir)
            report.update({"wide": wide, "narrow": narrow})
            checks = {"wide": wide["assertions"], "narrow": narrow["assertions"]}
        report.update({"executable": str(executable), "root": str(root), "repository": str(repository), "model": args.model, "checks": checks})
        report["ok"] = all(all(values.values()) for values in checks.values())
        if not report["ok"]:
            if any(not values.get("negative_absent", True) for values in checks.values()):
                raise ProbeError("negative expected label appeared in reconstructed screen")
            raise ProbeError("one or more goal probe assertions failed")
    except (OSError, ProbeError, ValueError, json.JSONDecodeError, subprocess.TimeoutExpired) as error:
        report["error"] = str(error)
    if output is not None and report_path is not None:
        report["report"] = str(report_path)
        _write_private(report_path, (json.dumps(report, indent=2, sort_keys=True) + "\n").encode())
    encoded = json.dumps(report, sort_keys=True)
    if report["ok"]:
        print(encoded, flush=True)
        return 0
    if report.get("error", "").find("negative expected label") >= 0:
        print(f"{RED}{encoded}{RESET}", file=sys.stderr, flush=True)
    else:
        print(encoded, file=sys.stderr, flush=True)
    return 1


if __name__ == "__main__":
    sys.exit(run(parse_args(sys.argv[1:])))
