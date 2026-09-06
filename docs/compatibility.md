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

The selected Go dependencies use compatible OSI licenses: Copilot Go SDK and Goldmark are MIT; GitLab API client-go and `jsonschema` are Apache-2.0. The project pins versions in [go.mod](../go.mod) and [go.sum](../go.sum).

## Verified with local fixtures

The focused test suite verifies these application guarantees without live credentials:

- strict configuration, secret separation, provider lock, label-shape validation, absolute config selection, and launch-directory-independent relative paths;
- explicit BYOK session configuration, disabled subscription authentication, skill invocation, native tool calls, correction bounds, telemetry integrity, cancellation, and credential redaction through a fake runtime;
- a real Copilot CLI 1.0.83 protocol exchange through a local OpenAI Responses stub when `COPILOT_CLI_PATH` is supplied;
- pinned Git snapshots, additions, deletions, renames, cross-file search, bounded continuation, guidance materialization, Go declaration extraction, configured whole-project structural scans, and fallback coverage in temporary repositories;
- strict `submit_review` validation, accepted-argument persistence, conservative verdict policy, explicit reassessment, and durable source provenance;
- Markdown round trips, renderer escaping, checkbox check/uncheck, late human reversal, inert AI-control tampering, and the renderer-produced sample;
- GitLab credential routing, single fallback, API-only writes, redirect restrictions, pagination failure, targeted label updates, retry recovery, successor verification, note-history preservation, late controls, ownership and binding checks, restricted-source rejection, and report overflow;
- API-token-only author classification through GitLab's Users API, including cached human/bot results and fail-closed denied or malformed responses;
- coordinator fingerprint reuse, changed-context reassessment, bounded batching, cross-file findings, checkpoint recovery, checkbox-only synchronization without AI, partial-review policy, and stale-publication handling.

The local CLI protocol fixture is stronger than a mocked SDK test, but it still uses a local Responses server. It is not evidence that Ollama Cloud accepted the model, that provider usage events are exposed in production, or that a GitLab 18.9 instance accepted the final requests.

## Unverified live compatibility

No live Ollama Cloud request or GitLab mutation is performed by the repository checks. Until an operator explicitly authorizes a target, credentials, and bounded provider spend, these remain unverified:

- availability and tool-calling behavior of `deepseek-v4-flash:cloud` through Ollama Cloud at run time;
- Ollama Cloud usage/model telemetry exposed through Copilot CLI 1.0.83;
- authentication and endpoint behavior on the operator's GitLab 18.9 Ultimate EE instance, including installation-specific job-token permissions;
- access to `GET /users/:id` for public-note author classification with the supplied API token;
- creation, verification, late-control reconciliation, deletion, and label synchronization against a real merge request;
- runner, network, certificate, proxy, and container-platform compatibility in the target GitLab installation.

The reviewer reports absent provider telemetry as unknown. It reports denied or incomplete GitLab context as an error rather than an empty history. Fixture success must not be described as live GitLab 18.9 or live Ollama Cloud verification.

AI discussion acknowledgement is limited to authors confirmed through GitLab 18.9's `GET /users/:id` response. The reviewer retains only the resulting `human`, `bot`, or `unknown` classification in review context. A missing `bot` field, a denied lookup, or another lookup failure produces `unknown`; the public discussion remains available for ordinary review context but cannot support an AI discussion acknowledgement. These lookups use only `GITLAB_API_TOKEN` and are cached for the current job.

## Operational limits

Structural extraction initially understands Go declarations. Other eligible source is reviewed through explicit file fallback units. Review coverage remains probabilistic: the host makes scheduling, validation, persistence, and conservative verdict behavior deterministic, but it cannot guarantee deterministic AI findings.

The publisher retains history inside one note and refuses to replace it if the complete report exceeds `limits.max_report_bytes`. There is no archive service. GitLab offers no atomic conditional note read/delete operation, leaving a narrow final race after the last predecessor reread. Temporary duplicate bot-owned generations are recoverable.

Fingerprint reuse is deliberately conservative. Identical relevant inputs can reuse accepted work; changed code, public human context, guidance, tools, model settings, or trusted review rules trigger reassessment. There is no permanent dependency graph or aggressive function-level cache. Full-project reviews can exceed configured session or token budgets and then publish supported findings with partial coverage and `needs_review`.
