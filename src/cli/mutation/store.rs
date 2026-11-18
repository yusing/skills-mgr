use std::{
    collections::HashSet,
    fs::{self, DirBuilder, File, OpenOptions},
    io::{self, Read, Write},
    os::unix::fs::{DirBuilderExt, OpenOptionsExt, PermissionsExt, symlink},
    path::{Path, PathBuf},
};

use serde::Serialize;
use sha2::{Digest, Sha256};

use crate::{
    Error, SkillsManager,
    catalog::Skill,
    content::{self, MANIFEST, Metadata},
    io_error,
    remote::{self, Record, RemoteRef, hex_digest},
};

use super::exclusion::Exclusion;
use super::repository::{MAX_BYTES, MAX_FILES, RemoteFile, valid_file_path};
use super::{Result, atomic_write, exclusive_lock};

pub(super) struct Prepared {
    dir: tempfile::TempDir,
    pub(super) record: Record,
    pub(super) metadata: Metadata,
}

pub(super) struct Store<'a> {
    manager: &'a SkillsManager,
    pub(super) root: PathBuf,
}

impl<'a> Store<'a> {
    pub(super) fn new(manager: &'a SkillsManager) -> Self {
        Self {
            manager,
            root: remote::store_path(manager),
        }
    }

    pub(super) fn lock(&self) -> Result<File> {
        exclusive_lock(&self.root.join(".lock"))
    }

    pub(super) fn ensure(&self, reference: &RemoteRef) -> Result<Metadata> {
        if !reference.valid() {
            return Err("invalid remote skill reference".into());
        }
        let exclusion = {
            let _guard = self.lock()?;
            let record = self.find(reference)?;
            let exclusion = self.exclusion(reference)?;
            if let Some(metadata) = self.fresh(record.as_ref(), &exclusion)? {
                return Ok(metadata);
            }
            exclusion
        };
        let files = super::repository::fetch(reference, &exclusion)?;
        let prepared = self.prepare(reference.clone(), files, exclusion.patterns.clone())?;
        let _guard = self.lock()?;
        let current = self.find(reference)?;
        let current_exclusion = self.exclusion(reference)?;
        if let Some(metadata) = self.fresh(current.as_ref(), &current_exclusion)? {
            return Ok(metadata);
        }
        if current_exclusion.patterns != exclusion.patterns {
            return Err(
                "remote skill exclusions changed during download; retry the operation".into(),
            );
        }
        let metadata = prepared.metadata.clone();
        self.publish(prepared, current.is_none(), None, || Ok(()))?;
        Ok(metadata)
    }

    fn exclusion(&self, reference: &RemoteRef) -> Result<Exclusion> {
        const LIMIT: u64 = 64 * 1024;
        let path = remote::patch_path(self.manager, &reference.key()).with_extension("exclude");
        let Some(metadata) = super::symlink_metadata(&path)? else {
            return Exclusion::new(Vec::new());
        };
        if !metadata.is_file() {
            return Err(format!(
                "remote skill exclusion {} is not a regular file",
                path.display()
            )
            .into());
        }
        let mut value = String::new();
        File::open(&path)
            .and_then(|file| file.take(LIMIT + 1).read_to_string(&mut value))
            .map_err(|err| io_error(&path, err))?;
        if value.len() as u64 > LIMIT {
            return Err(format!(
                "remote skill exclusion {} exceeds {LIMIT} bytes",
                path.display()
            )
            .into());
        }
        let value = value.strip_suffix('\n').unwrap_or(&value);
        let patterns = if value.is_empty() {
            Vec::new()
        } else {
            value.split('\n').map(str::to_owned).collect()
        };
        Exclusion::new(patterns)
    }

    fn fresh(&self, record: Option<&Record>, exclusion: &Exclusion) -> Result<Option<Metadata>> {
        let Some(record) = record else {
            return Ok(None);
        };
        let Some(root) = remote::content_root(&self.root, &record.content)? else {
            return Ok(None);
        };
        let fetched: jiff::Timestamp = record.fetched_at.parse()?;
        if fetched.duration_until(jiff::Timestamp::now()).as_secs() >= 3 * 60 * 60
            || record.exclude.as_deref().unwrap_or_default() != exclusion.patterns
        {
            return Ok(None);
        }
        let dir = content::open_root(&root)?;
        let metadata = content::discover(&dir)
            .map_err(|err| io_error(&root.join(MANIFEST), err))?
            .filter(|metadata| metadata.name == record.reference.name)
            .ok_or_else(|| {
                format!(
                    "persisted remote skill {:?} has an invalid SKILL.md",
                    record.reference.name
                )
            })?;
        Ok(Some(metadata))
    }

    // Read canonical identities even when their content is no longer cached.
    pub(super) fn find(&self, reference: &RemoteRef) -> Result<Option<Record>> {
        let mut result = None;
        for (record, _) in remote::records(&self.root, None)? {
            if record.reference.provider == reference.provider
                && record.reference.id == reference.id
            {
                if record.reference != *reference {
                    return Err(format!(
                        "remote skill {:?} identity conflicts with persisted reference",
                        reference.name
                    )
                    .into());
                }
                result = Some(record);
            } else if record.reference.name == reference.name {
                return Err(format!(
                    "remote skill name {:?} is already persisted from {}",
                    reference.name,
                    record.reference.provider.label()
                )
                .into());
            }
        }
        Ok(result)
    }

    pub(super) fn prepare(
        &self,
        reference: RemoteRef,
        files: Vec<RemoteFile>,
        patterns: Vec<String>,
    ) -> Result<Prepared> {
        let name = &reference.name;
        if files.is_empty() || files.len() > MAX_FILES {
            return Err(format!(
                "remote skill {name:?} must contain between 1 and {MAX_FILES} files"
            )
            .into());
        }
        let content = self.root.join("content");
        create_private_dir(&content)?;
        let dir = tempfile::Builder::new()
            .prefix(&format!("{}-", reference.key()))
            .tempdir_in(&content)
            .map_err(|err| io_error(&content, err))?;
        let mut seen = HashSet::new();
        let mut alias = false;
        let mut total = 0;
        for file in &files {
            crate::cli::cancel::check()?;
            valid_file_path(&file.path)?;
            if !seen.insert(file.path.as_str()) {
                return Err(format!(
                    "remote skill {name:?} contains duplicate path {:?}",
                    file.path
                )
                .into());
            }
            total += file.data.len();
            if total > MAX_BYTES {
                return Err(format!("remote skill {name:?} exceeds {MAX_BYTES} bytes").into());
            }
            if file.path == content::PLACEHOLDER_MARKER_NAME
                && file.data == content::PLACEHOLDER_MARKER
            {
                return Err(
                    format!("remote skill {name:?} is marked as a manager placeholder").into(),
                );
            }
            if file.claude_alias {
                if file.path != "CLAUDE.md" || file.data != b"AGENTS.md" {
                    return Err("unsupported remote link".into());
                }
                alias = true;
                continue;
            }
            let path = dir.path().join(&file.path);
            create_private_dir(path.parent().expect("content path has a parent"))?;
            let write = || -> io::Result<()> {
                let mut output = OpenOptions::new()
                    .write(true)
                    .create_new(true)
                    .mode(if file.executable { 0o700 } else { 0o600 })
                    .open(&path)?;
                output.write_all(&file.data)?;
                output.sync_all()
            };
            write().map_err(|err| io_error(&path, err))?;
        }
        if alias && !seen.contains("AGENTS.md") {
            return Err(
                format!("remote skill {name:?} CLAUDE.md link is missing AGENTS.md").into(),
            );
        }
        if seen.contains("AGENTS.md") && (!seen.contains("CLAUDE.md") || alias) {
            let path = dir.path().join("CLAUDE.md");
            symlink("AGENTS.md", &path).map_err(|err| io_error(&path, err))?;
        }
        let root = content::open_root(dir.path())?;
        let metadata = content::discover(&root)
            .map_err(|err| io_error(&dir.path().join(MANIFEST), err))?
            .filter(|metadata| metadata.name == *name)
            .ok_or_else(|| format!("remote skill {name:?} is missing a matching valid SKILL.md"))?;
        let relative = dir.path().strip_prefix(&self.root)?.to_owned();
        let record = Record {
            schema_revision: 1,
            reference,
            content: relative,
            fetched_at: jiff::Timestamp::now().to_string(),
            exclude: (!patterns.is_empty()).then_some(patterns),
        };
        Ok(Prepared {
            dir,
            record,
            metadata,
        })
    }

    // The caller holds the store lock. Until the record and its dependent
    // transaction both publish, Prepared owns and cleans up the new tree.
    pub(super) fn publish(
        &self,
        prepared: Prepared,
        first: bool,
        replace_exclusion: Option<&Exclusion>,
        done: impl FnOnce() -> Result<()>,
    ) -> Result<()> {
        let key = prepared.record.reference.key();
        let entries = self.root.join("entries");
        let record = entries.join(&key).with_extension("json");
        let mut changes = FileChanges::default();
        let result = (|| {
            crate::cli::cancel::check()?;
            if first {
                changes.remove(&entries.join(&key).with_extension("override"))?;
            }
            if let Some(exclusion) = replace_exclusion {
                let path = remote::patch_path(self.manager, &key).with_extension("exclude");
                if exclusion.patterns.is_empty() {
                    changes.remove(&path)?;
                } else {
                    changes.write(
                        &path,
                        format!("{}\n", exclusion.patterns.join("\n")).as_bytes(),
                    )?;
                }
            }
            changes.write(&record, &json_bytes(&prepared.record)?)?;
            done()
        })();
        match result {
            Ok(()) => {
                let _ = prepared.dir.keep();
                Ok(())
            }
            Err(err) => Err(changes.rollback(err)),
        }
    }

    // Stage metadata by rename, not by parsing it: uninstall is also the
    // recovery path for missing content and malformed sidecars.
    pub(super) fn remove(&self, record: &Record, done: impl FnOnce() -> Result<()>) -> Result<()> {
        let key = record.reference.key();
        let entry = self.root.join("entries").join(&key);
        let patch = remote::patch_path(self.manager, &key);
        let mut backups = Vec::new();
        let result = (|| {
            crate::cli::cancel::check()?;
            for path in [
                entry.with_extension("override"),
                patch.with_extension("exclude"),
                patch,
                entry.with_extension("json"),
            ] {
                if super::symlink_metadata(&path)?.is_none() {
                    continue;
                }
                let parent = path.parent().expect("remote metadata has a parent");
                let backup = tempfile::Builder::new()
                    .prefix(".remove-")
                    .tempdir_in(parent)
                    .map_err(|err| io_error(parent, err))?;
                fs::rename(&path, backup.path().join("metadata"))
                    .map_err(|err| io_error(&path, err))?;
                backups.push((path, backup));
            }
            done()
        })();
        if let Err(err) = result {
            let mut failures = Vec::new();
            for (path, backup) in backups.into_iter().rev() {
                if let Err(rollback) = fs::rename(backup.path().join("metadata"), &path) {
                    let recovery = backup.keep();
                    failures.push(format!(
                        "restore {} from {}: {rollback}",
                        path.display(),
                        recovery.display()
                    ));
                }
            }
            if !failures.is_empty() {
                return Err(format!("{err}; {}", failures.join("; ")).into());
            }
            return Err(err);
        }
        Ok(())
    }

    fn owner(
        &self,
        skill: &Skill,
        key: &str,
        expected: Option<&Path>,
    ) -> Result<(Record, PathBuf)> {
        let name = skill.metadata.name.as_str();
        let record = remote::record_by_key(&self.root, key)?
            .ok_or_else(|| format!("persisted remote skill {name:?} is missing"))?;
        let content = remote::content_root(&self.root, &record.content)?
            .ok_or_else(|| format!("persisted remote skill {name:?} content is missing"))?;
        if record.reference.name != name
            || content != skill.root
            || expected.is_some_and(|path| content.join(MANIFEST) != path)
        {
            return Err(
                format!("remote skill {name:?} changed ownership since it was read").into(),
            );
        }
        Ok((record, content))
    }

    pub(super) fn save_patch(
        &self,
        skill: &Skill,
        key: &str,
        owner: Option<&Path>,
        expected: Option<&str>,
        edited: &[u8],
        metadata: &Metadata,
    ) -> Result<()> {
        let _guard = self.lock()?;
        let (_, root) = self.owner(skill, key, owner)?;
        let original = read_base(&root)?;
        let layered = self.layered(key, &original)?;
        let name = &skill.metadata.name;
        if let Some(expected) = expected {
            let digest = hex_digest(&Sha256::digest(&layered));
            if !expected.eq_ignore_ascii_case(&digest) {
                return Err(format!(
                    "remote skill {name:?} changed: expected SHA256 {expected}, found {digest}"
                )
                .into());
            }
        }
        let old = content::patched_metadata(&layered)
            .ok_or_else(|| format!("remote skill {name:?} has invalid frontmatter"))?;
        if metadata.name != *name || metadata.hide != old.hide {
            return Err(
                "remote identity and invocation metadata are not changed by content edits".into(),
            );
        }
        crate::cli::cancel::check()?;
        let path = remote::patch_path(self.manager, key);
        if original == edited {
            return match fs::remove_file(&path) {
                Ok(()) => Ok(()),
                Err(err) if err.kind() == io::ErrorKind::NotFound => Ok(()),
                Err(err) => Err(io_error(&path, err).into()),
            };
        }
        let mut patch = Vec::new();
        writeln!(
            patch,
            "# skills-mgr-base-sha256 {}",
            hex_digest(&Sha256::digest(&original))
        )?;
        writeln!(
            patch,
            "# skills-mgr-result-sha256 {}",
            hex_digest(&Sha256::digest(edited))
        )?;
        let diff = diffy::DiffOptions::new()
            .set_original_filename("a/SKILL.md")
            .set_modified_filename("b/SKILL.md")
            .create_patch_bytes(&original, edited);
        diffy::PatchFormatter::new().write_patch_into(&diff, &mut patch)?;
        save_private(&path, &patch)
    }

    fn layered(&self, key: &str, original: &[u8]) -> Result<Vec<u8>> {
        let bytes = match remote::patch(self.manager, key)
            .and_then(|patch| patch.map(|patch| patch.apply(original)).transpose())
        {
            Ok(Some(edited)) => edited,
            Ok(None) | Err(Error::StaleRemotePatch(_)) => original.to_vec(),
            Err(err) => return Err(err.into()),
        };
        Ok(bytes)
    }

    pub(super) fn toggle_invocation(&self, skill: &Skill, key: &str, owner: &Path) -> Result<bool> {
        let _guard = self.lock()?;
        let (_, root) = self.owner(skill, key, Some(owner))?;
        let disabled = !match remote::invocation_override(&self.root, key)? {
            Some(flag) => flag,
            None => {
                content::patched_metadata(&read_base(&root)?)
                    .filter(|meta| meta.name == skill.metadata.name)
                    .ok_or_else(|| {
                        format!(
                            "persisted remote skill {:?} is invalid",
                            skill.metadata.name
                        )
                    })?
                    .hide
            }
        };
        crate::cli::cancel::check()?;
        save_json(
            &self
                .root
                .join("entries")
                .join(key)
                .with_extension("override"),
            &remote::Override {
                schema_revision: 1,
                disable_model_invocation: disabled,
            },
        )?;
        Ok(disabled)
    }
}

fn read_base(root: &Path) -> Result<Vec<u8>> {
    let dir = content::open_root(root)?;
    let mut bytes = Vec::new();
    content::open_file(&dir, Path::new(MANIFEST))
        .and_then(|mut file| file.read_to_end(&mut bytes))
        .map_err(|err| io_error(&root.join(MANIFEST), err))?;
    Ok(bytes)
}

pub(super) fn save_json(path: &Path, value: &impl Serialize) -> Result<()> {
    save_private(path, &json_bytes(value)?)
}

fn json_bytes(value: &impl Serialize) -> Result<Vec<u8>> {
    let mut data = serde_json::to_vec_pretty(value)?;
    data.push(b'\n');
    Ok(data)
}

fn save_private(path: &Path, data: &[u8]) -> Result<()> {
    let dir = path.parent().expect("metadata has a parent");
    create_private_dir(dir)?;
    atomic_write(path, data, 0o600)
}

fn create_private_dir(dir: &Path) -> Result<()> {
    DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(dir)
        .map_err(|err| io_error(dir, err))?;
    Ok(())
}

type SavedFile = (Vec<u8>, u32);

#[derive(Default)]
struct FileChanges(Vec<(PathBuf, Option<SavedFile>)>);

impl FileChanges {
    fn write(&mut self, path: &Path, data: &[u8]) -> Result<()> {
        let previous = read_saved(path)?;
        save_private(path, data)?;
        self.0.push((path.to_owned(), previous));
        Ok(())
    }

    fn remove(&mut self, path: &Path) -> Result<()> {
        let Some(previous) = read_saved(path)? else {
            return Ok(());
        };
        fs::remove_file(path).map_err(|err| io_error(path, err))?;
        self.0.push((path.to_owned(), Some(previous)));
        Ok(())
    }

    fn rollback(self, error: Box<dyn std::error::Error>) -> Box<dyn std::error::Error> {
        let failures: Vec<_> = self
            .0
            .into_iter()
            .rev()
            .filter_map(|(path, previous)| {
                let result = match previous {
                    Some((bytes, mode)) => atomic_write(&path, &bytes, mode),
                    None => fs::remove_file(&path).map_err(|err| io_error(&path, err).into()),
                };
                result.err().map(|err| err.to_string())
            })
            .collect();
        if failures.is_empty() {
            error
        } else {
            format!(
                "{error}; remote metadata rollback failed: {}",
                failures.join("; ")
            )
            .into()
        }
    }
}

fn read_saved(path: &Path) -> Result<Option<SavedFile>> {
    match super::symlink_metadata(path)? {
        Some(metadata) if metadata.is_file() => Ok(Some((
            fs::read(path).map_err(|err| io_error(path, err))?,
            metadata.permissions().mode() & 0o777,
        ))),
        Some(_) => Err(format!("remote metadata {} is not a regular file", path.display()).into()),
        None => Ok(None),
    }
}
