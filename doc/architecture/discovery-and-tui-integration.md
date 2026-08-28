# Discovery and TUI integration

## CTR-REMOTE-004 — Discovery and TUI integration

The existing manager discovery owner reads the canonical remote store after
the existing agent and plugin roots, preserving their current precedence. A
persisted remote entry participates in the current name-based project
selection only when no higher-precedence discovered skill already owns that
name.

Catalog presentation carries provider identity and refresh locator through the
existing asynchronous TUI message path. Remote rows derive enabled state from
the current project selection; a completed toggle reloads discovery and
selection together before rendering success.

This contract supports `REQ-SYNC-002`.
