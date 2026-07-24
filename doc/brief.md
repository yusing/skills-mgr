# Remote skill persistence and catalog toggling

## Problem

The TUI can browse skills.sh and SkillsMP, but catalog entries cannot be enabled.
The existing toggle applies only to skills already discovered from agent-managed
skill directories, so a remote result has no durable content for `list` or `get`
to consume.

## Outcome

A user can press Space on a skill in either remote catalog to fetch and persist
one canonical user-level copy and enable it for the current project. Persisted
content is shared across projects, while each project's existing
`.skills-mgr.json` independently records whether the skill is enabled.

## First-draft scope

- Toggle skills from the skills.sh topic catalog, skills.sh search results,
  the SkillsMP catalog, and SkillsMP search results.
- Keep fetched skill content for three hours. Enabling uses a fresh persisted
  copy without network access and refreshes a missing or stale copy before
  changing project selection.
- Disabling changes only current-project selection. It performs no network
  request and retains persisted content for later offline re-enablement.
- Extend the existing daemon to refresh stale persisted copies in the
  background while continuing to retain last-known-good content after a failed
  refresh.
- Make an enabled remote skill available through the existing installed-skill,
  `list`, and `get` behavior.

## User-visible surface

The existing Space key toggles the selected skill on all three TUI tabs. Remote
rows show whether their skill is enabled for the current project, and status
text reports fetching, enabling, disabling, and failures. SkillsMP installs show
a progress popup while cloning. No command, option, configuration key, or
environment variable is added.

## Constraints

- Remote persistence is owned by skills-mgr at user scope. The feature must not
  create, copy, link, or modify content under project or user
  `.agents/skills` or `.codex/skills` directories.
- SkillsMP content is fetched with `git clone --depth 1`; other fetching,
  persistence, toggling, and daemon refresh operations remain in process.
  Downloaded content must not be executed.
- A failed or invalid download must not enable the skill, replace a valid
  persisted copy, or partially update project selection.
- Existing local-skill discovery, the separately requested `skills-mgr run`
  operation, and `$EDITOR` integration remain supported and unchanged.

## Non-goals

- A remove, update, force-refresh, or cache-management command.
- Copying or linking remote skills into agent-owned discovery roots.
- Automatically enabling a persisted skill in another project.
- Executing, auditing, sandboxing, or otherwise endorsing downloaded scripts.
- Changing catalog ranking, search, authentication, or the existing local
  toggle contract.
