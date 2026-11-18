# Discovery and TUI integration

## CTR-REMOTE-004 — Discovery and TUI integration

The Rust discovery owner reads the canonical remote store after
the existing agent and plugin roots, preserving their current precedence. A
persisted remote entry participates in the current name-based project
selection only when no higher-precedence discovered skill already owns that
name.

Single-name access uses the same ordering and ownership rules, stopping at the
first filesystem owner rather than requiring later catalogs to load. Selection
and skill content are read live on each invocation; no persisted discovery or
condition-result cache changes when edits become visible. Remote access applies
the local patch when serving `SKILL.md`, while full catalog discovery uses it
for the displayed description.

The Rust executable is the public CLI. Interactive launches replace it with
the adjacent Go companion, which owns terminal rendering, input, and editor
processes. It requests its startup catalog and selection together from the
same Rust executable. Each later core operation uses one subprocess with
private JSON over stdin/stdout. Moving through loaded rows and filtering local
skills don't start a subprocess. The companion may read the two registry
display caches but doesn't fetch or write durable skill state itself.

Catalog presentation carries provider identity and refresh locator through the
asynchronous TUI message path. Remote rows derive enabled state from the
current project selection; a completed toggle reloads discovery and selection
together before rendering success.

This contract supports `REQ-SYNC-002`.
