#!/usr/bin/env python3
"""Contract tests for the native Projects render probe."""

from __future__ import annotations

import importlib.util
import os
import pathlib
import tempfile

ROOT = pathlib.Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("probe_project_render", ROOT / "probe-project-render.py")
assert SPEC is not None and SPEC.loader is not None
PROBE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PROBE)


def test_title_is_not_screen_content() -> None:
    screen = PROBE.VTScreen()
    screen.feed(b"\x1b]0;Expected Project\x07")
    assert not PROBE._assertions(screen.text(), "Expected Project", None, None)["project"]


def test_wrong_or_deleted_label_is_rejected() -> None:
    wrong = PROBE.VTScreen()
    wrong.feed("Expected Thread\r\nWrong Project            │\r\n                         │\r\n                         │".encode())
    checks = PROBE._assertions(wrong.text(), "Expected Project", "Expected Thread", None)
    assert checks == {"project": False, "thread": True}

    deleted = PROBE.VTScreen()
    deleted.feed(b"Expected Project")
    deleted.feed(b"\r\x1b[K")
    assert not PROBE._assertions(deleted.text(), "Expected Project", None, None)["project"]


def test_labels_must_appear_in_navigation() -> None:
    frame = "Expected Thread\nExpected Project         │native content\n                         │\n                         │\n"
    assert PROBE._assertions(frame, "Expected Project", "Expected Thread", "native content") == {"project": True, "thread": True, "content": True}
    content_only = "Wrong Thread\nWrong Project            │Expected Project\n                         │Expected Thread\n                         │\n"
    assert PROBE._assertions(content_only, "Expected Project", "Expected Thread", None) == {"project": False, "thread": False}


def test_split_queries_and_utf8_box_drawing() -> None:
    read_fd, write_fd = os.pipe()
    try:
        responder = PROBE.QueryResponder(write_fd)
        responder.feed(b"\x1b]11;")
        responder.feed(b"?")
        responder.feed(b"\x1b]4;2;")
        responder.feed(b"?\x1b")
        responder.feed(b"\\")
        responder.feed(b"\x1b]11;?")
        responder.feed(b"\x1b\\")
        os.close(write_fd)
        write_fd = -1
        replies = os.read(read_fd, 65536)
    finally:
        if write_fd >= 0:
            os.close(write_fd)
        os.close(read_fd)
    assert replies.count(PROBE.BACKGROUND_REPLY) == 2
    assert b"\x1b]4;2;" + PROBE.PALETTE_REPLY + b"\x1b\\" in replies

    screen = PROBE.VTScreen()
    screen.feed("┛".encode())
    assert "┛" in screen.text()


def test_unconfirmed_cleanup_fails_capture() -> None:
    with tempfile.TemporaryDirectory(prefix="probe-render-test-") as temporary:
        fake = pathlib.Path(temporary) / "fake-herdr"
        fake.write_text("#!/bin/sh\nprintf 'Expected Project\\nExpected Thread\\n'\nsleep 5\n", encoding="utf-8")
        fake.chmod(0o700)
        original = PROBE._kill_and_reap

        def false_cleanup(pid: int, fd: int):
            try:
                os.kill(pid, PROBE.signal.SIGKILL)
            except ProcessLookupError:
                pass
            try:
                os.waitpid(pid, 0)
            except ChildProcessError:
                pass
            os.close(fd)
            return None, None, False

        PROBE._kill_and_reap = false_cleanup
        original_settle = PROBE.SETTLE_TIME
        original_timeout = PROBE.PROBE_TIMEOUT
        PROBE.SETTLE_TIME = 0.01
        PROBE.PROBE_TIMEOUT = 0.05
        try:
            try:
                PROBE.capture(fake, "render-test", "Expected Project", "Expected Thread", None)
            except PROBE.ProbeError as error:
                assert "confirmed reaped" in str(error)
            else:
                raise AssertionError("capture accepted unconfirmed cleanup")
        finally:
            PROBE.SETTLE_TIME = original_settle
            PROBE.PROBE_TIMEOUT = original_timeout
            PROBE._kill_and_reap = original


def main() -> int:
    for test in (
        test_title_is_not_screen_content,
        test_wrong_or_deleted_label_is_rejected,
        test_labels_must_appear_in_navigation,
        test_split_queries_and_utf8_box_drawing,
        test_unconfirmed_cleanup_fails_capture,
    ):
        test()
    print("probe-project-render contract tests passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
