use std::{
    collections::{HashMap, HashSet},
    ffi::OsString,
    io::{self, Write},
    ops::RangeInclusive,
    path::Path,
    process::Command,
};

use crate::{
    Harness, SkillsManager, catalog, condition::Conditions, content, remote, selection::Selections,
};

use super::{Result, current_manager, native};

pub(super) fn harness_args(mut args: &[OsString]) -> Result<(Vec<Harness>, &[OsString])> {
    let mut harnesses = Vec::new();
    while let [head, tail @ ..] = args {
        let head = head.to_string_lossy();
        let harness = match head.as_ref() {
            "--claude" => Harness::Claude,
            "--grok" => Harness::Grok,
            "--codex" => Harness::Codex,
            head if head.starts_with("--") => return Err(format!("unknown flag {head:?}").into()),
            _ => break,
        };
        if !harnesses.contains(&harness) {
            harnesses.push(harness);
        }
        args = tail;
    }
    Ok((harnesses, args))
}

pub(super) fn list(out: &mut impl Write, args: &[OsString]) -> Result<()> {
    let (harnesses, rest) = harness_args(args)?;
    if !rest.is_empty() {
        return Err("usage: skills-mgr list [--claude] [--grok] [--codex]".into());
    }
    let xml = current_manager()?.list_xml(&harnesses)?;
    out.write_all(xml.as_bytes())?;
    Ok(())
}

pub(super) fn get(out: &mut impl Write, args: &[OsString]) -> Result<()> {
    let (_, args) = harness_args(args)?;
    let [target, rest @ ..] = args else {
        return Err(GET_USAGE.into());
    };
    if rest.len() > 1 {
        return Err(GET_USAGE.into());
    }
    let target = target.to_string_lossy();
    let (name, relative) = content::split_target(&target)?;
    let manager = current_manager()?;
    let skill = acquire(&manager, name)?;
    let lines = rest
        .first()
        .filter(|arg| !arg.is_empty())
        .map(|arg| line_range(&arg.to_string_lossy()))
        .transpose()
        .map_err(|e| acquisition_error(name, e))?;
    let (bytes, patch_error) =
        content::get_with_patch_error(&manager, &skill, relative, lines, true)
            .map_err(|e| acquisition_error(name, e))?;
    out.write_all(&bytes)?;
    if let Some(error) = patch_error {
        return Err(acquisition_error(name, error));
    }
    Ok(())
}

const GET_USAGE: &str =
    "usage: skills-mgr get [--claude] [--grok] [--codex] <skill-name>[/relative/path] [start:end]";

fn line_range(value: &str) -> Result<RangeInclusive<usize>> {
    let Some((start, end)) = value
        .split_once(':')
        .filter(|(a, b)| !a.is_empty() && !b.is_empty())
    else {
        return Err(format!("invalid line range {value:?}; want start:end").into());
    };
    if let (Ok(start), Ok(end)) = (start.parse::<isize>(), end.parse::<isize>())
        && start > 0
        && end >= start
    {
        return Ok(start as usize..=end as usize);
    }
    Err(format!("invalid line range {value:?}; want positive start:end with start <= end").into())
}

pub(super) fn acquire(manager: &SkillsManager, name: &str) -> Result<catalog::Skill> {
    let (skill, fallback) =
        native::resolve(manager, name).map_err(|e| acquisition_error(name, e))?;
    if fallback {
        eprintln!(
            "skills-mgr: using {} skill {name:?} at {} after an inaccessible owner",
            skill.source.label(),
            skill.manifest_path().display()
        );
    }
    Ok(skill)
}

pub(super) fn acquisition_error(
    name: &str,
    error: impl std::fmt::Display,
) -> Box<dyn std::error::Error> {
    format!("{error}; use skills-mgr inspect {name} to diagnose").into()
}

pub(super) fn run_script(args: &[OsString]) -> Result<()> {
    let (_, args) = harness_args(args)?;
    let [target, args @ ..] = args else {
        return Err("usage: skills-mgr run [--claude] [--grok] [--codex] <skill-name>/<relative/script> [args...]".into());
    };
    let target = target.to_string_lossy();
    if !target.contains('/') {
        return Err(format!(
            "invalid script target {target:?}; want <skill-name>/<relative/script>"
        )
        .into());
    }
    let (name, relative) = content::split_target(&target)?;
    let skill = acquire(&current_manager()?, name)?;
    let dir = content::open_root(&skill.root).map_err(|e| acquisition_error(name, e))?;
    let metadata = dir
        .metadata(relative)
        .map_err(|e| acquisition_error(name, format!("stat {target}: {e}")))?;
    if !metadata.is_file() {
        return Err(acquisition_error(
            name,
            format!("{target} is not a regular file"),
        ));
    }
    let script = skill.root.join(relative);
    #[cfg(unix)]
    let executable = {
        use cap_std::fs::MetadataExt;
        metadata.mode() & 0o111 != 0
    };
    #[cfg(not(unix))]
    let executable = false;
    let extension = relative.extension().and_then(|suffix| suffix.to_str());
    let command = |runtime: Option<&str>| {
        let mut command = if let Some(runtime) = runtime {
            let mut command = Command::new(runtime);
            command.arg(&script);
            command
        } else {
            Command::new(&script)
        };
        command.current_dir(&skill.root).args(args);
        command
    };
    let status = match (executable, extension) {
        (false, Some("py")) => command(Some("python3")).status(),
        (false, Some("js" | "mjs" | "cjs" | "ts" | "mts" | "cts")) => {
            let node = command(Some("node")).status();
            if node
                .as_ref()
                .is_err_and(|e| e.kind() == io::ErrorKind::NotFound)
            {
                let bun = command(Some("bun")).status();
                if bun
                    .as_ref()
                    .is_err_and(|e| e.kind() == io::ErrorKind::NotFound)
                {
                    return Err(
                        "run JavaScript or TypeScript: neither node nor bun was found in PATH"
                            .into(),
                    );
                }
                bun
            } else {
                node
            }
        }
        _ => command(None).status(),
    }
    .map_err(|e| format!("run {target}: {e}"))?;
    if !status.success() {
        std::process::exit(status.code().unwrap_or(1));
    }
    Ok(())
}

pub(super) fn check(out: &mut impl Write, args: &[OsString]) -> Result<()> {
    if args.is_empty() {
        return Err("usage: skills-mgr check <target> [target...]".into());
    }
    let manager = current_manager()?;
    let mut seen = HashSet::new();
    let mut resolved = HashMap::new();
    let mut diagnostics = io::stderr().lock();
    let mut report = |errors: &mut Vec<String>,
                      target: &str,
                      result: std::result::Result<(), String>|
     -> io::Result<()> {
        match result {
            Ok(()) => writeln!(out, "PASS {target}"),
            Err(error) => {
                let failure = format!("{target}: {error}");
                writeln!(diagnostics, "FAIL {failure}")?;
                errors.push(failure);
                Ok(())
            }
        }
    };
    let check_file = |skill: &catalog::Skill, path: &Path| {
        let (body, patch_error) = content::get_with_patch_error(&manager, skill, path, None, false)
            .map_err(|e| e.to_string())?;
        debug_assert!(patch_error.is_none());
        content::validate_body(&body, content::is_manifest(path) && skill.metadata.hide)
    };
    let mut errors = Vec::new();
    let mut checked_references = HashSet::new();
    for arg in args {
        let target = arg.to_string_lossy();
        if !seen.insert(target.to_string()) {
            continue;
        }
        let (name, path) = match content::split_target(&target) {
            Ok(target) => target,
            Err(error) => {
                report(&mut errors, &target, Err(error.to_string()))?;
                continue;
            }
        };
        let skill = match resolved
            .entry(name.to_owned())
            .or_insert_with(|| native::resolve(&manager, name).map(|(skill, _)| skill))
        {
            Ok(skill) => skill,
            Err(error) => {
                report(&mut errors, &target, Err(error.to_string()))?;
                continue;
            }
        };
        report(&mut errors, &target, check_file(skill, path))?;
        if content::is_manifest(path) && checked_references.insert(name.to_owned()) {
            match content::open_root(&skill.root)
                .and_then(|dir| catalog::references(&skill.root, &dir))
            {
                Ok(mut paths) => {
                    paths.sort_by_key(|path| path.contains('/'));
                    for path in paths {
                        let target = format!("{name}/{path}");
                        if seen.insert(target.clone()) {
                            report(&mut errors, &target, check_file(skill, Path::new(&path)))?;
                        }
                    }
                }
                Err(error) => errors.push(format!("{name} references: {error}")),
            }
        }
    }
    if errors.is_empty() {
        Ok(())
    } else {
        Err(errors.join("\n").into())
    }
}

pub(super) fn complete(out: &mut impl Write, enabled_only: bool) -> Result<()> {
    let manager = current_manager()?;
    let skills = catalog::discover(&manager, &[], None)?;
    let mut selected = enabled_only
        .then(|| Selections::load(&manager))
        .transpose()?;
    for (name, skill) in skills {
        if let Some(selected) = &mut selected
            && !selected.enabled(&manager, &skill)?
        {
            continue;
        }
        writeln!(out, "{name}")?;
    }
    Ok(())
}

pub(super) fn complete_remote(out: &mut impl Write) -> Result<()> {
    for (record, _) in remote::records(&remote::store_path(&current_manager()?), None)? {
        writeln!(out, "{}", record.reference.name)?;
    }
    Ok(())
}

pub(super) fn predicate(args: &[OsString]) -> Result<()> {
    let [command, args @ ..] = args else {
        return Err("missing selection predicate".into());
    };
    let command = command.to_string_lossy();
    let args: Vec<_> = args.iter().map(|arg| arg.to_string_lossy()).collect();
    let args = args.iter().map(|arg| arg.as_ref()).collect::<Vec<_>>();
    match Conditions::default().predicate(&current_manager()?, &command, &args) {
        Ok(true) => Ok(()),
        Ok(false) => std::process::exit(1),
        Err(error) => {
            eprintln!("skills-mgr: {error}");
            std::process::exit(2);
        }
    }
}
