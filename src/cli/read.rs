use std::{collections::HashMap, env, ffi::OsString, io::Write, ops::RangeInclusive};

use crate::{
    Harness, SkillsManager, catalog, condition::Conditions, content, selection::Selections,
};

use super::{Result, current_manager};

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

fn infer_harness() -> Option<Harness> {
    let set = |name| env::var_os(name).is_some_and(|value| !value.is_empty());
    let mut harnesses = [
        set("CLAUDECODE").then_some(Harness::Claude),
        (set("GROK_AGENT") || set("GROK_SESSION_ID")).then_some(Harness::Grok),
        set("CODEX_THREAD_ID").then_some(Harness::Codex),
    ]
    .into_iter()
    .flatten();
    let first = harnesses.next();
    if harnesses.next().is_none() {
        first
    } else {
        None
    }
}

pub(super) fn list(out: &mut impl Write, args: &[OsString]) -> Result<()> {
    let (mut harnesses, rest) = harness_args(args)?;
    if !rest.is_empty() {
        return Err("usage: skills-mgr list [--claude] [--grok] [--codex]".into());
    }
    if harnesses.is_empty() {
        harnesses.extend(infer_harness());
    }
    let skills = catalog::list_harnesses(&current_manager()?, &harnesses)?;
    if skills.is_empty() {
        writeln!(out, "<skills></skills>")?;
        return Ok(());
    }
    writeln!(out, "<skills>")?;
    for skill in skills {
        write!(
            out,
            "  <skill name=\"{}\" description=\"{}\">",
            xml_escape(&skill.name, true),
            xml_escape(&skill.description, true),
        )?;
        if !skill.references.is_empty() {
            writeln!(
                out,
                "\n    <references>{}</references>",
                xml_escape(&reference_list(&skill.references), false)
            )?;
            write!(out, "  ")?;
        }
        writeln!(out, "</skill>")?;
    }
    writeln!(out, "</skills>")?;
    Ok(())
}

fn reference_list(files: &[String]) -> String {
    // Root files are walked before references/, then directory groups appear
    // in the order of their first path.
    let mut directories = Vec::new();
    let mut groups: HashMap<&str, Vec<String>> = HashMap::new();
    for file in files
        .iter()
        .filter(|path| !path.contains('/'))
        .chain(files.iter().filter(|path| path.contains('/')))
    {
        let (dir, base) = file.rsplit_once('/').unwrap_or(("", file));
        groups
            .entry(dir)
            .or_insert_with(|| {
                directories.push(dir);
                Vec::new()
            })
            .push(shell_quote(base.strip_suffix(".md").unwrap_or(base)));
    }
    directories
        .into_iter()
        .map(|dir| {
            let prefix = if dir.is_empty() {
                String::new()
            } else {
                format!("{}/", shell_quote(dir))
            };
            let names = &groups[dir];
            match names.as_slice() {
                [name] => format!("{prefix}{name}.md"),
                _ => format!("{prefix}{{{}}}.md", names.join(",")),
            }
        })
        .collect::<Vec<_>>()
        .join(" ")
}

fn shell_quote(value: &str) -> String {
    if value
        .bytes()
        .all(|b| b.is_ascii_alphanumeric() || b"_./-".contains(&b))
    {
        value.into()
    } else {
        format!("'{}'", value.replace('\'', "'\\''"))
    }
}

fn xml_escape(value: &str, attribute: bool) -> String {
    let mut output = String::with_capacity(value.len());
    for c in value.chars() {
        match c {
            '&' => output.push_str("&amp;"),
            '<' => output.push_str("&lt;"),
            '>' => output.push_str("&gt;"),
            '"' if attribute => output.push_str("&#34;"),
            '\'' if attribute => output.push_str("&#39;"),
            '\t' => output.push_str("&#x9;"),
            '\n' => output.push_str("&#xA;"),
            '\r' => output.push_str("&#xD;"),
            '\u{0}'..='\u{1f}' | '\u{fffe}' | '\u{ffff}' => output.push('\u{fffd}'),
            c => output.push(c),
        }
    }
    output
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
    catalog::find(manager, name).map_err(|e| acquisition_error(name, e))
}

pub(super) fn acquisition_error(
    name: &str,
    error: impl std::fmt::Display,
) -> Box<dyn std::error::Error> {
    format!("{error}; use skills-mgr inspect {name} to diagnose").into()
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
