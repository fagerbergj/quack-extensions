# github

quack's GitHub App integration: webhook triggers, dispatch envelopes, staged delivery, CI auto-heal and merge.

## Decision points

The extension declares five observe-only points (`DecisionPoints()` in `decisions.go`). quack namespaces each as `ext:github/<name>` and records the handler's answer beside the extension's own; no answer, error or timeout changes what the extension posts or dispatches. Every call runs in a goroutine off the posting or dispatch path, one at a time, each capped at 10s.

| Point | Primary question | Asked per | Baseline |
| --- | --- | --- | --- |
| `finding.severity` | `severity` (blocking, suggestion, nit, question) | labelled review finding | the finding's label |
| `finding.blocking` | `blocking` (noul) | review finding | `true` when labelled blocking |
| `review.verdict` | `verdict` (approve, comment, request_changes) | posted review | the staged verdict |
| `intent` | `write` (noul), plus `deliverable` (reply, review, commit, pull_request, plan) | free-text mention on an issue or PR | `true` when the classifiers picked a commit or a new PR |
| `ci.flaky` | `flaky` (noul) | failing check, up to 5, on the auto-heal and `quack:fix` paths | none |

`intent` is asked only for mentions; label triggers, slash commands and synthetic runs (CI fix, review follow-up) classify nothing. Its state is the comment body, the issue or PR title, the sender and their author association, and the grant (the delivery kinds labels and authorship allow). The baseline covers `write` only: the extension's deliverable pick is in that chat's envelope `<deliverable>` block, not in the decision record.

`ci.flaky` has no baseline at failure time. Its state carries `repo`, `pr`, `head_sha` and `check`, plus the check's summary and annotations and the PR's changed files (first 50). A scorer labels a decision by joining `(repo, head_sha, check)` to that check run's later conclusion on the same sha: a re-run that passes means flaky, a re-run that fails again means a real failure, and no re-run leaves it unlabelled. The check-runs API returns the latest run of that check on the sha by default; add `filter=all` to list every run (`GET /repos/{repo}/commits/{head_sha}/check-runs?check_name={check}&filter=all`). The extension never re-runs a check itself.

Each request names its PR or issue chat (`DecideRequest.ChatID`), so quack attributes the webhook-side asks, which have no run context, to that chat's ledger.
