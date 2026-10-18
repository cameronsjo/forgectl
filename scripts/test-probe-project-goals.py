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


if __name__ == "__main__":
    test_selected_dashboard_id_is_display_width_bounded()
    test_detail_must_match_selected_goal()
    print("Projects goal probe contract tests passed")
