---
pjdoc:
  version: 1
  kind: spec
  scope: root
  status: draft
  revision: SPEC-1
  files:
    []
---
# skills-mgr product specification

This draft specifies the first increment described by
[`doc/brief.md`](../brief.md).

## REQ-REMOTE-001 — Persist a remote skill on enable

When Space is pressed on a disabled skill in a skills.sh or SkillsMP catalog
view, skills-mgr shall obtain the complete skill content, validate it as one
safe relative file tree containing a valid `SKILL.md`, persist one canonical
user-level copy, and enable that skill in the current project's existing
selection.

The behavior shall apply to topic/default catalog rows and search-result rows.
It shall not write to a project or user `.agents/skills` or `.codex/skills`
directory and shall not invoke or execute any external process or downloaded
file.

Acceptance examples:

- Enabling an uncached skills.sh topic skill persists its files outside the
  agent-owned roots, records it as enabled only in the current project, and
  makes it visible in the Installed tab and existing `list` and `get`
  operations.
- Enabling an uncached SkillsMP search result has the same observable result.
- Unsafe paths, duplicate file paths, a missing or invalid `SKILL.md`, a
  provider failure, or a name collision with a different discovered or
  persisted skill returns an error without changing the project selection or
  replacing an existing valid copy.

## REQ-REMOTE-002 — Reuse and refresh persisted content

Persisted remote content shall be fresh for three hours after its last
successful fetch.

When enabling a disabled remote skill, skills-mgr shall use a fresh persisted
copy without a network request. If the copy is missing or stale, it shall
refresh and atomically persist the complete valid replacement before enabling
the skill. A refresh failure shall leave both the last-known-good copy and
current project selection unchanged.

Acceptance examples:

- A persisted copy less than three hours old can be enabled while offline.
- Enabling a copy at least three hours old performs one provider refresh before
  changing selection.
- If that refresh fails, stale content remains intact but the skill remains
  disabled in the current project.

## REQ-REMOTE-003 — Disable without removing persisted content

When Space is pressed on an enabled remote skill, skills-mgr shall disable it
only for the current project, perform no provider request, and retain its
persisted content.

Acceptance examples:

- Disabling a remote skill removes it from existing `list` and `get` output for
  the current project without changing another project's selection.
- Re-enabling the retained copy within its freshness period succeeds without a
  network request.

## REQ-REMOTE-004 — Refresh persisted skills in the daemon

The existing daemon shall inspect persisted remote skills in the background and
refresh every copy that is at least three hours old. It shall use the same
validation and atomic replacement rules as interactive enablement.

The daemon shall never execute fetched content or launch an external process.
A refresh failure shall retain the last-known-good copy, report the failure
through the daemon's existing diagnostic output, and allow later background
refresh attempts.

Acceptance examples:

- A daemon cycle does not fetch a fresh persisted skill.
- A daemon cycle refreshes a stale skills.sh copy and a stale SkillsMP copy.
- A failed daemon refresh leaves the stale copy readable and reports which
  persisted skill could not be refreshed.

## REQ-REMOTE-005 — Present remote toggle state

Each remote catalog row shall indicate whether that remote skill is enabled in
the current project. Space shall start at most one toggle operation for the
selected remote row, prevent overlapping input while it is active, and report
fetching, enabled, disabled, or error status through the existing TUI status
line.

After a successful remote toggle, the remote row state, Installed tab, and
current-project selection shall agree without restarting the TUI. A failed
toggle shall preserve their prior state.
