# skills-mgr

One skill selection for Codex, Claude Code, and Grok, narrowed to the project
you are actually in.

`skills-mgr` finds every `SKILL.md` your machine already has, records which of
them apply to the current directory, and serves their instructions and scripts
to an agent on demand. A selection entry is a plain on/off switch or a condition
such as `lang go && tooling shadowtree`, so a Go project and a React project see
different skills without you touching anything. Its terminal interface also
browses and installs skills from [skills.sh](https://skills.sh) and
[SkillsMP](https://skillsmp.com).

TL;DR: run `skills-mgr` to choose skills and `skills-mgr -g` to choose them for
every project. A session-start hook feeds `skills-mgr list` to your agent, which
reads what it picks with `get` and `run`.

## Quick Start

Requirements:

- Go 1.26 or newer
- Git, when installing a SkillsMP skill

Install the command:

```sh
go install github.com/yusing/skills-mgr@latest
```

From the project whose selection you want to manage, open the interface:

```sh
skills-mgr
```

Move with `j`/`k` or the arrow keys and press Space to enable a skill. This
writes `.skills-mgr.json` in the current directory. Then see what an agent
would see:

```sh
skills-mgr list
```

```xml
<skills>
  <skill name="codebase-design" description="Shared vocabulary for designing deep modules.">
    <references>DEEPENING.md
DESIGN-IT-TWICE.md</references>
  </skill>
  <skill name="writing-for-agents" description="Writing documents for agents.">
    <references>SKILL-MECHANICS.md</references>
  </skill>
</skills>
```

Read one, or just part of one of its references:

```sh
skills-mgr get codebase-design
skills-mgr get codebase-design/DEEPENING.md 1:40
```

To make this automatic in a session, see [Agent
Integration](#agent-integration).

## Why skills-mgr?

Each agent harness discovers skills from its own directory. A skill you install
for Claude Code is invisible to Codex, and the same skill installed three times
is three copies to keep current.

A discovered skill is also an active skill. Every harness loads the name and
description of everything in its skill directory into every session, whatever
the project is. Fifty skills means fifty descriptions in the prompt before the
first message, most of them irrelevant to the repository you opened.

`skills-mgr` separates the two questions those directories conflate:

- **Is this skill available?** Answered once, per machine, by discovery.
- **Does this skill apply right now?** Answered per project, by a selection
  entry that can be a condition rather than a constant.

In a Go repository, a fifty-skill collection typically resolves to a handful,
and the agent reads the body of one only when it decides to use it. The selection
governs content in the manager's stores, so skills you write belong in the
[manager home](#the-manager-home).

## How It Works

Three stages, in order. Every command is one of them.

**1. Catalog.** Discovery walks a fixed list of roots and collects every
directory holding a valid `SKILL.md`. Harness-owned roots, the Codex plugin
cache, the manager home, and the remote store all feed one namespace. First
root to declare a name wins. See [Skill Discovery](#skill-discovery).

**2. Selection.** Two files decide what applies:
`$HOME/.skills-mgr/.skills-mgr.json` for every project, and `./.skills-mgr.json`
for this one. Each entry is `true`, `false`, or a Bash expression run from the
project directory. See [The Selection File](#the-selection-file).

**3. Access.** `list` advertises the enabled set as XML, `get` prints a skill
file, and `run` executes a skill script. `get` and `run` refuse a skill that is
not effectively enabled here. See [Command Reference](#command-reference).

Content and selection stay separate throughout. Disabling a skill never deletes
it, and enabling one never copies it into an agent's directory; it writes only a
frontmatter stub the model cannot see. See
[Where Skill Content Lives](#where-skill-content-lives).

## How It Differs From the Vercel `skills` CLI

The two tools solve adjacent problems and compose. Vercel's `npx skills`
publishes and installs skills; `skills-mgr` decides which of your installed
skills apply and serves them. `skills-mgr` installs from skills.sh, Vercel's
registry.

| | `npx skills` | `skills-mgr` |
| --- | --- | --- |
| Primary job | Install and update skills from GitHub repositories | Select among discovered skills and serve them on demand |
| Where content lives | Copied or symlinked into each agent's own `skills/` directory | Two user-level stores; harness directories get frontmatter-only placeholders the model cannot see |
| Turning a skill off | `npx skills remove` deletes it | Flip one selection entry; the content stays installed |
| Per-project relevance | Installed means active | `true`, `false`, or a Bash condition evaluated in the project directory |
| What the agent sees up front | Every installed skill's description, every session | From `skills-mgr`, only manager-owned skills enabled here |
| Harness coverage | 76 agents | Codex, Claude Code, Grok |

## Managing Skills

### The Interactive Interface

Six tabs, selected with `←` and `→`:

| Tab | Contents | Network |
| --- | --- | --- |
| Installed | Shared `.agents/skills` roots, the manager home, and installed remote skills | None |
| Codex | Codex-native skills. `[` and `]` cycle the User, Plugin, Builtin, and System sources | None |
| Grok | Grok-native skills. `[` and `]` cycle the User, Plugin, and Bundled sources | None |
| Claude | Claude-native skills. `[` and `]` cycle the User and Plugin sources | None |
| skills.sh | Browse topics, search, and install | skills.sh, then `git clone --depth 1` of the skill's source repository |
| SkillsMP | Browse, search, and install | SkillsMP, then `git clone --depth 1` of the skill's repository |

| Key | Action |
| --- | --- |
| `←` / `→` | Change tab |
| `[` / `]` | Cycle source subtabs in the Codex, Grok, or Claude tab |
| `j` / `k`, `↓` / `↑` | Move through results |
| `f` | Filter local skills, or search the active registry |
| Enter or click | Expand or collapse details |
| Space | Enable or disable the selected skill |
| `i` | Edit this skill's current-layer `enabled` value in `$EDITOR` |
| `e` | Open an editable skill's `SKILL.md` in `$EDITOR` |
| `m` | Toggle `disable-model-invocation` in the skill's own `SKILL.md` |
| `a` | Adopt a `$HOME/.agents/skills` skill into the manager home, or release it back |
| `u` | Uninstall the selected installed remote skill |
| `q` or Ctrl-C | Quit |

The `i` draft accepts `true`, `false`, a bare Bash expression, or a JSON string;
saving it empty removes the current-layer value. `m` rewrites the skill's source
file, preserving the rest of its frontmatter. `a` moves content rather than
copying it; see [The Manager Home](#the-manager-home).

For an installed remote skill, `e` opens the provider content with any existing
local edit applied. Saving stores the edit under
`$HOME/.skills-mgr/skills/.remote-patches/`; the fetched provider files remain
unchanged. Each patch keeps two integrity hashes followed by a readable unified
diff for `SKILL.md`.

`u` deletes stored remote content, so it is refused for a skill configured in
the global layer while you are in project mode; uninstall that one from
`skills-mgr -g`.

### Global and Project Layers

Run the interface with `-g` to manage the selection that applies everywhere:

```sh
skills-mgr -g
```

This writes `$HOME/.skills-mgr/.skills-mgr.json`. A file left at the older
`$HOME/.skills-mgr.json` moves there on the next run, unless one already exists.
In project mode, an entry in `./.skills-mgr.json` overrides the global entry
with the same name, and deleting the project entry restores inheritance. The
`-g` flag must be the only argument; the other commands always act on the
current directory.

### Skills That Are Enabled Without an Entry

Two kinds of skill are enabled even with nothing recorded for them:

- A skill under the project's own `./.agents/skills`, on the assumption that a
  skill committed to a repository belongs to it.
- A skill whose frontmatter sets `disable-model-invocation: true`. These are
  omitted from `list` so the agent does not consider them on its own, but
  `get` and `run` still work when you name one.

## Agent Integration

`skills-mgr` does not wire itself into a session. Two pieces do that, and every
harness needs both:

1. A session-start hook that injects the inventory, so the agent starts the
   session already knowing what is enabled.
2. An instruction file that tells the agent how to read a skill it picks. One
   text covers all three harnesses.

### Injecting the Inventory

Run `skills-mgr list` from a session-start hook and its output becomes part of
the session's opening context. One command serves every harness: `list` reads
`CLAUDECODE`, `GROK_AGENT` or `GROK_SESSION_ID`, and `CODEX_THREAD_ID` from the
hook's environment and scopes itself to whichever it finds, so no scope flag is
needed.

Codex, in `~/.codex/hooks.json`:

```json
{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          { "type": "command", "command": "skills-mgr list", "timeout": 5 }
        ]
      }
    ],
    "SubagentStart": [
      {
        "hooks": [
          { "type": "command", "command": "skills-mgr list", "timeout": 5 }
        ]
      }
    ]
  }
}
```

Claude Code takes the same shape in `~/.claude/settings.json`, under a
`"matcher": "*"` wrapper. Grok takes it as a JSON file in `~/.grok/hooks/`.

The inventory has to be supplied when a session begins, when a subagent starts
with a fresh context, and after compaction. Register whichever events the
harness offers:

| Harness | Events |
| --- | --- |
| Codex | `SessionStart`, `SubagentStart` |
| Claude Code | `SessionStart`, `SubagentStart`, `PostCompact` |
| Grok | `SessionStart`, `PostCompact` |

Codex runs `SessionStart` again after compaction with `source: "compact"`, before
the next model request, so its existing hook restores the inventory. Claude Code
and Grok use `PostCompact` for the same job.

Pass an explicit `--claude`, `--grok`, or `--codex` only where the environment
cannot answer the question: a wrapper that clears it, or a session that sets
more than one marker. `list` hides nothing when it cannot identify exactly one
harness.

### The Instruction

Because the hook supplies the inventory, this text does not ask the agent to run
`list` itself. Put it in `~/.codex/AGENTS.md`, `~/.claude/CLAUDE.md`, and
`~/.grok/AGENTS.md`:

```markdown
## Skills

Read a skill whose listed description matches the operation you are about to
perform, just in time and exactly once per context. Start with the most
specific owner, and add another only when it covers a separate responsibility.
Keep a loaded skill active across follow-ups.

Read a skill's instructions with `skills-mgr get <skill-name> [start:end]`, and
a listed reference with `skills-mgr get <skill-name>/<relative-path>
[start:end]`. Omit the optional 1-based inclusive range to read the whole file,
and load only the references you actually need.
Run scripts with `skills-mgr run <skill-name>/<relative/script> [args...]`.

If a skill is missing or unreadable, say so briefly and carry on.
```

That is the minimum that makes the command usable. Selection policy beyond it is
yours to add: how to arbitrate between overlapping workflow skills, and when a
delegated agent owns its own skills.

### Per-Harness Setup

Claude Code and Grok need nothing beyond the instruction file and the hook. For
every harness, the manager's selection governs skills adopted into its home;
skills left in harness-owned locations stay outside that selection.

| Harness | Instruction file | Skills outside the manager selection |
| --- | --- | --- |
| Codex | `~/.codex/AGENTS.md` | Skills kept in Codex-owned locations or installed through plugins |
| Claude Code | `~/.claude/CLAUDE.md` | Skills left in `.claude/skills` or `~/.claude/skills` |
| Grok | `~/.grok/AGENTS.md` | Skills left in `.grok/skills`, `~/.grok/skills`, the shared `.agents/skills` roots, or the `.claude/skills` roots |

Grok scans the Claude directories for compatibility. Set
`GROK_CLAUDE_SKILLS_ENABLED=false` and `skills-mgr` stops crediting Grok with
them. The equivalent `[compat.claude]` setting in Grok's `config.toml` is not
read, so turning it off there means a skill is named to the agent twice rather
than withheld from it. Because Grok claims the shared roots too,
`skills-mgr list --grok` reports little more than the installed remote skills.

Codex is the only harness that provides options both to replace its base
instructions and to disable its built-in skills instructions and catalog. To
use the instruction above and let `skills-mgr` supply the catalog, set both in
`config.toml`:

```toml
model_instructions_file = "/path/to/your/base_instructions.md"

[skills]
include_instructions = false
```

The stock base prompts are published at
[openai/codex](https://github.com/openai/codex/blob/main/codex-rs/protocol/src/prompts/base_instructions/default.md)
and in
[system_prompts_leaks](https://github.com/asgeirtj/system_prompts_leaks/blob/main/OpenAI/Codex/gpt-5.6.md).
Start from one and delete its `## Using skills` section, leaving nothing in its
place.

## Command Reference

Every command uses the current working directory as the project.

| Command | Result |
| --- | --- |
| `skills-mgr` | Open the project selection interface |
| `skills-mgr -g` | Open the global selection interface |
| `skills-mgr help` | Print every accepted invocation form |
| `skills-mgr adopt` | Move all valid shared skills from `$HOME/.agents/skills` into the manager home |
| `skills-mgr list` | Write the enabled skills to stdout as XML: name, description, and reference-file tree |
| `skills-mgr get <skill>` | Write the body of the skill's `SKILL.md` to stdout |
| `skills-mgr get <skill>/<path>` | Write a file from the skill to stdout |
| `skills-mgr get <skill>/<path> <start>:<end>` | Write an inclusive, 1-based line range |
| `skills-mgr run <skill>/<script> [args...]` | Run a script from the skill, passing through its standard streams and exit status |
| `skills-mgr sync` | Fetch the enabled remote skills this project references but this machine does not have |
| `skills-mgr daemon` | Refresh registry metadata and stale remote content until interrupted |
| `skills-mgr daemon refresh` | Ask the running daemon to refresh the skills.sh registry cache now |
| `skills-mgr daemon sync` | Ask the running daemon to update stale persisted remote skills now |

`get` and `run` reject a skill that is not effectively enabled in this project.
For Grok plugin and bundled skills, that status is the one shown in the TUI and
owned by `$HOME/.grok/config.toml`. For Claude plugin skills, it is Claude's
`enabledPlugins` value. `list`, `get`, and `run` accept the `--claude`,
`--grok`, and `--codex` flags described under [Agent Integration](#agent-integration),
though only `list` filters on them.

`get` omits YAML frontmatter from Markdown files, and a line range applies to
that frontmatter-free content.

For an installed remote skill, `get` applies its local `SKILL.md` patch before
writing the requested body or range. If a provider refresh makes the patch no
longer applicable, `get` writes the unmodified provider body to stdout, reports
a compact patch error on stderr, and exits nonzero so its caller cannot silently
consume the fallback as the edited skill.

`run` uses the skill directory as the script's working directory. Executable
files run directly, non-executable `.py` files run under `python3`, and
non-executable `.js`, `.mjs`, `.cjs`, `.ts`, `.mts`, and `.cts` files run under
the first of `node` and `bun` found in `PATH`.

```sh
# Read an enabled skill's main instructions.
skills-mgr get writing-readme

# Read part of a reference file.
skills-mgr get deliver-vertical-slice/references/validation-scenarios.md 10:30

# Run a script the skill ships, passing arguments through to it.
skills-mgr run go-microoptimizations/scripts/go_asm_metrics.py ./...
```

## Skill Discovery

A skill is a directory holding a `SKILL.md` whose YAML frontmatter carries a
valid `name` and a non-empty `description`. Names may contain letters, digits,
`.`, `_`, `-`, and `:`, up to 80 characters.

Discovery reads these roots in order, and the first one to declare a given name
owns it:

| Priority | Root | Reported source |
| --- | --- | --- |
| 1 | `./.agents/skills` | `project` |
| 2 | `$HOME/.agents/skills` | `user` |
| 3 | `$HOME/.skills-mgr/skills` | `managed` |
| 4 | `./.claude/skills` | `claude` |
| 5 | `./.grok/skills` | `grok` |
| 6 | `./.codex/skills` | `codex` |
| 7 | `$CODEX_HOME/skills`, or `$HOME/.codex/skills` when `CODEX_HOME` is unset | `codex` |
| 8 | `/etc/codex/skills` | `admin` |
| 9 | `skills` directories in `$CODEX_HOME/plugins/cache`, up to 10 levels deep | `plugin` |
| 10 | The `skills-mgr` remote store | The provider name |

A `.system` subdirectory inside either Codex root is scanned as well and
reported as `bundled`. Missing, unreadable, or invalid entries are skipped
silently, as are the placeholder directories `skills-mgr` maintains for
autocomplete.

Grok Plugin and Bundled rows use metadata from `grok inspect --json`. Space on
one of those rows changes the global `skills.disabled` list in
`$HOME/.grok/config.toml`; Grok does not support a project-local override for
these skills. The other status and source-editing keys do not apply to those
rows. Claude Plugin rows read their status from `enabledPlugins` in
`$HOME/.claude/settings.json` and are display-only.

The interactive interface also reads installed Claude plugin skill directories
from `$HOME/.claude/plugins/installed_plugins.json` and Grok Plugin and Bundled
skills from `grok inspect --json`. These harness-owned catalogs are displayed
in their nested TUI tabs and are never emitted by `skills-mgr list`, regardless
of their displayed status. `get` and `run` still serve an enabled native skill
when named, using that harness's own enabled status rather than the manager
selection. A disabled skill with the same name is skipped in favor of a later
enabled owner.

Roots 4 through 9 belong to a specific harness. Under a `--claude`, `--grok`,
or `--codex` scope, roots owned by a different harness are dropped from the
catalog entirely. Roots assigned to the scoped harness remain in its catalog
but are omitted from `list`; selection takes effect after a skill is adopted
into the manager home.

## The Selection File

`.skills-mgr.json` is strict JSON at schema revision `3`. Revisions `1` and `2`
still load and are upgraded on the next selection write. The full contract is
in [`skills-mgr.schema.json`](skills-mgr.schema.json).

```json
{
  "schema_revision": 3,
  "skills": {
    "writing-readme": {
      "enabled": true
    },
    "unused-skill": {
      "enabled": false
    },
    "golang-best-practices": {
      "enabled": "lang go"
    }
  }
}
```

Prefer the interactive interface for edits. Configured names that no longer
resolve to a discovered skill are retained and ignored. Coordination locks live
in the user cache, not the project directory, so nothing extra appears in your
working tree.

Commit `.skills-mgr.json` when a repository should share its selection.

### Conditional Expressions

> An `enabled` string runs as Bash, with your environment and filesystem
> permissions, whenever the skill's state is checked. Review committed
> `.skills-mgr.json` changes before running `skills-mgr` in a repository you do
> not trust.

A string value is parsed as Bash by [`mvdan.cc/sh`](https://mvdan.cc/sh) and
evaluated from the project directory. Final status `0` enables the skill,
status `1` disables it, and anything from `2` to `255` is an error. External
commands run normally, so `command -v herdr` works as a condition.

Three builtins answer the common questions. All three read one shared scan of
the project described under [Evaluation Cost](#evaluation-cost), and none of
them starts a process.

#### `has_dependency <name> ['<operator><version>']`

Tests for a dependency declaration in any `go.mod`, `Cargo.toml`, or
`package.json` the scan reached. Operators are `>=`, `==`, `<=`, and `<`. A
quoted version argument may combine comparisons with `&&` and `||`; `&&` binds
tighter, and every comparison in an `&&` group must match the same declaration.
Comparisons use the precision you supply, so `==2` matches any version in major
`2`, while `==2.1` also compares the minor version.

Indirect Go requirements are ignored. Cargo dependency, dev-dependency,
optional-dependency, and `[workspace.dependencies]` catalog entries all count,
even when no member inherits them. A declaration with no version, such as a
Cargo path dependency, matches only the name-only form.

Version checks read the numeric boundaries written in a declaration rather than
fully interpreting npm or Cargo range semantics, so exact declarations are the
most predictable.

```sh
has_dependency tauri '>=2 && <3' || has_dependency '@tauri-apps/api' '>=2 && <3'
```

#### `lang <language>`

Tests whether the project contains a matching package marker or file extension.
A mixed-language project satisfies every language present. Names and aliases
are lowercase.

| Language | Package markers and file extensions |
| --- | --- |
| `go` | `go.mod`, `.go` |
| `rust` | `Cargo.toml`, `.rs` |
| `node` | `package.json` |
| `typescript` / `ts` | `tsconfig.json`, `.ts`, `.tsx`, `.mts`, `.cts` |
| `tsx` | `.tsx` |
| `javascript` / `js` | `.js`, `.jsx`, `.mjs`, `.cjs` |
| `jsx` | `.jsx` |
| `html` | `.html`, `.htm` |
| `css` | `.css` |
| `python` | `pyproject.toml`, `requirements.txt`, `Pipfile`, `.py`, `.pyw` |
| `c` | `.c` |
| `c++` | `.C`, `.cc`, `.cpp`, `.cxx`, `.c++`, `.hh`, `.hpp`, `.hxx` |
| `c#` | `.cs`, `.csproj` |
| `java` | `.java` |
| `lua` | `.lua` |
| `vb` | `.vb`, `.vbproj` |
| `php` | `composer.json`, `.php` |
| `r` | `.r`, `.rmd`, `.rproj`, matched case-insensitively |
| `ruby` | `Gemfile`, `.rb` |
| `swift` | `Package.swift`, `.swift` |
| `perl` | `cpanfile`, `.pl`, `.pm` |
| `assembly` / `asm` | `.asm`, `.s`, matched case-insensitively |
| `shell` / `sh` | `.sh` |
| `bash` | `.bash` |
| `postgres` | `postgresql.conf`, `pg_hba.conf`, `pg_ident.conf`, `.psql` |
| `sql` | `.sql` |
| `yaml` | `.yaml`, `.yml` |
| `json` | `.json` |
| `toml` | `.toml` |
| `ini` | `.ini` |

One file can detect several languages: `Cargo.toml` detects `rust` and `toml`,
and `package.json` detects `node` and `json`.

#### `tooling <name>`

Tests whether the project contains a conventional lockfile, configuration file,
or build entrypoint for a tool. This is evidence in the repository, not whether
the executable is installed.

| Tool | Project evidence |
| --- | --- |
| `bun` | `bun.lock`, `bun.lockb`, `bunfig.toml` |
| `yarn` | `yarn.lock`, `.yarnrc`, `.yarnrc.yml`, `.yarnrc.yaml` |
| `deno` | `deno.json`, `deno.jsonc`, `deno.lock` |
| `npm` | `package-lock.json`, `npm-shrinkwrap.json` |
| `pnpm` | `pnpm-lock.yaml`, `pnpm-workspace.yaml` |
| `maven` | `pom.xml`, `mvnw`, `mvnw.cmd` |
| `composer` | `composer.json`, `composer.lock` |
| `cmake` | `CMakeLists.txt`, `CMakePresets.json`, `CMakeUserPresets.json` |
| `make` | `Makefile`, `makefile`, `GNUmakefile` |
| `just` | `justfile`, `Justfile`, `.justfile` |
| `shadowtree` | `.shadowtree.toml` |
| `taskfile` | `Taskfile.yml`, `Taskfile.yaml`, `taskfile.yml`, `taskfile.yaml` |
| `bazel` | `.bazelrc`, `MODULE.bazel`, `WORKSPACE`, `WORKSPACE.bazel`, `BUILD`, `BUILD.bazel` |
| `docker` | `Dockerfile`, `Dockerfile.*`, `docker-bake.hcl`, `docker-bake.json`, or Docker Compose evidence |
| `docker-compose` | `compose.yml`, `compose.yaml`, `docker-compose.yml`, `docker-compose.yaml` |
| `kubernetes` / `k8s` | `kustomization.yml`, `kustomization.yaml`, `Kustomization`, `Chart.yaml`, `skaffold.yml`, `skaffold.yaml` |
| `pip` | `pip.conf`, `pip.ini`, or a `requirements*.txt` file |
| `uv` | `uv.lock`, `uv.toml` |

#### Evaluation Cost

`has_dependency`, `lang`, and `tooling` share one scan of the project. It runs
at most once per command, however many skills use a condition, and directory
reads run on a bounded worker pool.

The scan honors Git's ignore rules, so an ignored file is not evidence. It
applies `.git/info/exclude` and every `.gitignore` from the enclosing worktree
root down through the directories it visits, while a file recorded in the Git
index counts even when a rule would otherwise ignore it. Repository metadata is
read in process, so no `git` executable is involved. Outside a worktree, or when
that metadata cannot be read, in-tree `.gitignore` files still apply. `.git`,
`node_modules`, and `target` are always skipped, and unreadable directories are
passed over.

Two consequences are worth knowing:

- Run from `$HOME`, only its direct entries count. Home is where you run
  commands, not one project containing every checkout beneath it.
- A project directory that its parent repository ignores, and does not track,
  produces no evidence, so every condition on it is false.

```sh
lang ts && (tooling pnpm || tooling bun)
```

## Where Skill Content Lives

`skills-mgr` owns two stores, and content in either one is governed by the
selection because no harness scans them.

### The Manager Home

Skills you write belong in `$HOME/.skills-mgr/skills`. Press `a` on the
Installed tab to move one there from `$HOME/.agents/skills`, and `a` again to
move it back.

That move is what makes a selection entry mean anything for a skill you wrote.
Grok scans the shared `.agents/skills` roots, so a skill sitting there loads
into every session whatever its entry says. Claude Code does not scan them at
all, so the same skill is missing from its slash-command menu. Placeholders fix
both.

Adoption writes the placeholder after the move; release removes it first,
because in global mode the release destination is the placeholder's own path.
`e` still edits the content, and `u` refuses it.

### The Remote Store

Skills installed from skills.sh or SkillsMP live under the user cache directory,
in `skills-mgr/remote-skills`, because `sync` can refetch them. Authored content
cannot be refetched, which is why the manager home sits outside the cache.
Content is never executed at install time.

Remote edits are stored as stable per-entry `.patch` files under the global
manager home at `$HOME/.skills-mgr/skills/.remote-patches/`, not in this cache.
That location can be tracked with the rest of the manager home and restored on
a machine with an empty remote cache. Their body is a conventional unified diff,
with base and result SHA-256 headers protecting the layer from mismatched or
damaged content. Patches survive refreshes, are removed on uninstall, and affect
`skills-mgr get`; discovery metadata and `skills-mgr run` continue to use the
validated provider content.

Registry responses are cached alongside the content. The SkillsMP client sends
`SKILLSMP_API_KEY` as a bearer token when that variable is set, and makes
unauthenticated requests otherwise. Provider results are validated before they
reach the store: absolute or escaping paths, non-regular entries, duplicate
paths, responses over 16 MiB, and a root `SKILL.md` whose name does not match
the catalog entry are all rejected.

### Placeholders

Enabling a skill from either store writes frontmatter-only placeholders under
`.agents/skills/` and `.claude/skills/`, or under `$HOME` in global mode, so the
harness can offer the name. Disabling removes the placeholders and leaves the
content in place.

Every placeholder sets `disable-model-invocation: true`, so the harness offers
the name in its slash-command menu for you while withholding the description
from the model. `skills-mgr list` stays the only model-facing view of what
applies here. Two consequences follow: a conditional entry gets a placeholder
like any other, because a stub the model cannot see cannot bypass a false
condition, and a leftover placeholder that a project override cannot delete
under `$HOME` costs the model nothing. `list`, `get`, and `run` honor the
project override regardless.

A skill outside both stores gets no placeholder. It already occupies its
harness-owned path, and a stub there would collide with the real directory.

### Reproducing a Selection Elsewhere

A committed `.skills-mgr.json` records each remote skill's provider, provider
ID, name, and locator, which is everything needed to refetch it. On another
machine:

```sh
skills-mgr sync
```

`sync` matches project and inherited global names against identities in the
global selection and local store, fills in any missing project identity
metadata, fetches every effectively enabled remote that is absent or stale,
and prints one line per skill synchronized. It never fetches a disabled entry,
and a failure names the skill, returns non-zero, and leaves both selection files
unchanged.

Nothing else downloads on your behalf. `list`, `get`, `run`, and TUI startup
will not fetch a referenced remote skill they have never seen.

## Remote Refresh Daemon

`skills-mgr daemon` listens on `$XDG_RUNTIME_DIR/skills-mgr.sock`, falling back
to `skills-mgr.sock` in the user cache `skills-mgr` directory. It refreshes
skills.sh registry metadata at startup and every five minutes, and refreshes
installed remote content once it passes three hours old. Structured text logs go
to stderr for start, stop, inbound commands, cache refresh, and each
remote-skill update; a failure is logged and does not stop later cycles.

`skills-mgr daemon refresh` asks that process to refresh the registry cache
right now. `skills-mgr daemon sync` downloads any missing, unconditionally
enabled remote identity recorded in `$HOME/.skills-mgr/.skills-mgr.json`, then
updates stale remote skills. It does not inspect project selections, so use
`skills-mgr sync` in the project for inherited conditions or project-only
identities. Both commands wait for the work to finish and fail if no daemon is
running. Startup and timed daemon cycles continue to refresh only identities
already present in the store.

To install it as a systemd user unit:

```sh
shadowtree install-systemd
```

That recipe runs `shadowtree install` first, so the binary lands wherever
`GOBIN` points, defaulting to `$HOME/go/bin`. That is the path
[`skills-mgr.service`](skills-mgr.service) names in `ExecStart`. If you set
`GOBIN` somewhere else, edit `ExecStart` to match before enabling the unit.

## Documentation

Design notes for the remote-selection subsystem live in
[`doc/brief.md`](doc/brief.md). Product requirements and architecture contracts
are inventoried by [`doc/spec/index.md`](doc/spec/index.md) and
[`doc/architecture/index.md`](doc/architecture/index.md).

## Development

The repository uses [Shadowtree](https://github.com/yusing/shadowtree) recipes.

```sh
git clone https://github.com/yusing/skills-mgr.git
cd skills-mgr
shadowtree check
```

| Workflow | Command | Result |
| --- | --- | --- |
| Build | `shadowtree build` | Compile all Go packages in a sandbox |
| Test | `shadowtree test` | Run the Go test suite |
| Vet and test | `shadowtree check` | Run `go vet`, then the test suite |
| Race detection | `shadowtree test-race` | Run tests with the race detector |
| Lint | `shadowtree lint` | Run `golangci-lint` |
| Format | `shadowtree fmt` | Format Go source in the checkout |
| Tidy | `shadowtree tidy` | Tidy `go.mod` and `go.sum` |
| Install checkout | `shadowtree install` | Install the main package with debug information stripped |
| Install service | `shadowtree install-systemd` | Install to `GOBIN` and enable the systemd user unit |

## License

MIT. See [`LICENSE`](LICENSE).
