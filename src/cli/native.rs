use std::{
    collections::HashSet,
    fs, io,
    path::{Path, PathBuf},
    process::Command,
};

use serde::Deserialize;

use crate::{
    Error, SkillsManager,
    catalog::{self, Native, Skill, Source},
    content::Metadata,
    remote,
};

use super::Result;

pub(super) fn resolve(manager: &SkillsManager, name: &str) -> Result<(Skill, bool)> {
    let mut inaccessible = false;
    if let Some((skill, enabled)) = catalog::find_filesystem(manager, name)? {
        if enabled {
            return Ok((skill, false));
        }
        inaccessible = true;
    }
    let mut catalog_error = None;
    for load in [claude_for_resolution, grok_for_resolution] {
        match load(manager, name) {
            Ok(skills) => {
                for skill in skills {
                    if skill.native.as_ref().is_some_and(|native| native.enabled) {
                        if !inaccessible {
                            inaccessible = missing_remote(manager, Some(name))
                                .is_ok_and(|missing| !missing.is_empty());
                        }
                        return Ok((skill, inaccessible));
                    }
                    inaccessible = true;
                }
            }
            Err(error) => {
                catalog_error.get_or_insert(error);
            }
        }
    }
    if inaccessible {
        Err(Error::NotEnabled(name.into()).into())
    } else if let Some(error) = catalog_error {
        Err(error)
    } else {
        Err(Error::NotFound(name.into()).into())
    }
}

fn claude_for_resolution(manager: &SkillsManager, name: &str) -> Result<Vec<Skill>> {
    Ok(catalog::claude_plugins(manager, Some(name), true)?)
}

fn grok_for_resolution(manager: &SkillsManager, name: &str) -> Result<Vec<Skill>> {
    grok(manager, Some(name))
}

pub(super) fn missing_remote(manager: &SkillsManager, name: Option<&str>) -> Result<Vec<Skill>> {
    let store = remote::store_path(manager);
    let mut missing = Vec::new();
    for (record, key) in remote::records(&store, name)? {
        if remote::content_root(&store, &record.content)?.is_some() {
            continue;
        }
        let root = store.join(record.content);
        missing.push(Skill {
            metadata: Metadata { name: record.reference.name, description: "Cached content is missing. Use sync or reinstall to restore it, or u to uninstall.".into(), hide: false },
            entry_path: root.clone(), root,
            remote_key: Some(key), source: Source::Remote(record.reference.provider), native: None,
        });
    }
    Ok(missing)
}

pub(super) fn grok(manager: &SkillsManager, name: Option<&str>) -> Result<Vec<Skill>> {
    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct Inspect {
        skills: Vec<InspectedSkill>,
    }
    #[derive(Deserialize, Default)]
    #[serde(default, rename_all = "camelCase")]
    struct InspectedSkill {
        name: String,
        description: String,
        source: InspectedSource,
        user_invocable: bool,
        vendor: String,
        compatibility_status: String,
    }
    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct InspectedSource {
        #[serde(rename = "type")]
        kind: String,
        path: PathBuf,
    }

    let output = match Command::new("grok")
        .args(["inspect", "--json"])
        .current_dir(&manager.project)
        .output()
    {
        Ok(output) if output.status.success() => output,
        Ok(output) => return Err(format!("run grok inspect --json: {}", output.status).into()),
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(e) => return Err(format!("run grok inspect --json: {e}").into()),
    };
    let inspect: Inspect = serde_json::from_slice(&output.stdout)
        .map_err(|e| format!("decode grok inspect --json: {e}"))?;
    let disabled = load_grok_disabled(&manager.home.join(".grok/config.toml"))?;
    let mut skills = Vec::new();
    for inspected in inspect.skills {
        let source = match inspected.source.kind.as_str() {
            "plugin" => Source::GrokPlugin,
            "bundled" => Source::GrokBundled,
            _ => continue,
        };
        if name.is_some_and(|name| name != inspected.name) {
            continue;
        }
        let manifest = manager.project.join(inspected.source.path);
        let root = manifest.parent().unwrap_or(Path::new(".")).to_path_buf();
        let enabled = !disabled.contains(&inspected.name);
        skills.push(Skill {
            metadata: Metadata {
                name: inspected.name,
                description: inspected.description,
                hide: false,
            },
            entry_path: root.clone(),
            root,
            remote_key: None,
            source,
            native: Some(Native {
                enabled,
                vendor: inspected.vendor,
                user_invocable: inspected.user_invocable,
                compatibility_status: inspected.compatibility_status,
                manifest: Some(manifest),
                ..Native::default()
            }),
        });
    }
    Ok(skills)
}

pub(super) fn load_grok_disabled(path: &Path) -> Result<HashSet<String>> {
    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct Config {
        skills: GrokSkills,
    }
    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct GrokSkills {
        disabled: HashSet<String>,
    }
    let config = match fs::read_to_string(path) {
        Ok(config) => config,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(HashSet::new()),
        Err(e) => return Err(crate::io_error(path, e).into()),
    };
    Ok(toml::from_str::<Config>(&config)
        .map_err(|e| crate::invalid_data(path, e))?
        .skills
        .disabled)
}
