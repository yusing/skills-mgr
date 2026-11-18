use std::{
    ffi::OsString,
    fs::{self, File, OpenOptions},
    io::{self, Read, Write},
    os::unix::fs::{OpenOptionsExt, PermissionsExt},
    path::{Path, PathBuf},
};

use sha2::{Digest, Sha256};

use crate::{
    SkillsManager, catalog,
    catalog::{Skill, Source},
    content::{self, Metadata},
    io_error,
    remote::hex_digest,
    selection::{Enabled, Lock, Selection},
};

use super::{
    Result, Scope, SelectionWrite, atomic_write, coordination_dir, current_manager, exclusive_lock,
    global_flag, managed_lock, placeholder, same_identity, wants_placeholder,
};

pub(in crate::cli) fn edit(out: &mut impl Write, args: &[OsString]) -> Result<()> {
    let usage = "usage: skills-mgr edit [-g] <skill> --file <path|-> [--expect-sha256 <digest>]";
    let (global, args) = global_flag(args);
    let [name, flag, input, options @ ..] = args else {
        return Err(usage.into());
    };
    let name = name
        .to_str()
        .filter(|name| content::valid_name(name))
        .ok_or(usage)?;
    if flag != "--file" {
        return Err(usage.into());
    }
    let expected = match options {
        [] => None,
        [flag, digest] if flag == "--expect-sha256" => {
            let digest = digest
                .to_str()
                .filter(|value| {
                    value.len() == 64 && value.as_bytes().iter().all(u8::is_ascii_hexdigit)
                })
                .ok_or("expected SHA256 must have 64 hexadecimal digits")?;
            Some(digest)
        }
        _ => return Err(usage.into()),
    };
    let contents = if input == "-" {
        let mut contents = Vec::new();
        io::stdin().lock().read_to_end(&mut contents)?;
        contents
    } else {
        read_edit_file(Path::new(input))?
    };
    let metadata = content::edited_metadata(&contents)?;
    let manager = current_manager()?;
    let scope = Scope::new(&manager, global)?;
    edit_local(&manager, &scope, name, None, expected, &contents, &metadata)?;
    writeln!(out, "Updated {}", metadata.name)?;
    Ok(())
}

pub(in crate::cli) fn edit_draft(
    manager: &SkillsManager,
    scope: &Scope<'_>,
    name: &str,
    owner: &Path,
    expected: &str,
    draft: &Path,
) -> Result<()> {
    let contents = read_edit_file(draft)?;
    let metadata = content::edited_metadata(&contents)?;
    edit_local(
        manager,
        scope,
        name,
        Some(owner),
        Some(expected),
        &contents,
        &metadata,
    )
}

fn read_edit_file(path: &Path) -> Result<Vec<u8>> {
    let mut file = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NONBLOCK)
        .open(path)
        .map_err(|err| io_error(path, err))?;
    if !file
        .metadata()
        .map_err(|err| io_error(path, err))?
        .is_file()
    {
        return Err("edit input is not a regular file".into());
    }
    let mut contents = Vec::new();
    file.read_to_end(&mut contents)
        .map_err(|err| io_error(path, err))?;
    Ok(contents)
}

fn edit_local(
    manager: &SkillsManager,
    scope: &Scope<'_>,
    name: &str,
    owner: Option<&Path>,
    expected: Option<&str>,
    contents: &[u8],
    metadata: &Metadata,
) -> Result<()> {
    let _guard = managed_lock(manager)?;
    let skill = catalog::discover(manager, &[], Some(name))?
        .remove(name)
        .ok_or_else(|| format!("skill {name:?} was not discovered"))?;
    if skill.remote_key.is_some() {
        return Err("remote manifest edits are not yet handled by the Rust command".into());
    }
    let manifest = LocalManifest::open(manager, &skill, owner)?;
    if let Some(expected) = expected {
        let digest = hex_digest(&Sha256::digest(&manifest.original));
        if !expected.eq_ignore_ascii_case(&digest) {
            return Err(format!(
                "skill {name:?} changed: expected SHA256 {expected}, found {digest}"
            )
            .into());
        }
    }
    let renamed = name != metadata.name;
    if renamed
        && let Some(owner) =
            catalog::discover(manager, &[], Some(&metadata.name))?.remove(&metadata.name)
    {
        return Err(format!(
            "skill name {:?} is already owned by {}",
            metadata.name,
            owner.manifest_path().display()
        )
        .into());
    }
    let mut selection = SelectionWrite::open(manager, scope)?;
    let changed = renamed && rename_selection(&mut selection, name, &metadata.name)?;
    let plans = if matches!(skill.source, Source::Managed) {
        edited_placeholders(manager, scope, name, metadata, &selection)?
    } else {
        Vec::new()
    };
    placeholder::publish_all(&plans, || {
        manifest.replace(contents)?;
        if let Err(err) = selection.publish(changed) {
            let rollback = (|| {
                if fs::read(&manifest.path).map_err(|err| io_error(&manifest.path, err))?
                    != contents
                {
                    return Err("source changed during edit rollback".into());
                }
                atomic_write(&manifest.path, &manifest.original, manifest.mode())
            })();
            return match rollback {
                Ok(()) => Err(err),
                Err(rollback) => Err(format!("{err}; {rollback}").into()),
            };
        }
        Ok(())
    })
}

pub(in crate::cli) fn toggle_invocation(
    manager: &SkillsManager,
    name: &str,
    owner: &Path,
) -> Result<bool> {
    let _guard = managed_lock(manager)?;
    let skill = catalog::discover(manager, &[], Some(name))?
        .remove(name)
        .ok_or_else(|| format!("skill {name:?} was not discovered"))?;
    if skill.remote_key.is_some() {
        return Err(format!("skill {name:?} changed ownership since the catalog was read").into());
    }
    let manifest = LocalManifest::open(manager, &skill, Some(owner))?;
    let (updated, disabled) = content::toggled_invocation(&manifest.original)?;
    crate::cli::cancel::check()?;
    manifest.replace(&updated)?;
    Ok(disabled)
}

struct LocalManifest {
    _guard: File,
    path: PathBuf,
    original: Vec<u8>,
    before: fs::Metadata,
}

impl LocalManifest {
    fn open(manager: &SkillsManager, skill: &Skill, owner: Option<&Path>) -> Result<Self> {
        let name = &skill.metadata.name;
        if !skill.source.editable() {
            return Err(format!("skill {name:?} is not editable at its discovered source").into());
        }
        let discovered = skill.manifest_path();
        let path = discovered
            .canonicalize()
            .map_err(|err| io_error(&discovered, err))?;
        if owner.is_some_and(|owner| owner != discovered && owner != path) {
            return Err(
                format!("skill {name:?} changed ownership since the catalog was read").into(),
            );
        }
        if !path.starts_with(&skill.root) {
            return Err(
                format!("skill {name:?} source is outside its discovered directory").into(),
            );
        }
        let key = hex_digest(&Sha256::digest(path.as_os_str().as_encoded_bytes()));
        let guard = exclusive_lock(
            &coordination_dir(manager).join(format!("skill-model-invocation-{key}.lock")),
        )?;
        let before = fs::metadata(&path).map_err(|err| io_error(&path, err))?;
        if !before.is_file() {
            return Err(format!("skill source {} is not a regular file", path.display()).into());
        }
        let original = fs::read(&path).map_err(|err| io_error(&path, err))?;
        Ok(Self {
            _guard: guard,
            path,
            original,
            before,
        })
    }

    fn mode(&self) -> u32 {
        self.before.permissions().mode() & 0o777
    }

    fn replace(&self, contents: &[u8]) -> Result<()> {
        let path = &self.path;
        let current = fs::metadata(path).map_err(|err| io_error(path, err))?;
        if !same_identity(&self.before, &current)
            || self.before.permissions() != current.permissions()
            || fs::read(path).map_err(|err| io_error(path, err))? != self.original
        {
            return Err(format!("skill source {} changed during edit", path.display()).into());
        }
        atomic_write(path, contents, self.mode())
    }
}

fn rename_selection(selection: &mut SelectionWrite, old: &str, new: &str) -> Result<bool> {
    let inherited = selection
        .global
        .as_ref()
        .and_then(|layer| enabled(layer, old));
    let explicit = enabled(&selection.layer, old);
    let Some(old_value) = explicit.or_else(|| inherited.filter(|value| wants_placeholder(value)))
    else {
        return Ok(false);
    };
    let merged = match (old_value, enabled(&selection.layer, new)) {
        (value, None) => value.clone(),
        (Enabled::Boolean(a), Some(Enabled::Boolean(b))) => Enabled::Boolean(*a || *b),
        (_, Some(_)) => {
            return Err(format!("cannot merge enabled values while renaming skill {old:?} to {new:?} when either value is an expression").into());
        }
    };
    let old_value = inherited.is_some().then_some(Enabled::Boolean(false));
    for (name, value) in [(old, old_value), (new, Some(merged))] {
        let mut entry = selection.layer.remove(name).unwrap_or(Selection {
            enabled: None,
            remote: None,
        });
        entry.enabled = value;
        if entry.enabled.is_some() || entry.remote.is_some() {
            selection.layer.insert(name.to_owned(), entry);
        }
    }
    Ok(true)
}

fn enabled<'a>(lock: &'a Lock, name: &str) -> Option<&'a Enabled> {
    lock.get(name)?.enabled.as_ref()
}

fn edited_placeholders(
    manager: &SkillsManager,
    scope: &Scope<'_>,
    old: &str,
    metadata: &Metadata,
    selection: &SelectionWrite,
) -> Result<Vec<placeholder::Plan>> {
    let new = metadata.name.as_str();
    let description = &metadata.description;
    let base = scope.placeholder_base;
    let mut plans = Vec::new();
    if old != new {
        plans.push(placeholder::Plan::new(base, old, description, false)?);
    } else if let Some(global) = &selection.global {
        plans.push(placeholder::Plan::new(
            &manager.home,
            new,
            description,
            enabled(global, new).is_some_and(wants_placeholder),
        )?);
    }
    plans.push(placeholder::Plan::new(
        base,
        new,
        description,
        enabled(&selection.layer, new).is_some_and(wants_placeholder),
    )?);
    Ok(plans)
}
