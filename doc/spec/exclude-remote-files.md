# Exclude files from a remote skill

## REQ-LAYER-002 — Exclude files from a remote skill

`skills-mgr install` shall accept repeatable `--exclude <pattern>` options.
Each pattern uses Go `path.Match` syntax against a file's path relative to the
skill directory and excludes a file when it matches that path or any of its
parent directories. A malformed pattern shall fail the command before any
content is fetched.

Excluded files shall be omitted before the file-count and size limits are
evaluated, so a skill that exceeds a limit only because of excluded files can
be installed. The patterns shall be stored with the remote identity under the
global manager home, beside its local patch, and every later refresh and
`sync` of that identity shall apply them. Changing them shall make the
persisted copy stale so its next ensure refetches it. Each `install` shall
replace the stored patterns; installing without `--exclude` shall remove them.
A failed install shall leave the previous patterns in place, and uninstalling
the remote skill shall remove them.

Acceptance examples:

- Installing a skill with `--exclude 'examples/*.html'` stores no matching file,
  and a later refresh keeps omitting them.
- `--exclude examples` omits every file below `examples/`.
- A skill over the size limit installs when its excluded files bring it under
  the limit.
- Reinstalling an installed skill with different patterns refetches it with
  the new patterns even while the persisted copy is fresh.
- A malformed pattern such as `[` fails without changing selection or store.
