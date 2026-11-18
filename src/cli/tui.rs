use std::{
    collections::BTreeMap,
    env,
    io::{self, IsTerminal, Write},
    path::PathBuf,
    process::Command,
};

use serde::{Deserialize, Serialize};

use crate::{
    SkillsManager,
    catalog::{self, Skill, Source},
    selection::{self, Enabled, LOCK_NAME, Lock},
};

use super::{Result, current_manager, native};

#[derive(Deserialize)]
#[serde(tag = "op", rename_all = "snake_case", deny_unknown_fields)]
enum Request {
    Snapshot { global: bool },
}

pub(super) fn dispatch(out: &mut impl Write) -> Result<()> {
    match serde_json::from_reader(io::stdin().lock())? {
        Request::Snapshot { global } => snapshot(out, &current_manager()?, global),
    }
}

pub(super) fn launch(global: bool) -> Result<()> {
    if !io::stdin().is_terminal() || !io::stdout().is_terminal() {
        return Err("TUI requires a terminal".into());
    }
    let exe = env::current_exe()?.canonicalize()?;
    let tui = exe.with_file_name(format!("skills-mgr-tui{}", env::consts::EXE_SUFFIX));
    let mut command = Command::new(&tui);
    command.env("_SKILLS_MGR_TUI_CORE", exe);
    if global {
        command.arg("-g");
    }
    #[cfg(unix)]
    {
        use std::os::unix::process::CommandExt;
        Err(format!("start {}: {}", tui.display(), command.exec()).into())
    }
    #[cfg(not(unix))]
    {
        let status = command
            .status()
            .map_err(|err| format!("start {}: {err}", tui.display()))?;
        if !status.success() {
            std::process::exit(status.code().unwrap_or(1));
        }
        Ok(())
    }
}

#[derive(Serialize)]
#[serde(rename_all = "PascalCase")]
struct Row {
    name: String,
    description: String,
    path: PathBuf,
    root: PathBuf,
    entry_path: PathBuf,
    source: &'static str,
    editable: bool,
    remote_key: String,
    disable_model_invocation: bool,
    plugin: String,
    vendor: String,
    user_invocable: bool,
    compatibility_status: String,
    external_enabled: bool,
    content_missing: bool,
}

impl Row {
    fn from_skill(manager: &SkillsManager, skill: Skill, content_missing: bool) -> Result<Self> {
        let description = if content_missing {
            skill.metadata.description.clone()
        } else {
            skill.description(manager)?.into_owned()
        };
        let path = skill.manifest_path();
        let native = skill.native.unwrap_or_default();
        let mut row = Row {
            name: skill.metadata.name,
            description,
            path,
            root: skill.root,
            entry_path: skill.entry_path,
            source: skill.source.label(),
            editable: skill.source.editable(),
            remote_key: skill.remote_key.unwrap_or_default(),
            disable_model_invocation: skill.metadata.hide,
            plugin: native.plugin,
            vendor: native.vendor,
            user_invocable: native.user_invocable,
            compatibility_status: native.compatibility_status,
            external_enabled: native.enabled,
            content_missing,
        };
        if content_missing {
            row.path.clear();
            row.root.clear();
            row.entry_path.clear();
            row.editable = false;
        }
        Ok(row)
    }
}

#[derive(Serialize)]
#[serde(rename_all = "PascalCase")]
struct Snapshot {
    skills: Vec<Row>,
    selected: BTreeMap<String, bool>,
    global_selected: BTreeMap<String, bool>,
    project_selected: BTreeMap<String, bool>,
    global_expressions: BTreeMap<String, String>,
    project_expressions: BTreeMap<String, String>,
}

fn snapshot(out: &mut impl Write, manager: &SkillsManager, global: bool) -> Result<()> {
    let skills = catalog::discover(manager, &[], None)?;
    let global_lock = selection::global_path(manager)?
        .as_deref()
        .map(selection::load)
        .transpose()?
        .unwrap_or_default();
    let project_lock = if global {
        Lock::new()
    } else {
        selection::load(&manager.project.join(LOCK_NAME))?
    };
    let flags = |lock: &Lock| {
        lock.iter()
            .filter_map(|(name, entry)| {
                let flag = !matches!(entry.enabled.as_ref()?, Enabled::Boolean(false));
                Some((name.clone(), flag))
            })
            .collect::<BTreeMap<_, _>>()
    };
    let expressions = |lock: Lock| {
        lock.into_iter()
            .filter_map(|(name, entry)| match entry.enabled {
                Some(Enabled::Condition(expression)) => Some((name, expression)),
                _ => None,
            })
            .collect::<BTreeMap<_, _>>()
    };
    let global_selected = flags(&global_lock);
    let project_selected = flags(&project_lock);
    let mut selected = global_selected.clone();
    selected.extend(
        project_selected
            .iter()
            .map(|(name, flag)| (name.clone(), *flag)),
    );
    if !global {
        for (name, skill) in &skills {
            if matches!(skill.source, Source::Project) || skill.metadata.hide {
                selected.entry(name.clone()).or_insert(true);
            }
        }
    }
    let missing = native::missing_remote(manager, None)?;
    let claude = catalog::claude_plugins(manager, None, true)?;
    let grok = native::grok(manager, None)?;
    let rows = skills
        .into_values()
        .map(|skill| (skill, false))
        .chain(missing.into_iter().map(|skill| (skill, true)))
        .chain(claude.into_iter().chain(grok).map(|skill| (skill, false)))
        .map(|(skill, missing)| Row::from_skill(manager, skill, missing))
        .collect::<Result<Vec<_>>>()?;
    let snapshot = Snapshot {
        skills: rows,
        selected,
        global_selected,
        project_selected,
        global_expressions: expressions(global_lock),
        project_expressions: expressions(project_lock),
    };
    serde_json::to_writer(&mut *out, &snapshot)?;
    writeln!(out)?;
    Ok(())
}
