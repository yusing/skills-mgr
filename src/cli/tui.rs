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

#[cfg(unix)]
use super::mutation::{self, EnabledChange, Scope};
use super::{Result, current_manager, native};

#[derive(Deserialize)]
#[serde(tag = "op", rename_all = "snake_case", deny_unknown_fields)]
enum Request {
    Snapshot {
        global: bool,
    },
    #[cfg(unix)]
    SetEnabled {
        global: bool,
        name: String,
        path: PathBuf,
        value: Option<Enabled>,
    },
    #[cfg(unix)]
    Toggle {
        global: bool,
        name: String,
        path: PathBuf,
    },
    #[cfg(unix)]
    Edit {
        global: bool,
        name: String,
        path: PathBuf,
        draft: PathBuf,
        digest: String,
    },
    #[cfg(unix)]
    Relocate {
        global: bool,
        name: String,
        path: PathBuf,
    },
}

pub(super) fn dispatch(out: &mut impl Write) -> Result<()> {
    match serde_json::from_reader(io::stdin().lock())? {
        Request::Snapshot { global } => snapshot(out, &current_manager()?, global),
        #[cfg(unix)]
        Request::SetEnabled {
            global,
            name,
            path,
            value,
        } => {
            let manager = current_manager()?;
            let scope = Scope::new(&manager, global)?;
            let skills = catalog::discover(&manager, &[], None)?;
            let changed = mutation::set_enabled(
                &manager,
                &scope,
                &name,
                Some(&path),
                EnabledChange::Set(value),
            )?;
            let (global, project) = if scope.global {
                (changed.layer, Lock::new())
            } else {
                (changed.inherited.unwrap_or_default(), changed.layer)
            };
            reply(out, &selection_view(global, project, scope.global, &skills))
        }
        #[cfg(unix)]
        Request::Toggle { global, name, path } => {
            let manager = current_manager()?;
            let scope = Scope::new(&manager, global)?;
            let changed =
                mutation::set_enabled(&manager, &scope, &name, Some(&path), EnabledChange::Toggle)?;
            reply(out, &changed.enabled.expect("toggle always sets a Boolean"))
        }
        #[cfg(unix)]
        Request::Edit {
            global,
            name,
            path,
            draft,
            digest,
        } => {
            let manager = current_manager()?;
            let scope = Scope::new(&manager, global)?;
            mutation::edit_draft(&manager, &scope, &name, &path, &digest, &draft)?;
            reply(out, &build_snapshot(&manager, scope.global, false)?)
        }
        #[cfg(unix)]
        Request::Relocate { global, name, path } => {
            #[derive(Serialize)]
            #[serde(rename_all = "PascalCase")]
            struct Location {
                skill: String,
                managed: bool,
                #[serde(flatten)]
                snapshot: Snapshot,
            }
            let manager = current_manager()?;
            let scope = Scope::new(&manager, global)?;
            let managed = mutation::relocate(&manager, &scope, &name, &path)?;
            reply(
                out,
                &Location {
                    skill: name,
                    managed,
                    snapshot: build_snapshot(&manager, scope.global, false)?,
                },
            )
        }
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
    #[serde(flatten)]
    selection: SelectionView,
}

#[derive(Serialize)]
#[serde(rename_all = "PascalCase")]
struct SelectionView {
    selected: BTreeMap<String, bool>,
    global_selected: BTreeMap<String, bool>,
    project_selected: BTreeMap<String, bool>,
    global_expressions: BTreeMap<String, String>,
    project_expressions: BTreeMap<String, String>,
}

fn snapshot(out: &mut impl Write, manager: &SkillsManager, global: bool) -> Result<()> {
    reply(out, &build_snapshot(manager, global, true)?)
}

fn build_snapshot(manager: &SkillsManager, global: bool, include_native: bool) -> Result<Snapshot> {
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
    let selection = selection_view(global_lock, project_lock, global, &skills);
    let missing = native::missing_remote(manager, None)?;
    let mut rows = skills
        .into_values()
        .map(|skill| (skill, false))
        .chain(missing.into_iter().map(|skill| (skill, true)))
        .map(|(skill, missing)| Row::from_skill(manager, skill, missing))
        .collect::<Result<Vec<_>>>()?;
    if include_native {
        for skill in catalog::claude_plugins(manager, None, true)?
            .into_iter()
            .chain(native::grok(manager, None)?)
        {
            rows.push(Row::from_skill(manager, skill, false)?);
        }
    }
    Ok(Snapshot {
        skills: rows,
        selection,
    })
}

fn selection_view(
    global_lock: Lock,
    project_lock: Lock,
    global: bool,
    skills: &BTreeMap<String, Skill>,
) -> SelectionView {
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
        for (name, skill) in skills {
            if matches!(skill.source, Source::Project) || skill.metadata.hide {
                selected.entry(name.clone()).or_insert(true);
            }
        }
    }
    SelectionView {
        selected,
        global_selected,
        project_selected,
        global_expressions: expressions(global_lock),
        project_expressions: expressions(project_lock),
    }
}

fn reply(out: &mut impl Write, value: &impl Serialize) -> Result<()> {
    serde_json::to_writer(&mut *out, value)?;
    writeln!(out)?;
    Ok(())
}
