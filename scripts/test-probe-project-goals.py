#!/usr/bin/env python3
"""Contract tests for the self-service Projects probe."""

import importlib.util
import pathlib

ROOT = pathlib.Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("probe_project_goals", ROOT / "probe-project-goals.py")
assert SPEC is not None and SPEC.loader is not None
PROBE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PROBE)


def test_selected_dashboard_id_is_display_width_bounded() -> None:
    prefix = "goal-e0f2b16b582fd"
    selected = f"  > ready        {prefix} Documentation\n"
    assert PROBE._selected_goal_prefix(selected) == prefix
    assert PROBE._selected_goal_prefix(selected.replace(">", " ")) is None
    assert PROBE._selected_goal_prefix(f"Summary mentions {prefix}") is None
    assert PROBE._selected_goal_prefix("  > ready        goal-1234 Short") is None


def test_detail_must_match_selected_goal() -> None:
    prefix = "goal-e0f2b16b582fd"
    detail = "  Status: ready\n  Goal: goal-e0f2b16b582fdecd0ae9b546\n"
    assert PROBE._detail_matches_goal(detail, prefix)
    assert not PROBE._detail_matches_goal(detail.replace("e0f2", "a0f2"), prefix)
    assert not PROBE._detail_matches_goal("  Status: ready\n  Goal: " + prefix, prefix)
    assert not PROBE._detail_matches_goal("Summary: " + detail.replace("\n", " "), prefix)


def test_focused_controls_are_not_unfocused_mentions() -> None:
    assert PROBE._focused("  > Goal:\n    a multiline objective", "Goal")
    assert PROBE._focused("  > [ Prepare goal ]", "Prepare goal")
    assert PROBE._focused("  > Duration (minutes): 5", "Duration (minutes)")
    assert not PROBE._focused("  [ Prepare goal ]", "Prepare goal")
    assert not PROBE._focused("  > Goalkeeping: wrong field", "Goal")
    assert not PROBE._focused("Summary mentions > [ Start ]", "Start")


def test_multiline_typing_uses_ordinary_enter() -> None:
    class Session:
        def __init__(self):
            self.inputs = []

        def send(self, data):
            self.inputs.append(data)

        def drain(self, duration):
            return ""

    session = Session()
    PROBE._type_lines(session, "question with spaces\nsecond line")
    assert session.inputs == [b"question with spaces", b"\r", b"second line"]
    assert b"\x0a" not in session.inputs
    assert b"\x13" not in session.inputs


def test_wrapped_commands_keep_every_non_whitespace_character() -> None:
    command = '["go","test","./internal/surface/goal"]'
    screen = 'Commands: ["go","test","./internal/surface/goa\n  l"]'
    assert PROBE._visible_wrapped(screen, command)
    assert not PROBE._visible_wrapped(screen.replace("goa", "gob"), command)
    assert not PROBE._visible_wrapped(screen.replace("goa", "go"), command)
    assert not PROBE._visible_wrapped("flow.\\nq d v c", "q d v c")
    assert PROBE._visible_wrapped("flow.\nq d v c", "q d v c")


def test_scan_detects_movement_even_when_start_and_finish_match() -> None:
    class Session:
        def __init__(self, movable):
            self.movable = movable
            self.position = "bottom"

        def send(self, data):
            if self.movable:
                self.position = "top" if data == b"\x1b[5~" else "bottom"

        def drain(self, duration):
            return self.position

    text, changed = PROBE._scan(Session(True))
    assert changed and text.startswith("bottom") and text.endswith("bottom")
    assert PROBE._scan(Session(False))[1] is False


def test_check_scan_preserves_lines_across_page_boundaries() -> None:
    class Session:
        def __init__(self):
            self.position = 0
            self.lines = ['before'] * 2 + ['complete wrapped', 'receipt line', 'with tail'] + ['after'] * 8

        def send(self, data):
            movement = {b'\x1b[5~': -8, b'\x1b[6~': 8, b'\x1b[B': 1}[data]
            self.position = min(max(self.position + movement, 0), len(self.lines) - 4)

        def drain(self, duration):
            return 'Pinned header\n' + '\n'.join(self.lines[self.position:self.position + 4]) + '\nFooter'

    expected = 'complete wrapped receipt line with tail'
    paged, _ = PROBE._scan(Session())
    assert not PROBE._visible_wrapped(paged, expected)
    linewise, changed = PROBE._scan(Session(), linewise=True)
    assert changed and PROBE._visible_wrapped(linewise, expected)
    assert not PROBE._visible_wrapped(linewise.replace('with tail', 'without tail'), expected)


if __name__ == "__main__":
    test_selected_dashboard_id_is_display_width_bounded()
    test_detail_must_match_selected_goal()
    test_focused_controls_are_not_unfocused_mentions()
    test_multiline_typing_uses_ordinary_enter()
    test_wrapped_commands_keep_every_non_whitespace_character()
    test_scan_detects_movement_even_when_start_and_finish_match()
    test_check_scan_preserves_lines_across_page_boundaries()
    print("Projects goal probe contract tests passed")
