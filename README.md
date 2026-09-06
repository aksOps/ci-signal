# ci-signal

`ci-signal` is a Go merge-request reviewer for GitLab 18.9 Ultimate EE. It controls the official Copilot CLI through the official Copilot Go SDK and uses Ollama Cloud BYOK with `deepseek-v4-flash:cloud`. It investigates a pinned Git snapshot with Git, ast-grep, and bounded repository tools, accepts results only through `submit_review`, and maintains one current bot-owned Markdown report plus configured labels.

The reviewer does not use a Copilot subscription or GitLab's native approval API. Its `approved` result is an AI verdict recorded in the report. The host can reject a model-proposed approval when coverage or findings fail policy.

## Runtime contract

The supported provider tuple is fixed:

| Setting | Value |
| --- | --- |
| Provider | `ollama_cloud` |
| SDK provider type | `openai` |
| Wire API | `responses` |
| Endpoint | `https://ollama.com/v1` |
| Model | `deepseek-v4-flash:cloud` |
| Credential reference | `OLLAMA_API_KEY` by default |

Every Copilot session receives that provider and model explicitly. Stored-user authentication is disabled, the runtime uses a private Copilot home, and inherited Copilot, GitHub, GitLab, and Ollama credentials are removed from the Copilot child environment. A missing or incompatible BYOK configuration fails the run; it does not select a subscription model, another provider, or another model.

The supported build uses Go language level 1.25 with toolchain 1.26.5. Do not build or run checks with a newer Go version.

## Configuration

Set `REVIEWER_CONFIG_FILE` to an absolute path to a host-controlled JSON file. The loader rejects unknown fields, trailing JSON values, invalid enum values, conflicting labels, and unsupported provider settings. Relative paths inside the document resolve from the configuration file's directory, so the packaged [example configuration](examples/reviewer.json) works independently of the launch directory.

The example is safe by default because `diagnostics.dry_run` is `true`. Copy it to a protected CI file variable or another host-controlled location, adjust the GitLab project and label rules, and set `REVIEWER_DRY_RUN=false` only for a publishing job.

Required runtime secrets are references, never literal values in the JSON:

- `OLLAMA_API_KEY` authenticates Ollama Cloud.
- `GITLAB_API_TOKEN` is a personal, project, or group access token with API access to the target project. It is the sole credential for mutations.
- `CI_JOB_TOKEN` is optional and preferred for GitLab 18.9 read endpoints that support it.

The application settings use the `REVIEWER_` prefix. These environment variables override the corresponding JSON values:

| Variable | Purpose |
| --- | --- |
| `REVIEWER_GITLAB_BASE_URL` | GitLab origin, including a self-managed base path |
| `REVIEWER_GITLAB_PROJECT` | Numeric project ID or URL-escaped/project path accepted by GitLab |
| `REVIEWER_GITLAB_MR_IID` | Positive merge-request IID |
| `REVIEW_PROJECT_DIR` | Repository checkout; takes precedence over `CI_PROJECT_DIR` and JSON |
| `CI_PROJECT_DIR` | Repository fallback when `REVIEW_PROJECT_DIR` is absent |
| `REVIEWER_COPILOT_HOME` | Private writable Copilot home |
| `REVIEWER_STATE_DIR` | Private writable checkpoints and runtime state |
| `REVIEWER_BYOK_PROVIDER` | Must remain `ollama_cloud` |
| `REVIEWER_BYOK_ENDPOINT` | Must remain `https://ollama.com/v1` |
| `REVIEWER_BYOK_MODEL` | Must remain `deepseek-v4-flash:cloud` |
| `REVIEWER_BYOK_CREDENTIAL_ENV` | Name of the provider credential environment variable |
| `REVIEWER_REVIEW_SCOPE` | `mr_impact` or `full_project` |
| `REVIEWER_DRY_RUN` | Boolean mutation control |

Credential environment names for GitLab and Ollama must be distinct. External MCP configuration cannot reference any of them.

The CLI exposes:

```text
--dry-run
--log-file PATH
--log-level debug|info|warn|error
--log-reasoning
--log-tool-calls
--log-tool-results
```

Sensitive diagnostic content is off by default. Tool calls, tool results, and runtime-exposed reasoning require separate opt-in flags and pass through credential redaction. Review output lives in GitLab; stdout contains a compact JSON run result with `verdict`, `completion`, `used_ai`, and `published`.

## Review scope and budgets

`mr_impact` builds a worklist from changed declarations and fallback files, then permits repository-wide investigation for effects in callers, tests, configuration, and contracts. `full_project` places every eligible repository unit on the worklist. A global search does not turn an `mr_impact` review into full-project coverage.

Initial structural extraction supports Go with the pinned ast-grep rule. Configured structural scans can cover the whole eligible project in either review scope, but they do not mark every project unit as assessed. Deleted declarations use base content, additions use head content, and renames preserve both paths. Unsupported languages, generated or vendor exclusions, extraction failures, and oversized content receive explicit fallback or exclusion coverage. AST matches are candidates for investigation rather than proven semantic dependencies.

Large worklists are grouped into bounded sessions using `max_units_per_session`, `max_sessions`, `max_concurrency`, session and overall timeouts, byte limits, and input/output token budgets. Each batch can retrieve repository-wide evidence. Multi-batch work adds an integration unit. Compatible accepted checkpoints are reused only for an identical relevant fingerprint and assignment; they are local recovery data, not published history. Any unassessed, failed, stale, or budget-exhausted work produces partial coverage and cannot produce an approved verdict.

## Skills, instructions, and tools

Core guidance is loaded from configured directories rather than compiled into Go. The image ships:

- `core/skills/review`, which defines investigation, reassessment, coverage, and `submit_review` behavior;
- `core/skills/ast-analysis`, which defines structural investigation against the pinned snapshot;
- `core/instructions/reviewer.md`, which preserves host trust boundaries; and
- `core/prompts/review.md`, the configurable review prompt.

Adding another supported core instruction or skill file requires a configuration change, not an engine edit. Reserve required core skill names so a project cannot shadow them. The host verifies that every required review skill was actually invoked.

The repository snapshot discovers applicable `AGENTS.md`, `.github/copilot-instructions.md`, `.github/instructions/*.instructions.md`, `.github/skills`, and `.agents/skills` files. It materializes regular files from the pinned source head into a bounded private runtime directory. Symlinks, submodules, path escapes, and oversized inputs are rejected. Project guidance may describe repository conventions; it cannot replace endpoints, credentials, permissions, labels, verdict rules, or publication behavior.

Built-in read-only tools are `repository_read`, `repository_search`, `git_read`, `ast_grep`, and the host-configured `structural_scan`; `submit_review` is always required. They return bounded content and continuation state. A `tools.structural_scans` entry names a Go rule file and makes that rule available through `structural_scan`; the example wires the shipped declaration rule. Adding a native application tool is done in application composition and requires rebuilding the binary, without changing the engine. External MCP servers use SDK-native configuration from the trusted JSON file. Configure either a `stdio` command or an HTTPS URL, allow it explicitly in `permissions.external_mcp`, and pass only separately named non-protected environment references.

## Findings and acknowledgements

The review describes consequences and corrective actions in prose. It does not generate code or patches. Code blocks and patch formats are rejected; inline identifiers and source paths remain valid references. This format check cannot classify arbitrary code disguised as ordinary prose.

Test files and test coverage are outside review scope. Conventional test directories and Go, JavaScript, TypeScript, Java, Python, and C# test filenames are excluded from assignments and repository evidence tools. Excluded files are counted as `excluded_by_policy`, not reviewed production units. Unusual test layouts require a trusted `repository.excluded_paths` list of literal repository-relative files or directories, for example `["csharp/Program.cs"]` for a test harness alongside production code. Entries exclude that exact path and its descendants; globs and semantic test detection are not used. Other production files named `Program.cs` remain eligible.

The model submits structured assessments only through `submit_review`. Raw arguments are checked against JSON Schema and domain rules before decoding is trusted. Unknown fields, missing fields, foreign finding/unit/source IDs, invalid enums, checkbox claims by AI, unsupported evidence, incomplete retrieval, and inconsistent coverage are rejected. The model cannot supply persistent finding IDs, run IDs, timestamps, snapshot identity, labels, or publication generations.

The host policy defaults to `blocker`, `risk`, and `question` as gating categories. A present or unknown gating finding prevents approval even after acknowledgement. Info findings alone do not. Incomplete, failed, or stale work cannot approve. `review.exit_policy` controls the process status independently:

- `always_zero` always exits zero after a completed application run.
- `needs_review_nonzero` exits 1 for a `needs_review` verdict.
- `incomplete_nonzero` exits 1 unless submission completion is `complete`.
- Configuration, adapter, runtime, or coordinator errors exit 2.

The reviewer does not call GitLab's native approval API.

The report uses GitLab Flavored Markdown task lists. An open finding has an unchecked box. A human can check it to acknowledge the finding; the next run moves it to the acknowledged section without a Copilot call. Unchecking a human acknowledgement reopens it the same way. An AI acknowledgement appears with a green tick and no checkbox. Addressed findings omit the redundant assessment row. It requires validated human discussion or code-change evidence. Removing the icon or inserting a checkbox does not alter it. A later human reversal takes precedence over an older AI proposal, while the original assessment and history remain available.

Stable finding identity and versioned canonical state live in HTML comments, so they are hidden from rendered Markdown but visible in note source. Only eligible public GitLab evidence text and attribution can be retained there. Restricted, confidential, internal, system, and bot-owned report text is excluded before prompts, fingerprints, and state.

## GitLab reads, publication, and labels

On GitLab 18.9, merge-request metadata and note reads prefer `CI_JOB_TOKEN`. A 401 or 403, and a validated authorization-masking 404, receives one retry with `GITLAB_API_TOKEN`. Known unsupported reads such as discussions and labels use the API token directly. Rate limits, server errors, TLS failures, timeouts, and cancellation do not switch credentials. Diagnostics name `job_token` or `api_token` and the fallback reason without logging token values. All writes use only `GITLAB_API_TOKEN`.

The publisher maintains one current bot-owned summary note. It reads prior source Markdown and human discussion, reconciles controls and accepted findings, creates a marked successor, reads it back, applies late control changes, and only then deletes a verified predecessor. It does not delete developer notes, human replies, or an only valid report. Unknown state versions, malformed or duplicate markers, ownership or binding conflicts, missing durable provenance, human replies, or report-size overflow preserve the prior report and stop cleanup. An interrupted replacement may temporarily leave duplicate owned generations, which the next run can recover.

GitLab's note API does not provide an atomic conditional read/delete operation. A human edit can race the final predecessor reread and delete. The publisher narrows that window and preserves detected changes; it cannot promise that every intermediate checkbox click is observed.

Labels derive only from trusted configuration, accepted state, and observed runtime telemetry. Static labels and dynamic sources cover verdict, coverage, requested models, observed models, executed tools, token usage, team, and configured metadata. Requested and runtime-observed model identities stay separate. Installed tools are not treated as executed. Token events are deduplicated across retries and cumulative snapshots are not added to component events; incomplete telemetry is `unknown`, not zero.

Single-value mappings emit labels with one exclusive `::` scope. Set mappings use a non-scoped prefix. Token usage supports exact labels through a scoped prefix or ordered bounded buckets with a final unbounded bucket. Missing values use the enum `omit` or `unknown`. `create_missing` may create configured project labels; otherwise existing project or group labels are reused. Synchronization applies targeted add/remove differences only and preserves unrelated MR labels and label definitions.

## Local checks

Local source runs require:

- Go 1.26.5, as selected by [go.mod](go.mod);
- [Git](https://git-scm.com/downloads) available through `tools.git_path`;
- [ast-grep 0.45.3](https://github.com/ast-grep/ast-grep/releases/tag/0.45.3) available through `tools.ast_grep_path`; the example uses `ast-grep` from `PATH`, or the JSON field can name an absolute pinned binary path; and
- the complete [Copilot CLI 1.0.83 release package](https://github.com/github/copilot-cli/releases/tag/v1.0.83), with `COPILOT_CLI_PATH` pointing to its platform runtime. Preserve the extracted directory layout: `runtime.node` must remain beside `prebuilds/<platform>/copilot-runtime`, with the package's other support files in their original locations. Copying the runtime executable alone is insufficient.

Build a persistent local binary from the source checkout with the exact toolchain. The subshell makes the command independent of the caller's current directory:

```bash
mkdir -p /chosen/path
(
  cd /absolute/path/to/ci-signal
  GOTOOLCHAIN=go1.26.5 go build -buildvcs=false -trimpath -o /chosen/path/ci-signal ./cmd/ci-signal
)
```

After copying and editing `examples/reviewer.json`, a dry run can be launched from any directory. Dry run prevents GitLab mutations; it still reads the target MR and can spend Ollama Cloud tokens.

```bash
export OLLAMA_API_KEY
export GITLAB_API_TOKEN
export CI_JOB_TOKEN
export COPILOT_CLI_PATH=/absolute/path/to/github-copilot-1.0.83/prebuilds/linux-x64/copilot-runtime

REVIEWER_CONFIG_FILE=/absolute/path/to/reviewer.json \
REVIEW_PROJECT_DIR=/absolute/path/to/target-checkout \
/chosen/path/ci-signal --dry-run
```

Run the focused fixture set with the exact toolchain:

```bash
./scripts/check-local.sh
```

If the pinned Copilot CLI 1.0.83 executable is available, add the local protocol fixture:

```bash
COPILOT_CLI_PATH=/absolute/path/to/github-copilot-1.0.83/prebuilds/linux-x64/copilot-runtime ./scripts/check-local.sh
```

That fixture sends explicit BYOK requests to a local Responses API stub. It does not contact Ollama Cloud or GitLab. See [compatibility and verification](docs/compatibility.md) for pins, evidence, and remaining live checks. A renderer-produced report sample is checked at [internal/markdown/testdata/sample.md](internal/markdown/testdata/sample.md).

Build and tag the local container from any directory by giving Docker the absolute source path:

```bash
docker build --tag ci-signal:test /absolute/path/to/ci-signal
```

The packaged example resolves its relative guidance paths inside the image. Override its placeholder MR values and mount the target repository at the configured project path:

```bash
export OLLAMA_API_KEY
export GITLAB_API_TOKEN
export CI_JOB_TOKEN

docker run --rm \
  --volume /absolute/path/to/target-checkout:/workspace:ro \
  --env OLLAMA_API_KEY \
  --env GITLAB_API_TOKEN \
  --env CI_JOB_TOKEN \
  --env REVIEWER_CONFIG_FILE=/opt/ci-signal/examples/reviewer.json \
  --env REVIEW_PROJECT_DIR=/workspace \
  --env REVIEWER_GITLAB_BASE_URL=https://gitlab.example.com \
  --env REVIEWER_GITLAB_PROJECT=group/project \
  --env REVIEWER_GITLAB_MR_IID=1 \
  ci-signal:test --dry-run
```

## GitHub Container Registry

The [publish workflow](.github/workflows/publish-image.yml) runs the focused local checks, builds and checks the existing Docker image, then publishes it to `ghcr.io/<lowercase-owner>/<lowercase-repository>`. A push to `main` publishes both `sha-<commit>` and `latest`; a manual run publishes only the commit tag unless it runs from `main`. The workflow summary records the immutable digest reference to use as `AI_REVIEWER_IMAGE` in GitLab CI. For the expected repository name, the image is `ghcr.io/aksops/ci-signal`.

The workflow uses the repository's automatic `GITHUB_TOKEN` with `contents: read` and `packages: write`. It does not receive GitLab or Ollama secrets and does not run for pull requests. GitHub creates a newly published container package as private. After the first successful run, set the package visibility to public in its package settings if anonymous GitLab runners must pull it.

## GitLab CI

The image contains `/usr/local/bin/ci-signal`, pinned Git and ast-grep tooling, the compatible Copilot CLI, and read-only core guidance under `/opt/ci-signal/core`. Its private writable directories are `/var/lib/ci-signal/copilot` and `/var/lib/ci-signal/state`.

Use the provided [.gitlab-ci.yml.example](.gitlab-ci.yml.example) as a starting point. Store the reviewer JSON as a protected file variable and set `REVIEWER_CONFIG_FILE` to its absolute temporary path. Because relative paths resolve from that temporary file rather than from `/opt/ci-signal/examples`, a CI file-variable configuration must replace every shipped core path with these image paths:

```json
{
  "guidance": {
    "core_skill_dirs": ["/opt/ci-signal/core/skills"],
    "core_instruction_dirs": ["/opt/ci-signal/core/instructions"],
    "review_prompt_file": "/opt/ci-signal/core/prompts/review.md"
  },
  "tools": {
    "structural_scans": [
      {
        "name": "go-declarations",
        "language": "go",
        "rule_path": "/opt/ci-signal/core/skills/ast-analysis/rules/go-declarations.yml"
      }
    ]
  }
}
```

These are field replacements inside the complete strict configuration, not a standalone configuration document. Supply `OLLAMA_API_KEY` and `GITLAB_API_TOKEN` only to trusted merge-request jobs, and serialize publishers per MR with `resource_group`. Do not expose trusted credentials to untrusted forks or execute repository hooks/scripts with them. Use `REVIEWER_DRY_RUN=true` first; set it to `false` only in the authorized publishing job.

No webhook service is included. Title, description, discussion, or checkbox-only changes are processed by the next pipeline or manual job unless the project already has an approved trigger.

## License

The `ci-signal` source is available under the [MIT License](LICENSE), and its Go library dependencies use open-source licenses. The container also bundles GitHub Copilot CLI 1.0.83 under the [Copilot CLI license](https://github.com/github/copilot-cli/blob/v1.0.83/LICENSE.md); that bundled executable is not covered by this project's MIT License.
