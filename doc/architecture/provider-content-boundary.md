# Provider content boundary

## CTR-REMOTE-002 — Provider content boundary

Content providers convert registry records or supplied repository addresses
into validated remote file sets:

- skills.sh content comes from a `git clone --depth 1` of the catalog source
  repository. Extra ID path segments are searched as a source subdirectory;
  otherwise the named skill is located from that repository the same way
  `npx skills add <source> --skill <name>` is.
- SkillsMP content comes from the GitHub location supplied in its catalog
  record and is retrieved with `git clone --depth 1`.
- Direct installations use provider `repository`. Their locator is the
  normalized HTTPS repository or GitHub tree URL, and their ID is that locator
  followed by `#` and the declared skill name. Refetch resolves that name within
  the locator's scope without consulting a registry. A repository-root skill
  includes its manifest and standard resource directories, not unrelated files.

Provider results are untrusted data. Before they reach the store, the manager
must reject absolute or escaping paths, non-regular entries, duplicate paths,
oversized responses, and any tree whose root `SKILL.md` does not parse to the
recorded skill's name. Provider and store operations must not execute downloaded
content.

This contract supports `REQ-SYNC-002` and `REQ-SYNC-003`.
