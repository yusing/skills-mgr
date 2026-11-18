use std::{
    collections::BTreeMap,
    fs, io,
    path::{Path, PathBuf},
};

use serde::Deserialize;

use crate::{
    Error, Result, SkillsManager, catalog::Skill, invalid_data, io_error, remote::RemoteRef,
};

pub(crate) const LOCK_NAME: &str = ".skills-mgr.json";

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Lock {
    schema_revision: u32,
    skills: BTreeMap<String, Entry>,
}

#[derive(Deserialize)]
#[serde(untagged)]
enum Entry {
    Legacy(bool),
    Current(Selection),
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Selection {
    enabled: Option<Enabled>,
    remote: Option<RemoteRef>,
}

#[derive(Deserialize)]
#[serde(untagged)]
enum Enabled {
    Boolean(bool),
    Condition(String),
}

#[derive(Default)]
pub(crate) struct Selections {
    entries: BTreeMap<String, Enabled>,
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
        if !present(path)? {
            return Ok(());
        }
        let bytes = fs::read(path).map_err(|e| io_error(path, e))?;
        let Lock {
            schema_revision,
            skills,
        } = serde_json::from_slice(&bytes).map_err(|e| invalid_data(path, e))?;
        if !(1..=3).contains(&schema_revision) {
            return Err(invalid_data(
                path,
                format!("unsupported selection revision {schema_revision}"),
            ));
        }
        for (name, entry) in skills {
            let invalid = || invalid_data(path, format!("invalid selection for skill {name:?}"));
            let enabled = match entry {
                Entry::Legacy(value) if schema_revision == 1 => Some(Enabled::Boolean(value)),
                Entry::Current(Selection { enabled, remote }) if schema_revision > 1 => {
                    if let Some(remote) = &remote {
                        if remote.name != name || !remote.valid() {
                            return Err(invalid());
                        }
                    } else if enabled.is_none() {
                        return Err(invalid());
                    }
                    if let Some(Enabled::Condition(expr)) = &enabled
                        && (schema_revision != 3 || expr.trim().is_empty())
                    {
                        return Err(invalid());
                    }
                    enabled
                }
                _ => return Err(invalid()),
            };
            if let Some(enabled) = enabled {
                self.entries.insert(name, enabled);
            }
        }
        Ok(())
    }

    pub(crate) fn enabled(&mut self, manager: &SkillsManager, skill: &Skill) -> Result<bool> {
        match self.entries.get(&skill.metadata.name) {
            Some(Enabled::Boolean(value)) => Ok(*value),
            Some(Enabled::Condition(expr)) => {
                let result = self.conditions.evaluate(manager, expr);
                #[cfg(feature = "cli")]
                let result = match result {
                    Ok(None) if manager.shell_conditions => {
                        crate::cli::shell_condition(manager, expr).map(Some)
                    }
                    result => result,
                };
                result
                    .map_err(|source| Error::Condition {
                        skill: skill.metadata.name.clone(),
                        source,
                    })?
                    .ok_or_else(|| Error::UnsupportedCondition(skill.metadata.name.clone()))
            }
            None => Ok(skill.source.is_project() || skill.metadata.hide),
        }
    }
}

pub(crate) fn is_enabled(manager: &SkillsManager) -> Result<bool> {
    Ok(present(&manager.project.join(LOCK_NAME))? || global_path(manager)?.is_some())
}

fn global_path(manager: &SkillsManager) -> Result<Option<PathBuf>> {
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
