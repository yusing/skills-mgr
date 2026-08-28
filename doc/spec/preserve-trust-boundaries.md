# Preserve trust boundaries and report failures

## REQ-SYNC-003 — Preserve trust boundaries and report failures

Synchronization shall use the existing remote provider, content validation,
resource limits, safe-path checks, and atomic persistence behavior. It shall
never execute downloaded content or write it into agent-owned skill roots.

For every successfully synchronized remote skill, the command shall write its
name to standard output. If any selected identity is invalid, unavailable,
conflicts with another persisted skill, or produces invalid content, the
command shall return an error identifying that skill. Previously valid content
and both selection files shall remain unchanged.

Acceptance examples:

- Unsafe paths, excessive content, invalid `SKILL.md`, or provider failure
  produces a non-zero result without partial project-selection updates.
- If an earlier skill succeeded before a later skill failed, its valid
  user-level persisted copy may remain; the configuration is unchanged.
- Successful output contains one line per synchronized skill and no downloaded
  file is executed.
