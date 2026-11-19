use std::{
    fmt::Write,
    fs, io,
    io::Read,
    path::{Component, Path, PathBuf},
};

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::{Error, Result, SkillsManager, catalog, content::valid_name, invalid_data, io_error};

#[derive(Clone, Copy, Deserialize, PartialEq, Eq, Serialize)]
pub enum Provider {
    #[serde(rename = "skills.sh")]
    SkillsSh,
    #[serde(rename = "SkillsMP")]
    SkillsMp,
    #[serde(rename = "repository")]
    Repository,
}

impl Provider {
    pub fn label(self) -> &'static str {
        match self {
            Self::SkillsSh => "skills.sh",
            Self::SkillsMp => "SkillsMP",
            Self::Repository => "repository",
        }
    }
}

#[derive(Clone, Deserialize, PartialEq, Eq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct RemoteRef {
    pub provider: Provider,
    pub id: String,
    pub name: String,
    pub locator: String,
}

impl RemoteRef {
    pub fn valid(&self) -> bool {
        valid_name(&self.name)
            && !self.id.is_empty()
            && self.id.len() <= 1024
            && !self.locator.is_empty()
            && self.locator.len() <= 4096
            && !self
                .id
                .chars()
                .chain(self.locator.chars())
                .any(char::is_control)
    }

    pub fn key(&self) -> String {
        let mut hash = Sha256::new();
        hash.update(self.provider.label().as_bytes());
        hash.update([0]);
        hash.update(self.id.as_bytes());
        hex_digest(&hash.finalize())
    }
}

pub fn hex_digest(bytes: &[u8]) -> String {
    let mut hex = String::with_capacity(2 * bytes.len());
    for byte in bytes {
        write!(&mut hex, "{byte:02x}").expect("writing to String cannot fail");
    }
    hex
}

#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Record {
    pub schema_revision: u32,
    #[serde(flatten)]
    pub reference: RemoteRef,
    pub content: PathBuf,
    pub fetched_at: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub exclude: Option<Vec<String>>,
}

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
pub struct Override {
    pub schema_revision: u32,
    pub disable_model_invocation: bool,
}

pub(crate) struct Entry {
    pub key: String,
    pub name: String,
    pub provider: Provider,
    pub root: PathBuf,
    pub hide: Option<bool>,
}

pub(crate) fn entries(manager: &SkillsManager, name: Option<&str>) -> Result<Vec<Entry>> {
    let store = store_path(manager);
    let mut discovered = Vec::new();
    for (record, key) in records_admitted(&store, name, Some(manager))? {
        manager.admit(&store.join(&record.content))?;
        let Some(root) = content_root(&store, &record.content)? else {
            continue;
        };
        manager.admit(&store.join("entries").join(&key).with_extension("override"))?;
        let hide = invocation_override(&store, &key)?;
        discovered.push(Entry {
            key,
            name: record.reference.name,
            provider: record.reference.provider,
            root,
            hide,
        });
    }
    Ok(discovered)
}

pub fn invocation_override(store: &Path, key: &str) -> Result<Option<bool>> {
    let path = store.join("entries").join(key).with_extension("override");
    read_metadata::<Override>(&path)?
        .map(|metadata| {
            if metadata.schema_revision != 1 {
                return Err(invalid_data(&path, "unsupported remote override revision"));
            }
            Ok(metadata.disable_model_invocation)
        })
        .transpose()
}

pub fn store_path(manager: &SkillsManager) -> PathBuf {
    manager.cache.join("skills-mgr/remote-skills")
}

#[cfg(feature = "cli-support")]
pub fn records(store: &Path, name: Option<&str>) -> Result<Vec<(Record, String)>> {
    records_admitted(store, name, None)
}

fn records_admitted(
    store: &Path,
    name: Option<&str>,
    manager: Option<&SkillsManager>,
) -> Result<Vec<(Record, String)>> {
    if let Some(manager) = manager {
        manager.admit(&store.join("entries"))?;
    }
    let mut records = Vec::new();
    for item in catalog::entries(&store.join("entries"), false)? {
        let path = item.path();
        if path.extension().is_none_or(|ext| ext != "json")
            || item.file_type().map_err(|e| io_error(&path, e))?.is_dir()
        {
            continue;
        }
        if let Some(manager) = manager {
            manager.admit(&path)?;
        }
        let Some((record, key)) = read_record(&path)? else {
            continue;
        };
        if name.is_none_or(|name| name == record.reference.name) {
            records.push((record, key));
        }
    }
    records.sort_by(|(a, _), (b, _)| {
        (a.reference.provider.label(), &a.reference.id)
            .cmp(&(b.reference.provider.label(), &b.reference.id))
    });
    Ok(records)
}

fn read_record(path: &Path) -> Result<Option<(Record, String)>> {
    let Some(record) = read_metadata::<Record>(path)? else {
        return Ok(None);
    };
    let key = record.reference.key();
    if record.schema_revision != 1
        || !record.reference.valid()
        || record.fetched_at.is_empty()
        || record.fetched_at == "0001-01-01T00:00:00Z"
        || path.file_stem().is_none_or(|name| name != key.as_str())
    {
        return Err(invalid_data(path, "invalid remote cache identity"));
    }
    Ok(Some((record, key)))
}

#[cfg(all(feature = "cli-support", unix))]
pub fn record_by_key(store: &Path, key: &str) -> Result<Option<Record>> {
    if key.len() != 64 || !key.bytes().all(|byte| byte.is_ascii_hexdigit()) {
        return Err(invalid_data(store, "invalid remote cache key"));
    }
    Ok(
        read_record(&store.join("entries").join(key).with_extension("json"))?
            .map(|(record, _)| record),
    )
}

pub fn content_root(store: &Path, relative: &Path) -> Result<Option<PathBuf>> {
    let path = store.join(relative);
    let mut parts = relative.components();
    if !matches!((parts.next(), parts.next(), parts.next()),
        (Some(Component::Normal(parent)), Some(Component::Normal(_)), None) if parent == "content")
    {
        return Err(invalid_data(&path, "unsafe remote cache path"));
    }
    match fs::symlink_metadata(&path) {
        Ok(_) => {}
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(io_error(&path, e)),
    }
    let content_store = store.join("content");
    let content_store =
        fs::canonicalize(&content_store).map_err(|e| io_error(&content_store, e))?;
    let root = fs::canonicalize(&path).map_err(|e| io_error(&path, e))?;
    if root == content_store || !root.starts_with(content_store) || !root.is_dir() {
        return Err(invalid_data(&path, "invalid remote cache directory"));
    }
    Ok(Some(root))
}

fn read_metadata<T: serde::de::DeserializeOwned>(path: &Path) -> Result<Option<T>> {
    const LIMIT: u64 = 64 * 1024;
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.is_file() => {}
        Ok(_) => return Err(invalid_data(path, "not a regular file")),
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(io_error(path, e)),
    }
    let file = fs::File::open(path).map_err(|e| io_error(path, e))?;
    let mut bytes = Vec::new();
    file.take(LIMIT + 1)
        .read_to_end(&mut bytes)
        .map_err(|e| io_error(path, e))?;
    if bytes.len() as u64 > LIMIT {
        return Err(invalid_data(
            path,
            "remote cache metadata exceeds size limit",
        ));
    }
    serde_json::from_slice(&bytes)
        .map(Some)
        .map_err(|e| invalid_data(path, e))
}

pub struct Patch {
    path: PathBuf,
    data: Vec<u8>,
}

pub fn patch(manager: &SkillsManager, key: &str) -> Result<Option<Patch>> {
    let path = patch_path(manager, key);
    manager.admit(&path)?;
    match fs::symlink_metadata(&path) {
        Ok(metadata) if metadata.is_file() => {}
        Ok(_) => return Err(Error::StaleRemotePatch(path)),
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(io_error(&path, e)),
    }
    let data = fs::read(&path).map_err(|e| io_error(&path, e))?;
    Ok(Some(Patch { path, data }))
}

pub fn patch_path(manager: &SkillsManager, key: &str) -> PathBuf {
    manager
        .manager_home()
        .join("skills/.remote-patches")
        .join(key)
        .with_extension("patch")
}

impl Patch {
    pub fn apply(&self, original: &[u8]) -> Result<Vec<u8>> {
        self.apply_checked(original)
            .ok_or_else(|| Error::StaleRemotePatch(self.path.clone()))
    }

    fn apply_checked(&self, original: &[u8]) -> Option<Vec<u8>> {
        let mut parts = self.data.splitn(3, |&b| b == b'\n');
        let base = parts.next()?.strip_prefix(b"# skills-mgr-base-sha256 ")?;
        let result = parts.next()?.strip_prefix(b"# skills-mgr-result-sha256 ")?;
        if base == result || base != hex_digest(&Sha256::digest(original)).as_bytes() {
            return None;
        }
        let diff = parts
            .next()?
            .strip_prefix(b"--- a/SKILL.md\n+++ b/SKILL.md\n")?;
        let patch = diffy::Patch::from_bytes(diff).ok()?;
        if patch.hunks().is_empty() {
            return None;
        }
        let patched = diffy::apply_bytes(original, &patch).ok()?;
        (result == hex_digest(&Sha256::digest(&patched)).as_bytes()).then_some(patched)
    }
}
