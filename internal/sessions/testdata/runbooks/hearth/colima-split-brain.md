---
title: Colima brew-services XDG split brain
project: hearth
type: field-report
session_id: 11111111-1111-1111-1111-111111111111
---

# Colima brew-services XDG split brain

Launchd-started colima ignores XDG_CONFIG_HOME and boots a phantom default VM
from ~/.colima while the real profile sits stopped. The split brain persists
because colima prefers ~/.colima forever once it exists.
