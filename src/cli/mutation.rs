use std::{
    collections::BTreeMap,
    ffi::OsString,
    fs::{self, DirBuilder, File, Metadata, OpenOptions, Permissions, TryLockError},
    io::{self, Write},
    os::unix::fs::{DirBuilderExt, MetadataExt, OpenOptionsExt, PermissionsExt},
    path::{Path, PathBuf},
    thread,
    time::Duration,
};

use serde::Serialize;

use crate::{
    SkillsManager, catalog,
    catalog::Source,
    content::valid_name,
    io_error, remote,
    selection::{self, Enabled, LOCK_NAME, Lock, REVISION, Selection},
};

use super::{Result, current_manager};

mod edit;
mod exclusion;
mod install;
mod placeholder;
mod relocate;
mod repository;
mod store;
mod sync;
mod uninstall;
pub(super) use edit::{edit, edit_draft, toggle_invocation};
pub(super) use install::install;
pub(super) use relocate::{adopt, relocate};
pub(super) use sync::sync;
pub(super) use uninstall::uninstall;

pub(super) fn set(out: &mut impl Write, args: &[OsString]) -> Result<()> {
    let usage = "usage: skills-mgr set [-g] <skill> <true|false|condition|inherit>";
    let (force_global, args) = global_flag(args);
    let [name, value] = args else {
        return Err(usage.into());
    };
    let name = name.to_str().filter(|name| valid_name(name));
    let (Some(name), Some(value)) = (name, value.to_str()) else {
        return Err(usage.into());
    };
    let desired = match value {
        "inherit" => None,
        "true" | "false" => Some(Enabled::Boolean(value == "true")),
        value if value.trim().is_empty() => {
            return Err("selection condition is empty; use inherit to remove the override".into());
        }
        value => Some(Enabled::Condition(value.to_owned())),
    };
    let manager = current_manager()?;
    let scope = Scope::new(&manager, force_global)?;
    set_enabled(&manager, &scope, name, None, EnabledChange::Set(desired))?;
    let layer = if scope.global { "global" } else { "project" };
    writeln!(out, "Updated {name} ({layer} selection)")?;
    Ok(())
}

fn global_flag(args: &[OsString]) -> (bool, &[OsString]) {
    match args {
        [flag, rest @ ..] if flag == "-g" => (true, rest),
        _ => (false, args),
    }
}

pub(super) struct Scope<'a> {
    pub(super) global: bool,
    selection_dir: PathBuf,
    placeholder_base: &'a Path,
}

impl<'a> Scope<'a> {
    pub(super) fn new(manager: &'a SkillsManager, force_global: bool) -> Result<Self> {
        let global = force_global
            || same_directory(&manager.project, &manager.home)?
            || same_directory(&manager.project, &manager.manager_home())?;
        Ok(Self {
            global,
            selection_dir: if global {
                manager.manager_home()
            } else {
                manager.project.clone()
            },
            placeholder_base: if global {
                &manager.home
            } else {
                &manager.project
            },
        })
    }
}

fn same_identity(a: &Metadata, b: &Metadata) -> bool {
    a.dev() == b.dev() && a.ino() == b.ino()
}

fn same_directory(first: &Path, second: &Path) -> Result<bool> {
    if first == second {
        return Ok(true);
    }
    let first = fs::metadata(first).map_err(|err| io_error(first, err))?;
    match fs::metadata(second) {
        Ok(second) => Ok(same_identity(&first, &second)),
        Err(err) if err.kind() == io::ErrorKind::NotFound => Ok(false),
        Err(err) => Err(io_error(second, err).into()),
    }
}

// Match the Go writer's managed-then-per-directory lock order and inode key.
fn coordination_dir(manager: &SkillsManager) -> PathBuf {
    manager.cache.join("skills-mgr/selection-locks")
}

fn exclusive_lock(path: &Path) -> Result<File> {
    let parent = path.parent().expect("coordination files have a parent");
    DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(parent)
        .map_err(|err| io_error(parent, err))?;
    let file = OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .mode(0o600)
        .open(path)
        .map_err(|err| io_error(path, err))?;
    loop {
        super::cancel::check()?;
        match file.try_lock() {
            Ok(()) => return Ok(file),
            Err(TryLockError::WouldBlock) => thread::sleep(Duration::from_millis(25)),
            Err(TryLockError::Error(err)) => return Err(io_error(path, err).into()),
        }
    }
}

fn selection_lock(manager: &SkillsManager, dir: &Path) -> Result<File> {
    fs::create_dir_all(dir).map_err(|err| io_error(dir, err))?;
    let metadata = fs::metadata(dir).map_err(|err| io_error(dir, err))?;
    let name = format!("{:x}-{:x}.lock", metadata.dev(), metadata.ino());
    exclusive_lock(&coordination_dir(manager).join(name))
}

fn symlink_metadata(path: &Path) -> Result<Option<Metadata>> {
    match fs::symlink_metadata(path) {
        Ok(metadata) => Ok(Some(metadata)),
        Err(err) if err.kind() == io::ErrorKind::NotFound => Ok(None),
        Err(err) => Err(io_error(path, err).into()),
    }
}

pub(super) enum EnabledChange {
    Set(Option<Enabled>),
    Toggle,
}

pub(super) struct ChangedSelection {
    pub(super) enabled: Option<bool>,
    pub(super) layer: Lock,
    pub(super) inherited: Option<Lock>,
}

pub(super) fn set_enabled(
    manager: &SkillsManager,
    scope: &Scope<'_>,
    name: &str,
    expected: Option<&Path>,
    change: EnabledChange,
) -> Result<ChangedSelection> {
    if let EnabledChange::Set(Some(Enabled::Condition(value))) = &change {
        if value.trim().is_empty() {
            return Err("selection condition is empty".into());
        }
        brush_parser::Parser::new(value.as_bytes(), &Default::default())
            .parse_program()
            .map_err(|err| format!("invalid selection condition: {err}"))?;
    }
    let _guard = managed_lock(manager)?;
    let skill = catalog::discover(manager, &[], Some(name))?
        .remove(name)
        .ok_or_else(|| format!("skill {name:?} was not discovered"))?;
    if expected.is_some_and(|path| skill.manifest_path() != path || skill.remote_key.is_some()) {
        return Err(format!("skill {name:?} changed ownership since the catalog was read").into());
    }
    let mut write = SelectionWrite::open(manager, scope)?;
    let lock = &mut write.layer;
    let mut entry = lock.remove(name).unwrap_or(Selection {
        enabled: None,
        remote: None,
    });
    let inherited = write
        .global
        .as_ref()
        .and_then(|lock| lock.get(name)?.enabled.as_ref());
    let is_toggle = matches!(&change, EnabledChange::Toggle);
    let desired = match change {
        EnabledChange::Set(value) => value,
        EnabledChange::Toggle => {
            let current = entry.enabled.as_ref().or(inherited);
            let enabled = current.map_or_else(
                || !scope.global && (skill.source.is_project() || skill.metadata.hide),
                wants_placeholder,
            );
            Some(Enabled::Boolean(!enabled))
        }
    };
    let changed = entry.enabled != desired || (is_toggle && entry.remote.is_some());
    entry.enabled = desired;
    if is_toggle {
        entry.remote = None;
    }
    let effective = entry.enabled.as_ref().or(inherited);
    let enabled = effective.map(wants_placeholder);
    let placeholders = if matches!(skill.source, Source::Managed | Source::Remote(_)) {
        Some(placeholder::Plan::new(
            scope.placeholder_base,
            name,
            &skill.metadata.description,
            enabled.unwrap_or(false),
        )?)
    } else {
        None
    };
    if entry.enabled.is_some() || entry.remote.is_some() {
        lock.insert(name.to_owned(), entry);
    }
    let publish = || write.publish(changed);
    match placeholders {
        Some(plan) => plan.publish(publish),
        None => publish(),
    }?;
    Ok(ChangedSelection {
        enabled,
        layer: write.layer,
        inherited: write.global,
    })
}

fn wants_placeholder(value: &Enabled) -> bool {
    *value != Enabled::Boolean(false)
}

fn managed_lock(manager: &SkillsManager) -> Result<File> {
    exclusive_lock(&coordination_dir(manager).join("managed-mutation.lock"))
}

fn load_global(manager: &SkillsManager) -> Result<Option<Lock>> {
    Ok(selection::global_path(manager)?
        .as_deref()
        .map(selection::load)
        .transpose()?)
}

struct SelectionWrite {
    _guard: File,
    path: PathBuf,
    legacy: Option<PathBuf>,
    revision: u32,
    layer: Lock,
    global: Option<Lock>,
}

impl SelectionWrite {
    fn open(manager: &SkillsManager, scope: &Scope<'_>) -> Result<Self> {
        let guard = selection_lock(manager, &scope.selection_dir)?;
        let path = scope.selection_dir.join(LOCK_NAME);
        let legacy = if scope.global && symlink_metadata(&path)?.is_none() {
            let legacy = manager.home.join(LOCK_NAME);
            match symlink_metadata(&legacy)? {
                Some(metadata) if metadata.is_file() => Some(legacy),
                Some(_) => {
                    return Err(format!(
                        "legacy selection {} is not a regular file",
                        legacy.display()
                    )
                    .into());
                }
                None => None,
            }
        } else {
            None
        };
        let (revision, mut layer) =
            selection::load_with_revision(legacy.as_deref().unwrap_or(&path))?;
        let global = if scope.global {
            None
        } else {
            load_global(manager)?
        };
        if revision == 1 {
            migrate_legacy(manager, &mut layer, global.as_ref())?;
        }
        Ok(Self {
            _guard: guard,
            path,
            legacy,
            revision,
            layer,
            global,
        })
    }

    fn publish(&self, changed: bool) -> Result<()> {
        if let Some(legacy) = &self.legacy {
            fs::rename(legacy, &self.path).map_err(|err| io_error(legacy, err))?;
        }
        if (changed || self.revision != REVISION)
            && let Err(err) = save_selection(&self.path, &self.layer)
        {
            if let Some(legacy) = &self.legacy
                && let Err(rollback) = fs::rename(&self.path, legacy)
            {
                return Err(format!("{err}; restore {}: {rollback}", legacy.display()).into());
            }
            return Err(err);
        }
        Ok(())
    }
}

fn migrate_legacy(manager: &SkillsManager, lock: &mut Lock, global: Option<&Lock>) -> Result<()> {
    let mut references = BTreeMap::new();
    for (record, _) in remote::records(&remote::store_path(manager), None)? {
        let reference = record.reference;
        if let Some(previous) = references.insert(reference.name.clone(), reference.clone())
            && previous != reference
        {
            return Err(format!(
                "multiple persisted remote identities for skill {:?}",
                reference.name
            )
            .into());
        }
    }
    for (name, reference) in references {
        if let Some(entry) = lock.get_mut(&name) {
            entry.remote = Some(reference);
        } else if global
            .and_then(|lock| lock.get(&name))
            .is_some_and(|entry| entry.enabled.is_some())
        {
            lock.insert(
                name,
                Selection {
                    enabled: None,
                    remote: Some(reference),
                },
            );
        }
    }
    Ok(())
}

fn save_selection(path: &Path, skills: &Lock) -> Result<()> {
    #[derive(Serialize)]
    struct Document<'a> {
        schema_revision: u32,
        skills: &'a Lock,
    }
    let mut data = serde_json::to_vec_pretty(&Document {
        schema_revision: REVISION,
        skills,
    })?;
    data.push(b'\n');
    atomic_write(path, &data, 0o644)
}

fn atomic_write(path: &Path, bytes: &[u8], mode: u32) -> Result<()> {
    let write = || -> io::Result<()> {
        let mut file = tempfile::Builder::new()
            .prefix(".skills-mgr-")
            .tempfile_in(path.parent().expect("publication path has a parent"))?;
        file.write_all(bytes)?;
        file.as_file()
            .set_permissions(Permissions::from_mode(mode))?;
        file.as_file().sync_all()?;
        file.persist(path).map_err(|err| err.error)?;
        Ok(())
    };
    write().map_err(|err| io_error(path, err).into())
}
