use std::{
    collections::BTreeMap,
    fs, io,
    path::{Path, PathBuf},
};

use serde::{Deserialize, Serialize};

use crate::{
    Error, Result, SkillsManager, catalog::Skill, invalid_data, io_error, remote::RemoteRef,
};

pub(crate) const LOCK_NAME: &str = ".skills-mgr.json";

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawLock {
    schema_revision: u32,
    skills: BTreeMap<String, Entry>,
}

#[derive(Deserialize)]
#[serde(untagged)]
enum Entry {
    Legacy(bool),
    Current(Selection),
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct Selection {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub enabled: Option<Enabled>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub remote: Option<RemoteRef>,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(untagged)]
pub(crate) enum Enabled {
    Boolean(bool),
    Condition(String),
}

pub(crate) type Lock = BTreeMap<String, Selection>;

#[derive(Default)]
pub(crate) struct Selections {
    entries: Lock,
    conditions: crate::condition::Conditions,
}

impl Selections {
    pub(crate) fn load(manager: &SkillsManager) -> Result<Self> {
        let mut selected = Self::default();
        if let Some(global) = global_path(manager)? {
            selected.apply(&global)?;
        }
        selected.apply(&manager.project.join(LOCK_NAME))?;
        Ok(selected)
    }

    fn apply(&mut self, path: &Path) -> Result<()> {
        for (name, selection) in load(path)? {
            use std::collections::btree_map::Entry;
            match self.entries.entry(name) {
                Entry::Vacant(entry) => {
                    entry.insert(selection);
                }
                Entry::Occupied(mut entry) => {
                    let existing = entry.get_mut();
                    if selection.enabled.is_some() {
                        existing.enabled = selection.enabled;
                    }
                    if selection.remote.is_some() {
                        existing.remote = selection.remote;
                    }
                }
            }
        }
        Ok(())
    }

    fn configured_remote(&self, name: &str) -> Option<&Selection> {
        self.entries.get(name).filter(|entry| {
            matches!(
                entry.enabled,
                Some(Enabled::Boolean(true) | Enabled::Condition(_))
            )
        })
    }

    pub(crate) fn may_prefer_remote(&self, name: &str) -> bool {
        self.configured_remote(name).is_some()
    }

    pub(crate) fn prefers_remote(&self, name: &str, key: &str) -> bool {
        self.configured_remote(name).is_some_and(|entry| {
            entry
                .remote
                .as_ref()
                .is_none_or(|remote| remote.key() == key)
        })
    }

    pub(crate) fn enabled(&mut self, manager: &SkillsManager, skill: &Skill) -> Result<bool> {
        evaluate(
            &mut self.conditions,
            manager,
            &skill.metadata.name,
            self.entries
                .get(&skill.metadata.name)
                .and_then(|entry| entry.enabled.as_ref()),
            skill.source.is_project() || skill.metadata.hide,
        )
    }
}

pub(crate) fn load(path: &Path) -> Result<Lock> {
    if !present(path)? {
        return Ok(Lock::new());
    }
    let bytes = fs::read(path).map_err(|e| io_error(path, e))?;
    let RawLock {
        schema_revision,
        skills,
    } = serde_json::from_slice(&bytes).map_err(|e| invalid_data(path, e))?;
    if !(1..=3).contains(&schema_revision) {
        return Err(invalid_data(
            path,
            format!("unsupported selection revision {schema_revision}"),
        ));
    }
    let mut normalized = Lock::new();
    for (name, entry) in skills {
        let invalid = || invalid_data(path, format!("invalid selection for skill {name:?}"));
        let selection = match entry {
            Entry::Legacy(value) if schema_revision == 1 => Selection {
                enabled: Some(Enabled::Boolean(value)),
                remote: None,
            },
            Entry::Current(selection) if schema_revision > 1 => {
                if let Some(remote) = &selection.remote {
                    if remote.name != name || !remote.valid() {
                        return Err(invalid());
                    }
                } else if selection.enabled.is_none() {
                    return Err(invalid());
                }
                if let Some(Enabled::Condition(expr)) = &selection.enabled
                    && (schema_revision != 3 || expr.trim().is_empty())
                {
                    return Err(invalid());
                }
                selection
            }
            _ => return Err(invalid()),
        };
        normalized.insert(name, selection);
    }
    Ok(normalized)
}

pub(crate) fn evaluate(
    conditions: &mut crate::condition::Conditions,
    manager: &SkillsManager,
    skill: &str,
    value: Option<&Enabled>,
    default: bool,
) -> Result<bool> {
    match value {
        Some(Enabled::Boolean(value)) => Ok(*value),
        Some(Enabled::Condition(expr)) => {
            let result = conditions.evaluate(manager, expr);
            #[cfg(feature = "cli")]
            let result = match result {
                Ok(None) if manager.shell_conditions => {
                    crate::cli::shell_condition(manager, expr).map(Some)
                }
                result => result,
            };
            result
                .map_err(|source| Error::Condition {
                    skill: skill.to_owned(),
                    source,
                })?
                .ok_or_else(|| Error::UnsupportedCondition(skill.to_owned()))
        }
        None => Ok(default),
    }
}

pub(crate) fn is_enabled(manager: &SkillsManager) -> Result<bool> {
    Ok(present(&manager.project.join(LOCK_NAME))? || global_path(manager)?.is_some())
}

pub(crate) fn global_path(manager: &SkillsManager) -> Result<Option<PathBuf>> {
    let current = manager.manager_home().join(LOCK_NAME);
    if present(&current)? {
        return Ok(Some(current));
    }
    let legacy = manager.home.join(LOCK_NAME);
    Ok(present(&legacy)?.then_some(legacy))
}

fn present(path: &Path) -> Result<bool> {
    match fs::metadata(path) {
        Ok(metadata) if metadata.is_file() => Ok(true),
        Ok(_) => Err(invalid_data(path, "selection is not a regular file")),
        Err(e) if e.kind() == io::ErrorKind::NotFound && !path.is_symlink() => Ok(false),
        Err(e) => Err(io_error(path, e)),
    }
}
