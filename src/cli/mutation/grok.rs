use std::{
    fs::{self, DirBuilder, Metadata},
    os::unix::fs::{DirBuilderExt, PermissionsExt},
    path::Path,
};

use toml_edit::{Array, DocumentMut, Item, Table, Value};

use crate::{SkillsManager, cli::cancel, io_error};

use super::{
    Result, atomic_write_checked, coordination_dir, exclusive_lock, same_identity, symlink_metadata,
};

pub(in crate::cli) fn toggle_grok(manager: &SkillsManager, name: &str) -> Result<bool> {
    if name.is_empty() || name.chars().any(char::is_control) {
        return Err("invalid Grok skill name".into());
    }
    let _guard = exclusive_lock(&coordination_dir(manager).join("grok-config.lock"))?;
    let path = manager.home.join(".grok/config.toml");
    let (previous, bytes) = snapshot(&path)?;
    let mut config = std::str::from_utf8(&bytes)?
        .parse::<DocumentMut>()
        .map_err(|err| crate::invalid_data(&path, err))?;
    if config.get("skills").is_none() {
        config.insert("skills", Item::Table(Table::new()));
    }
    let skills = config["skills"]
        .as_table_like_mut()
        .ok_or("Grok skills must be a TOML table")?;
    if !skills.contains_key("disabled") {
        skills.insert("disabled", Item::Value(Value::Array(Array::new())));
    }
    let disabled = skills
        .get_mut("disabled")
        .and_then(Item::as_array_mut)
        .ok_or("Grok skills.disabled must be an array of strings")?;
    if disabled.iter().any(|item| !item.is_str()) {
        return Err("Grok skills.disabled must be an array of strings".into());
    }
    let enable = disabled.iter().any(|item| item.as_str() == Some(name));
    if enable {
        for index in (0..disabled.len()).rev() {
            if disabled.get(index).and_then(Value::as_str) == Some(name) {
                disabled.remove(index);
            }
        }
    } else {
        disabled.push(name);
    }
    let parent = path.parent().expect("Grok config path has a parent");
    DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(parent)
        .map_err(|err| io_error(parent, err))?;
    let mode = |metadata: &Metadata| metadata.permissions().mode() & 0o777;
    atomic_write_checked(
        &path,
        config.to_string().as_bytes(),
        previous.as_ref().map_or(0o600, mode),
        || {
            cancel::check()?;
            let (current, current_bytes) = snapshot(&path)?;
            let same = match (previous.as_ref(), current.as_ref()) {
                (None, None) => true,
                (Some(a), Some(b)) => {
                    same_identity(a, b) && mode(a) == mode(b) && current_bytes == bytes
                }
                _ => false,
            };
            if !same {
                return Err("Grok config changed while updating".into());
            }
            Ok(())
        },
    )?;
    Ok(enable)
}

fn snapshot(path: &Path) -> Result<(Option<Metadata>, Vec<u8>)> {
    match symlink_metadata(path)? {
        Some(metadata) if metadata.is_file() => Ok((
            Some(metadata),
            fs::read(path).map_err(|err| io_error(path, err))?,
        )),
        Some(_) => Err(format!("Grok config {} is not a regular file", path.display()).into()),
        None => Ok((None, Vec::new())),
    }
}
