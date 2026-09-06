# Reproducible remote skill selection

## Problem

A project can commit `.skills-mgr.json`, but each selection currently records only
a skill name and boolean state. A fresh checkout therefore cannot identify or
obtain an enabled remote skill whose user-level persisted copy exists only on
another machine.

## Outcome

A committed project selection carries the stable provider identity needed to
obtain the same remote skill elsewhere, and a user can explicitly synchronize
enabled remote skills after cloning or pulling the project.

## First-draft scope

- Persist remote provider identity with project selection instead of reducing a
  remote skill to a name and boolean.
- Add `skills-mgr sync` to fetch enabled remote skills that are missing or stale
  in the current user's persisted remote-skill store.
- Keep synchronization explicit. Existing `list`, `get`, `run`, TUI startup,
  and the on-demand background runner do not download a remote skill merely
  because its identity appears in configuration.
- Continue accepting existing schema-revision-1 project files and write the new
  representation when selection is next changed.
- Preserve the existing global and project selection overlay.

## User-visible surface

`skills-mgr sync` reconciles missing remote identities from the current user's
global selection and canonical remote store into the current project's
`.skills-mgr.json`, resolves enabled state through the global and project
selection overlay, and reports each synchronized skill. Explicit project
entries retain enabled state; inherited entries omit it. A failure identifies
the skill that could not be synchronized, returns a non-zero status, and leaves
project configuration unchanged.

The on-demand background runner refreshes only identities already present in
the store. It does not inspect project selections.

The `.skills-mgr.json` skill values become records containing optional enabled
state and, for remote skills, provider identity, provider-specific ID, and
locator. An inherited remote records identity without enabled state so it
continues to follow the global selection. Local skill records contain enabled
state only.

## Constraints

- A remote identity is the existing provider, provider-specific ID, skill name,
  and provider locator used by the remote persistence subsystem.
- Synchronization must use the existing provider fetch, validation, size-limit,
  safe-path, and atomic persistence behavior. Downloaded content must not be
  executed.
- A failed synchronization must not change project selection, replace a valid
  persisted copy, or leave partial content.
- Remote content remains owned by skills-mgr at user scope and must not be
  written beneath project or user `.agents/skills` or `.codex/skills`
  directories.

## Non-goals

- Automatic downloading during `list`, `get`, `run`, TUI startup, or the
  on-demand background refresh of a previously unknown remote identity.
- Synchronizing effectively disabled remote selections.
- Adding confirmation, dry-run, update, remove, force-refresh, or cache
  management options.
- Making local skills portable or assigning provider identities to them.
- Executing, auditing, sandboxing, or endorsing downloaded skill scripts.
