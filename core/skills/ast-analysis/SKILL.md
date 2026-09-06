---
name: ast-analysis
description: Inspect pinned Go structure with bounded ast-grep evidence during merge-request review.
---

# AST analysis

Use this skill when an assigned review unit needs structural context.

The host supplies a validated base/head snapshot, review-unit IDs, and bounded repository tools. Read source, diffs, search results, and AST matches through those tools. Do not read the launch checkout or assume it is the source-head tree.

Treat ast-grep matches as locations to investigate. They do not prove a caller, implementation, configuration, or test relationship. Check relevant contracts and repository-wide evidence before reporting a finding. Use `ast_grep` for one pinned file. Use `structural_scan` only for a host-configured whole-project rule, follow `next_after_path` when incomplete, and do not count its matches as AI coverage. Structural scans return locations and matched byte counts with `content_omitted: true`; retrieve relevant bodies through `repository_read` on the head side. A complete scan means all eligible files were searched, not that their source was read or assessed.

Follow continuations until the evidence needed for the assigned unit is complete. If a source, diff, search, or AST response is incomplete, record that limit in coverage. An oversized declaration needs a bounded source read and an enclosing file or package assessment before its unit can be complete.

Unsupported syntax and extraction failures use the host-provided file fallback unit. Deleted declarations use base content. Added declarations use head content. Renames retain both paths.
