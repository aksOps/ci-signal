# Compatibility and verification

## Supported pins

| Component | Supported version | Evidence |
| --- | --- | --- |
| Go language/toolchain | `go 1.25`, `toolchain go1.26.5` | [go.mod](../go.mod) |
| Copilot Go SDK | `v1.0.13` | [official SDK release](https://github.com/github/copilot-sdk/releases/tag/go%2Fv1.0.13) |
| Copilot CLI | `1.0.83` | [official CLI release](https://github.com/github/copilot-cli/releases/tag/v1.0.83) |
| ast-grep | `0.45.3` | [official release](https://github.com/ast-grep/ast-grep/releases/tag/0.45.3) |
| GitLab API target | `18.9.0-ee` | [merge requests](https://gitlab.com/gitlab-org/gitlab/-/blob/v18.9.0-ee/doc/api/merge_requests.md), [notes](https://gitlab.com/gitlab-org/gitlab/-/blob/v18.9.0-ee/doc/api/notes.md), [discussions](https://gitlab.com/gitlab-org/gitlab/-/blob/v18.9.0-ee/doc/api/discussions.md), [labels](https://gitlab.com/gitlab-org/gitlab/-/blob/v18.9.0-ee/doc/api/labels.md), [users](https://gitlab.com/gitlab-org/gitlab/-/blob/v18.9.0-ee/doc/api/users.md) |
| Ollama Cloud wire API | OpenAI-compatible Responses | [Ollama compatibility documentation](https://docs.ollama.com/api/openai-compatibility), [Ollama Cloud authentication](https://docs.ollama.com/cloud) |

The pinned Linux x86_64 ast-grep archive SHA256 is `f8ac830881339d1edee6b2652f54798c0f4da5a827f2db38a08ee31117783ce8`. The pinned Linux x64 Copilot CLI archive SHA256 is `888f8fbb4575c335afba4a8863c647ef04f81e5124c7c794bdcaee90c5fa4503`. Image construction verifies the appropriate archive before extraction.

The selected Go dependencies use compatible OSI licenses: Copilot Go SDK and Goldmark are MIT; GitLab API client-go and `jsonschema` are Apache-2.0. The project pins versions in [go.mod](../go.mod) and [go.sum](../go.sum). The bundled Copilot CLI has its own redistribution license, retained in the image at `/opt/copilot/LICENSE.md`, and is not covered by this project's MIT License.

## Verified with local fixtures

The focused test suite verifies these application guarantees without live credentials:

- strict configuration, secret separation, provider lock, label-shape validation, absolute config selection, and launch-directory-independent relative paths;
- explicit BYOK session configuration, disabled subscription authentication, skill invocation, native tool calls, correction bounds, telemetry integrity, cancellation, and credential redaction through a fake runtime;
- a real Copilot CLI 1.0.83 protocol exchange through a local OpenAI Responses stub when `COPILOT_CLI_PATH` is supplied;
- pinned Git snapshots, additions, deletions, renames, cross-file search, bounded continuation, guidance materialization, Go declaration extraction, configured whole-project structural scans, and fallback coverage in temporary repositories;
- strict `submit_review` validation, accepted-argument persistence, conservative verdict policy, paired addressed reassessment and code-change acknowledgement, and durable source provenance;
- Markdown round trips, renderer escaping, checkbox check/uncheck, late human reversal, inert AI-control tampering, and the renderer-produced sample;
- GitLab credential routing, single fallback, API-only writes, redirect restrictions, pagination failure, targeted label updates, retry recovery, successor verification, versioned state digests with exact historical-report verification, note-history preservation, late controls, ownership and binding checks, restricted-source rejection, and report overflow;
- API-token-only author classification through GitLab's Users API, including cached human/bot results and fail-closed denied or malformed responses;
- coordinator fingerprint reuse, changed-context reassessment, bounded batching, cross-file findings, checkpoint recovery, checkbox-only synchronization without AI, partial-review policy, and stale-publication handling.

The local CLI protocol fixture is stronger than a mocked SDK test, but it still uses a local Responses server. By itself, it is not evidence that Ollama Cloud accepted the model, that provider usage events are exposed in production, or that a GitLab instance accepted the final requests.

## Live verification on 2026-09-06

An authorized, bounded fixture used GitLab 19.4-pre, merge request `!1`, Ollama Cloud, Copilot CLI 1.0.83, and `deepseek-v4-flash:cloud`. The initial allowance of three provider sessions was consumed:

- Session 1, job `16329785758`, started a live provider session but produced no accepted submission. It ended with `waiting for session.idle: context canceled`; the underlying cause and any model, BYOK, budget, or usage detail are unrecoverable from that run. The published report conservatively recorded all nine units as failed.
- Session 2, job `16329883750`, completed all nine assigned units and published note `3793232786` with two blocker findings. Requested and observed model identities were both exactly `deepseek-v4-flash:cloud`. Eight usage events recorded 102,143 input tokens and 11,872 output tokens. Successful tool events covered `repository_search`, `git_read`, `repository_read`, `ast_grep`, `structural_scan`, and `submit_review`.
- After the preceding task-related fixes, a zero-diff control correctly avoided AI but exposed that two prior findings were not scheduled for reassessment. The coordinator now creates one pathless, host-owned reassessment unit when prior findings exist and the changed snapshot has no eligible diff units.
- Session 3, job `16329956406`, completed that reassessment unit in 127.9 seconds. Both persistent findings retained stable IDs and an open workflow state while their assessment moved from `present` to `addressed` through explicit, source-backed reassessments. No AI acknowledgement was proposed. Note `3793255743` was approved with complete coverage. Requested and observed model identities again matched exactly. Fourteen usage events recorded 174,092 input tokens and 14,307 output tokens. `git_read`, `repository_read`, `structural_scan`, and `submit_review` succeeded; one `repository_search` attempt failed before a later search succeeded.

Two intervening checkbox controls, jobs `16329896885` and `16329906152`, each ran with `used_ai=false`. They preserved the accepted run and telemetry while recording the finding history transitions from created to checkbox-acknowledged and then checkbox-reopened. Successor report creation, exact readback verification, predecessor retirement, and one-current-report recovery were exercised against the live merge request.

The two successful sessions in that initial allowance recorded 302,414 tokens in total. This is only a lower bound for the exercise because session 1 usage was unavailable.

A later allowance authorized two additional sessions. Session 4, [job `16330178872`](https://gitlab.com/aksops/ci-signal-test/-/jobs/16330178872), used one of them and completed the single reassessment unit in 22.3 seconds. It used source commit `d28a5b92adad4dbdba084ab08b7b8f25cd918c1f` and image `ghcr.io/aksops/ci-signal@sha256:4a9a53dff3cc98d1ae143e861d0da9ef2568d732e148849e6b6c776689c5b5dd` against unchanged fixture head `7532624f46c01a5476ae7102b86c521b3fe6636e`. Requested and observed model identities were `deepseek-v4-flash:cloud`. Complete usage telemetry recorded 96,274 input tokens and 5,441 output tokens. `repository_read`, `git_read`, `repository_search`, and `submit_review` succeeded.

The sole current [report, note `3793310366`](https://gitlab.com/aksops/ci-signal-test/-/merge_requests/1#note_3793310366), shows Approved, zero open findings, and two acknowledged findings under the collapsible Blocker/Corrections group. Both rows show the AI robot and Addressed tick with no checkboxes. Each finding received an explicit addressed reassessment followed by an `ai_code_change` acknowledgement whose source references occur in that reassessment's evidence. Stable finding IDs, prior runs, and all preceding history, including the human check/uncheck, were preserved. The old report recovered through historical digest verification, its successor uses a `state-v1:` canonical-state digest, and predecessor note `3793255743` was retired.

These findings are model assessments, not deterministic test results. Go declarations used the configured ast-grep extraction. The other five fixture languages used explicit file fallback units; the live run is not evidence of six-language AST support.

## Remaining unverified compatibility

Repository checks do not perform live Ollama Cloud requests or GitLab mutations. The authorized fixture above does not verify:

- authentication and endpoint behavior on GitLab 18.9 Ultimate EE, including installation-specific job-token permissions;
- AI discussion acknowledgement and `GET /users/:id` author classification in a live run, because no eligible human non-system note existed;
- preservation of unrelated existing labels, because the merge request started with none;
- runner, network, certificate, proxy, and container-platform compatibility outside the tested GitLab 19.4-pre fixture;
- deterministic finding accuracy across repositories, languages, or repeated model runs.

The reviewer reports absent provider telemetry as unknown. It reports denied or incomplete GitLab context as an error rather than an empty history. Local fixture success must not be described as live verification. The authorized live result above verifies Ollama Cloud and GitLab 19.4-pre only; it is not GitLab 18.9 evidence.

AI discussion acknowledgement is limited to authors confirmed through GitLab 18.9's `GET /users/:id` response. The reviewer retains only the resulting `human`, `bot`, or `unknown` classification in review context. A missing `bot` field, a denied lookup, or another lookup failure produces `unknown`; the public discussion remains available for ordinary review context but cannot support an AI discussion acknowledgement. These lookups use only `GITLAB_API_TOKEN` and are cached for the current job.

## Operational limits

Structural extraction initially understands Go declarations. Other eligible source is reviewed through explicit file fallback units. Review coverage remains probabilistic: the host makes scheduling, validation, persistence, and conservative verdict behavior deterministic, but it cannot guarantee deterministic AI findings.

The publisher retains history inside one note and refuses to replace it if the complete report exceeds `limits.max_report_bytes`. There is no archive service. GitLab offers no atomic conditional note read/delete operation, leaving a narrow final race after the last predecessor reread. Temporary duplicate bot-owned generations are recoverable.

Fingerprint reuse is deliberately conservative. Identical relevant inputs can reuse accepted work; changed code, public human context, guidance, tools, model settings, or trusted review rules trigger reassessment. There is no permanent dependency graph or aggressive function-level cache. Full-project reviews can exceed configured session or token budgets and then publish supported findings with partial coverage and `needs_review`.
