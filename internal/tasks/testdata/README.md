# Task update fixtures

`task-update-before.json` and `task-update-after.json` are one task before and after it is marked done, with every value synthetic.

They are written by hand from Vikunja's documented task shape. They have **not** been matched against a live instance. The live probe in `../liveprobe_test.go` (build tag `liveprobe`) writes sanitized replacements for both files. Replace these with its output once the probe has run, and delete this notice.

Between the two files these keys differ: `done`, `done_at`, `updated`, `description`, `bucket_id`. The expected-to-change list `CompleteTask` uses is those five plus `position`, which a column move can also change.
