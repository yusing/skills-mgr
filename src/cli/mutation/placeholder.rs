use std::{
    io::{self, Read, Write},
    path::{Path, PathBuf},
};

use cap_std::fs::{Dir, OpenOptions, Permissions, PermissionsExt};

use crate::content::{self, MANIFEST, PLACEHOLDER_MARKER, PLACEHOLDER_MARKER_NAME};

use super::{Result, same_directory};

#[derive(PartialEq, Eq)]
struct FileState {
    bytes: Vec<u8>,
    mode: u32,
}

#[derive(PartialEq, Eq)]
struct State {
    missing: Vec<PathBuf>,
    dir_mode: Option<u32>,
    manifest: Option<FileState>,
    marker: Option<FileState>,
}

impl State {
    fn owned(&self) -> bool {
        self.manifest.is_some()
            && self
                .marker
                .as_ref()
                .is_some_and(|file| file.bytes == PLACEHOLDER_MARKER)
    }
}

struct Change {
    dir: PathBuf,
    state: State,
}

pub(super) struct Plan {
    root: Dir,
    base: PathBuf,
    changes: Vec<Change>,
    desired: Option<Vec<u8>>,
}

impl Plan {
    pub(super) fn new(base: &Path, name: &str, description: &str, enabled: bool) -> Result<Self> {
        let root = content::open_root(base)?;
        let desired = enabled
            .then(|| {
                Ok::<_, serde_json::Error>(
                    format!(
                        "---\nname: {}\ndescription: {}\ndisable-model-invocation: true\n---\n",
                        serde_json::to_string(name)?,
                        serde_json::to_string(description)?,
                    )
                    .into_bytes(),
                )
            })
            .transpose()?;
        let mut changes = Vec::new();
        for harness in [".agents", ".claude"] {
            let dir = Path::new(harness).join("skills").join(name);
            let Some(state) = inspect(&root, base, &dir)? else {
                continue;
            };
            if let Some(bytes) = &desired {
                if state.owned() {
                    if state
                        .manifest
                        .as_ref()
                        .is_some_and(|file| file.bytes == *bytes)
                    {
                        continue;
                    }
                } else if state.marker.is_some() || state.manifest.is_some() {
                    return Err(format!(
                        "create skill placeholder {}: path already exists",
                        base.join(&dir).display()
                    )
                    .into());
                }
            } else if !state.owned() {
                continue;
            }
            changes.push(Change { dir, state });
        }
        Ok(Self {
            root,
            base: base.to_owned(),
            changes,
            desired,
        })
    }

    pub(super) fn publish(&self, publish: impl FnOnce() -> Result<()>) -> Result<()> {
        publish_all(std::slice::from_ref(self), publish)
    }

    fn rollback(&self, undo: Vec<Undo<'_>>) -> impl Iterator<Item = String> {
        undo.into_iter().rev().filter_map(|action| {
            let (path, result) = match &action {
                Undo::RemoveDir(path) => (path.as_path(), self.root.remove_dir(path)),
                Undo::RemoveFile(path) => (path.as_path(), self.root.remove_file(path)),
                Undo::RestoreDir(path, mode) => (
                    *path,
                    self.root.create_dir(path).and_then(|()| {
                        self.root
                            .set_permissions(path, Permissions::from_mode(*mode))
                    }),
                ),
                Undo::Restore(path, state, removed) => {
                    (path.as_path(), write(&self.root, path, state, *removed))
                }
            };
            result
                .err()
                .map(|err| format!("{}: {err}", self.base.join(path).display()))
        })
    }

    fn apply<'a>(&'a self, undo: &mut Vec<Undo<'a>>) -> Result<()> {
        for Change { dir, state } in &self.changes {
            let Some(current) = inspect(&self.root, &self.base, dir)? else {
                return Err(format!(
                    "skill placeholder {} changed before publication",
                    self.base.join(dir).display()
                )
                .into());
            };
            // A preceding change can create a harness directory shared by
            // several placeholders in the same transaction.
            if current.manifest != state.manifest
                || current.marker != state.marker
                || current.dir_mode != state.dir_mode
                || !current
                    .missing
                    .iter()
                    .all(|path| state.missing.contains(path))
            {
                return Err(format!(
                    "skill placeholder {} changed before publication",
                    self.base.join(dir).display()
                )
                .into());
            }
            let manifest = dir.join(MANIFEST);
            let marker = dir.join(PLACEHOLDER_MARKER_NAME);
            let error = |err| crate::io_error(&self.base.join(dir), err);
            match (&self.desired, &state.manifest, &state.marker) {
                (Some(bytes), Some(previous), Some(_)) => {
                    let mut file = writable(&self.root, &manifest, false)?;
                    undo.push(Undo::Restore(manifest, previous, false));
                    file.write_all(bytes).map_err(error)?;
                }
                (Some(bytes), None, None) => {
                    for path in current.missing {
                        self.root.create_dir(&path).map_err(error)?;
                        undo.push(Undo::RemoveDir(path));
                    }
                    for (path, data) in [(manifest, bytes.as_slice()), (marker, PLACEHOLDER_MARKER)]
                    {
                        let mut file = writable(&self.root, &path, true).map_err(error)?;
                        undo.push(Undo::RemoveFile(path));
                        file.write_all(data).map_err(error)?;
                    }
                }
                (None, Some(previous), Some(owner)) => {
                    for (path, original) in [(marker, owner), (manifest, previous)] {
                        self.root.remove_file(&path).map_err(error)?;
                        undo.push(Undo::Restore(path, original, true));
                    }
                    // Preserve directories with unrelated sidecars.
                    if self.root.remove_dir(dir).is_ok() {
                        undo.push(Undo::RestoreDir(
                            dir,
                            state.dir_mode.expect("managed directory exists"),
                        ));
                    }
                }
                _ => unreachable!("only managed or vacant placeholders are changed"),
            }
        }
        Ok(())
    }
}

pub(super) fn publish_all(plans: &[Plan], publish: impl FnOnce() -> Result<()>) -> Result<()> {
    let mut undo = Vec::new();
    let applied = plans.iter().try_for_each(|plan| {
        undo.push((plan, Vec::new()));
        plan.apply(&mut undo.last_mut().expect("just pushed a journal").1)
    });
    match applied.and_then(|()| publish()) {
        Ok(()) => Ok(()),
        Err(err) => {
            let rollback: Vec<_> = undo
                .into_iter()
                .rev()
                .flat_map(|(plan, actions)| plan.rollback(actions))
                .collect();
            if rollback.is_empty() {
                Err(err)
            } else {
                Err(format!(
                    "{err}; placeholder rollback failed: {}",
                    rollback.join("; ")
                )
                .into())
            }
        }
    }
}

enum Undo<'a> {
    RemoveDir(PathBuf),
    RemoveFile(PathBuf),
    RestoreDir(&'a Path, u32),
    Restore(PathBuf, &'a FileState, bool),
}

fn writable(root: &Dir, path: &Path, create: bool) -> io::Result<cap_std::fs::File> {
    use cap_std::fs::OpenOptionsExt;
    root.open_with(
        path,
        OpenOptions::new()
            .write(true)
            .create_new(create)
            .truncate(!create)
            .mode(0o644)
            .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK),
    )
}

fn write(root: &Dir, path: &Path, original: &FileState, create: bool) -> io::Result<()> {
    let mut file = writable(root, path, create)?;
    file.set_permissions(Permissions::from_mode(original.mode))?;
    file.write_all(&original.bytes)
}

fn inspect(root: &Dir, base: &Path, dir: &Path) -> Result<Option<State>> {
    let mut missing = Vec::new();
    let mut path = PathBuf::new();
    let mut dir_mode = None;
    for component in dir.components() {
        path.push(component);
        match root.symlink_metadata(&path) {
            Ok(metadata) if metadata.is_dir() => {
                if path == dir {
                    dir_mode = Some(metadata.permissions().mode());
                }
            }
            Ok(metadata) if metadata.is_symlink() && root_alias(base, &path) => return Ok(None),
            Ok(_) => {
                return Err(format!(
                    "skill placeholder path {} is not a real directory",
                    base.join(&path).display()
                )
                .into());
            }
            Err(err) if err.kind() == io::ErrorKind::NotFound => missing.push(path.clone()),
            Err(err) => return Err(crate::io_error(&base.join(&path), err).into()),
        }
    }
    Ok(Some(State {
        missing,
        dir_mode,
        manifest: read(root, base, &dir.join(MANIFEST))?,
        marker: read(root, base, &dir.join(PLACEHOLDER_MARKER_NAME))?,
    }))
}

fn read(root: &Dir, base: &Path, path: &Path) -> Result<Option<FileState>> {
    let metadata = match root.symlink_metadata(path) {
        Ok(metadata) if metadata.is_file() => metadata,
        Ok(_) => {
            return Err(format!(
                "skill placeholder {} is not a regular file",
                base.join(path).display()
            )
            .into());
        }
        Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(err) => return Err(crate::io_error(&base.join(path), err).into()),
    };
    let mut bytes = Vec::new();
    content::open_file(root, path)
        .and_then(|mut file| file.read_to_end(&mut bytes))
        .map_err(|err| crate::io_error(&base.join(path), err))?;
    Ok(Some(FileState {
        bytes,
        mode: metadata.permissions().mode(),
    }))
}

fn root_alias(base: &Path, path: &Path) -> bool {
    let mut components = path.components();
    let harness = components
        .next()
        .expect("placeholder always has a harness")
        .as_os_str();
    let suffix = components.as_path();
    if !suffix.as_os_str().is_empty() && suffix != Path::new("skills") {
        return false;
    }
    for candidate in [".agents", ".claude", ".codex", ".grok"] {
        if harness != candidate
            && same_directory(&base.join(candidate).join(suffix), &base.join(path)).unwrap_or(false)
        {
            return true;
        }
    }
    false
}
