use std::{
    env,
    ffi::OsString,
    io::{self, Write},
    path::Path,
    process::ExitCode,
};

mod inspect;
#[cfg(unix)]
mod mutation;
mod native;
mod read;
mod tui;

use crate::{SkillsManager, selection::LOCK_NAME};

type Result<T> = std::result::Result<T, Box<dyn std::error::Error>>;

/// Entry point of the standalone CLI. Library consumers use `SkillsManager`.
pub fn main() -> ExitCode {
    let args: Vec<_> = env::args_os().skip(1).collect();
    match dispatch(&args, &mut io::stdout().lock()) {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("skills-mgr: {error}");
            ExitCode::FAILURE
        }
    }
}

fn dispatch(args: &[OsString], out: &mut impl Write) -> Result<()> {
    match args {
        [command] if command == "help" || command == "--help" => print_help(out, None),
        [command, target] if command == "help" => print_help(out, Some(&target.to_string_lossy())),
        [command, flag] if flag == "--help" && command != "--help" => {
            print_help(out, Some(&command.to_string_lossy()))
        }
        [command, ..] if command == "help" || command == "--help" => {
            Err("usage: skills-mgr help [command]".into())
        }
        [command, shell] if command == "completion" && shell == "fish" => {
            out.write_all(include_bytes!("../completion.fish"))?;
            Ok(())
        }
        [command, ..] if command == "completion" => Err("usage: skills-mgr completion fish".into()),
        [command] if command == "info" => print_info(out, &current_manager()?),
        [command, ..] if command == "info" => Err("usage: skills-mgr info".into()),
        [command, args @ ..] if command == "list" => read::list(out, args),
        [command, args @ ..] if command == "get" => read::get(out, args),
        [command, args @ ..] if command == "run" => read::run_script(args),
        [command, args @ ..] if command == "check" => read::check(out, args),
        [command, args @ ..] if command == "inspect" => inspect::inspect(out, args),
        #[cfg(unix)]
        [command, args @ ..] if command == "set" => mutation::set(out, args),
        #[cfg(unix)]
        [command, args @ ..] if command == "edit" => mutation::edit(out, args),
        #[cfg(unix)]
        [command, args @ ..] if command == "adopt" => mutation::adopt(out, args),
        [command] if command == "__tui" => tui::dispatch(out),
        [command, args @ ..] if command == "__selection-predicate" => read::predicate(args),
        [command, kind]
            if command == "__complete-skills" && (kind == "all" || kind == "enabled") =>
        {
            read::complete(out, kind == "enabled")
        }
        [command, kind] if command == "__complete-skills" && kind == "remote" => {
            read::complete_remote(out)
        }
        [] => tui::launch(false),
        [flag] if flag == "-g" => tui::launch(true),
        [command, ..] => Err(format!("unknown command {:?}", command.to_string_lossy()).into()),
    }
}

fn current_manager() -> Result<SkillsManager> {
    let project = env::current_dir().map_err(|e| format!("get current directory: {e}"))?;
    let mut manager = SkillsManager::for_project(project)?;
    manager.shell_conditions = true;
    Ok(manager)
}

pub(super) fn shell_condition(manager: &SkillsManager, expression: &str) -> io::Result<bool> {
    // The standalone CLI historically runs Bash expressions. Only use a shell
    // for syntax the in-process predicate evaluator cannot handle.
    let script = format!(
        r#"lang() {{ "$0" __selection-predicate lang "$@"; }}
tooling() {{ "$0" __selection-predicate tooling "$@"; }}
has_dependency() {{ "$0" __selection-predicate has_dependency "$@"; }}
{expression}"#
    );
    use std::process::{Command, Stdio};
    let status = Command::new("bash")
        .args(["--noprofile", "--norc", "-c", &script])
        .arg(env::current_exe()?)
        .current_dir(&manager.project)
        .env_remove("BASH_ENV")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()?;
    match status.code() {
        Some(0) => Ok(true),
        Some(1) => Ok(false),
        Some(code) => Err(io::Error::other(format!("status {code}"))),
        None => Err(io::Error::other(format!("shell terminated: {status}"))),
    }
}

const HELP: &[(&str, &str)] = &[
    (
        "help [command] | --help",
        "Show all commands or help for one command.\n    Each command also accepts --help on its own.",
    ),
    ("completion fish", "Print fish shell completions."),
    ("info", "Show metadata and skill paths; mark missing paths."),
    (
        "adopt",
        "Move shared user skills into the managed skill store.",
    ),
    (
        "list [--claude] [--grok] [--codex]",
        "Print enabled skill metadata as XML.",
    ),
    (
        "sync",
        "Fetch globally listed remote skills and this project's enabled ones.",
    ),
    (
        "install [-g] [--exclude <pattern>]... <repo-address> [skill-name]",
        "Install a repository skill and enable it for this project, or globally with -g.\n    --exclude leaves out matching files and directories on this and later fetches.",
    ),
    (
        "uninstall [-g] <skill>",
        "Uninstall a remote skill and remove its current selection and placeholders.\n    Use -g for globally configured skills; local and harness-owned skills are not removed.",
    ),
    (
        "get [--claude] [--grok] [--codex] <skill-name>[/relative/path] [start:end]",
        "Print an enabled skill or file, optionally limited to an inclusive 1-based line range.\n    Lines refer to the served body after frontmatter removal and any remote patch.\n    Examples: skills-mgr get example 1:120\n              skills-mgr get example 121:240",
    ),
    (
        "inspect <skill>",
        "Print JSON owner, selection, fallback, patch, and body-health diagnostics.",
    ),
    (
        "check <target> [target...]",
        "Validate served bodies and a skill's listed references without running scripts.",
    ),
    (
        "set [-g] <skill> <true|false|condition|inherit>",
        "Set a selection value, or remove its override with inherit.",
    ),
    (
        "edit [-g] <skill> --file <path|-> [--expect-sha256 <digest>]",
        "Replace a complete SKILL.md, preserving remote ownership with a local patch.",
    ),
    (
        "run [--claude] [--grok] [--codex] <skill-name>/<relative/script> [args...]",
        "Run a script from an enabled skill, forwarding arguments.",
    ),
];

fn print_help(out: &mut impl Write, target: Option<&str>) -> Result<()> {
    let target = target
        .filter(|target| !target.is_empty())
        .map(|target| if target == "--help" { "help" } else { target });
    let help = HELP.iter().filter(|(usage, _)| {
        target.is_none_or(|target| usage.split_ascii_whitespace().next() == Some(target))
    });
    if let Some(target) = target
        && help.clone().next().is_none()
    {
        return Err(format!("unknown help target {target:?}").into());
    }
    writeln!(out, "Usage:")?;
    if target.is_none() {
        writeln!(
            out,
            "  skills-mgr [-g]\n    Open the project skill selection interface, or global with -g."
        )?;
    }
    for (usage, description) in help {
        writeln!(out, "\n  skills-mgr {usage}\n    {description}")?;
    }
    Ok(())
}

fn print_info(out: &mut impl Write, manager: &SkillsManager) -> Result<()> {
    let project = &manager.project;
    let home = &manager.home;
    let cache = manager.cache.join("skills-mgr");
    let managed = manager.manager_home().join("skills");
    let path_status = |path: &Path| {
        let missing = path
            .metadata()
            .is_err_and(|error| error.kind() == io::ErrorKind::NotFound);
        let mut display = match (!cfg!(windows))
            .then(|| path.strip_prefix(home))
            .and_then(std::result::Result::ok)
        {
            Some(relative) if relative.as_os_str().is_empty() => "~".into(),
            Some(relative) => format!("~/{}", relative.display()),
            None => path.display().to_string(),
        };
        if missing {
            display.push_str(" [MISSING]");
        }
        display
    };
    writeln!(out, "Metadata:")?;
    for (label, path) in [
        ("Project selection", project.join(LOCK_NAME)),
        ("Global selection", manager.manager_home().join(LOCK_NAME)),
        ("skills.sh registry", cache.join("skills-sh.json")),
        ("SkillsMP registry", cache.join("skillsmp.json")),
    ] {
        writeln!(out, "  {label}: {}", path_status(&path))?;
    }
    writeln!(out, "Skills:")?;
    for (label, path) in [
        ("Project shared", project.join(".agents/skills")),
        ("User shared", home.join(".agents/skills")),
        ("Managed", managed.clone()),
        ("Project Claude", project.join(".claude/skills")),
        ("User Claude", home.join(".claude/skills")),
        ("Project Grok", project.join(".grok/skills")),
        ("User Grok", home.join(".grok/skills")),
        ("Project Codex", project.join(".codex/skills")),
        ("User Codex", manager.codex_home.join("skills")),
        ("Admin", "/etc/codex/skills".into()),
        ("Claude plugins", home.join(".claude/plugins")),
        (
            "Codex plugin cache",
            manager.codex_home.join("plugins/cache"),
        ),
    ] {
        writeln!(out, "  {label}: {}", path_status(&path))?;
    }
    writeln!(out, "Remote skills:")?;
    writeln!(
        out,
        "  Store: {}",
        path_status(&cache.join("remote-skills"))
    )?;
    writeln!(
        out,
        "  Local patches: {}",
        path_status(&managed.join(".remote-patches"))
    )?;
    Ok(())
}
