# Provider content boundary

## CTR-REMOTE-002 — Provider content boundary

The existing skills.sh and SkillsMP registry owners also own conversion of
their catalog records into complete remote file sets:

- skills.sh content comes from a `git clone --depth 1` of the catalog source
  repository. Extra ID path segments are searched as a source subdirectory;
  otherwise the named skill is located from that repository the same way
  `npx skills add <source> --skill <name>` is.
- SkillsMP content comes from the GitHub location supplied in its catalog
  record and is retrieved with `git clone --depth 1`.

Provider results are untrusted data. Before they reach the store, the manager
must reject absolute or escaping paths, non-regular entries, duplicate paths,
oversized responses, and any tree whose root `SKILL.md` does not parse to the
catalog skill's name. Provider and store operations must not execute downloaded
content.

This contract supports `REQ-SYNC-002` and `REQ-SYNC-003`.
