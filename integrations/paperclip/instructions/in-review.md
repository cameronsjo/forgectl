<!-- forgectl-paperclip:in-review-rule -->
## Review handoff (operator rule)

This rule overrides the final-disposition checklist above.

- Never set an issue to `done`. A person closes it, usually by merging a pull request that says `Closes #N`.
- When the work is committed, post one comment naming the branch and the commit, then hand the issue to review in a single update:
  `PATCH /api/issues/{issueId}` with `{"status": "in_review", "assigneeUserId": "{{REVIEWER_USER_ID}}", "assigneeAgentId": null}`.
- Issues you create yourself start in `backlog`, not `todo`.
