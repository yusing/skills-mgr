use std::{
    fs::{self, DirBuilder, File},
    io::{self, Read, Write},
    os::unix::fs::DirBuilderExt,
    path::{Path, PathBuf},
};

use serde::Serialize;
use sha2::{Digest, Sha256};

use crate::{
    Error, SkillsManager,
    catalog::Skill,
    content::{self, MANIFEST, Metadata},
    io_error,
    remote::{self, Record, hex_digest},
};

use super::{Result, atomic_write, exclusive_lock};

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
    let mut data = serde_json::to_vec_pretty(value)?;
    data.push(b'\n');
    save_private(path, &data)
}

fn save_private(path: &Path, data: &[u8]) -> Result<()> {
    let dir = path.parent().expect("metadata has a parent");
    DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(dir)
        .map_err(|err| io_error(dir, err))?;
    atomic_write(path, data, 0o600)
}
