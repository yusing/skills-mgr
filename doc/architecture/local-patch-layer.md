# Local patch layer

## CTR-REMOTE-005 — Local patch layer

The remote store owns one stable `.patch` sidecar per remote identity under
`$HOME/.skills-mgr/skills/.remote-patches/`. This global manager-home location
is outside the refetchable remote cache, so a tracked patch can be restored
before its provider content is installed. The patch records the fetched
`SKILL.md` digest it was based on and the expected patched-result digest, so
changed provider instructions and structurally damaged patches cannot silently
produce another result. After those integrity headers, it stores a conventional
unified diff whose file headers name `a/SKILL.md` and `b/SKILL.md`; the store
strictly applies its hunks to the digest-matched provider bytes. The TUI edits a
private temporary copy of the currently layered `SKILL.md` and atomically
replaces the sidecar only after the editor succeeds. The store compares that
starting layer again under its exclusive lock before publishing, so concurrent
editors cannot overwrite an intervening edit. It never writes into provider
content.

The `get` owner applies the sidecar only to a remote skill's `SKILL.md` before
frontmatter stripping and line-range selection. Patch parsing or application
is all-or-nothing: failure discards any partial result, writes the original
provider content through the normal output path, and returns an error for the
CLI to report on stderr with nonzero status. Discovery, placeholders, and
script execution continue to use validated provider content. Refresh preserves
the sidecar, while uninstall removes it with the remote entry.

This contract supports `REQ-LAYER-001`.
