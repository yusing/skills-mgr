use std::{
    fmt::Write,
    fs, io,
    io::Read,
    path::{Component, Path, PathBuf},
};

use serde::Deserialize;
use sha2::{Digest, Sha256};

use crate::{Error, Result, SkillsManager, catalog, content::valid_name, invalid_data, io_error};

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct RemoteRef {
    provider: String,
    id: String,
    pub name: String,
    locator: String,
}

impl RemoteRef {
    pub(crate) fn valid(&self) -> bool {
        matches!(
            self.provider.as_str(),
            "skills.sh" | "SkillsMP" | "repository"
        ) && valid_name(&self.name)
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

    fn key(&self) -> String {
        let mut hash = Sha256::new();
        hash.update(self.provider.as_bytes());
        hash.update([0]);
        hash.update(self.id.as_bytes());
        hex_digest(&hash.finalize())
    }
}

fn hex_digest(bytes: &[u8]) -> String {
    let mut hex = String::with_capacity(2 * bytes.len());
    for byte in bytes {
        write!(&mut hex, "{byte:02x}").expect("writing to String cannot fail");
    }
    hex
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Record {
    schema_revision: u32,
    #[serde(flatten)]
    reference: RemoteRef,
    content: PathBuf,
    fetched_at: String,
    #[serde(rename = "exclude")]
    _exclude: Option<Vec<String>>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
struct Override {
    schema_revision: u32,
    disable_model_invocation: bool,
}

pub(crate) struct Entry {
    pub key: String,
    pub root: PathBuf,
    pub hide: Option<bool>,
}

pub(crate) fn entries(manager: &SkillsManager, name: Option<&str>) -> Result<Vec<Entry>> {
    let store = manager.cache.join("skills-mgr/remote-skills");
    let mut records = Vec::new();
    for item in catalog::entries(&store.join("entries"), false)? {
        let path = item.path();
        if path.extension().is_none_or(|ext| ext != "json")
            || item.file_type().map_err(|e| io_error(&path, e))?.is_dir()
        {
            continue;
        }
        let Some(record) = read_metadata::<Record>(&path)? else {
            continue;
        };
        let key = record.reference.key();
        if record.schema_revision != 1
            || !record.reference.valid()
            || record.fetched_at.is_empty()
            || record.fetched_at == "0001-01-01T00:00:00Z"
            || path.file_stem().is_none_or(|name| name != key.as_str())
        {
            return Err(invalid_data(&path, "invalid remote cache identity"));
        }
        if name.is_none_or(|name| name == record.reference.name) {
            records.push((record, key));
        }
    }
    records.sort_by(|(a, _), (b, _)| {
        (&a.reference.provider, &a.reference.id).cmp(&(&b.reference.provider, &b.reference.id))
    });
    let mut discovered = Vec::new();
    for (record, key) in records {
        let Some(root) = content_root(&store, &record.content)? else {
            continue;
        };
        let path = store.join("entries").join(&key).with_extension("override");
        let hide = read_metadata::<Override>(&path)?
            .map(|metadata| {
                if metadata.schema_revision != 1 {
                    return Err(invalid_data(&path, "unsupported remote override revision"));
                }
                Ok(metadata.disable_model_invocation)
            })
            .transpose()?;
        discovered.push(Entry { key, root, hide });
    }
    Ok(discovered)
}

fn content_root(store: &Path, relative: &Path) -> Result<Option<PathBuf>> {
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

pub(crate) struct Patch {
    path: PathBuf,
    data: Vec<u8>,
}

pub(crate) fn patch(manager: &SkillsManager, key: &str) -> Result<Option<Patch>> {
    let path = manager
        .manager_home()
        .join("skills/.remote-patches")
        .join(key)
        .with_extension("patch");
    match fs::symlink_metadata(&path) {
        Ok(metadata) if metadata.is_file() => {}
        Ok(_) => return Err(Error::StaleRemotePatch(path)),
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(io_error(&path, e)),
    }
    let data = fs::read(&path).map_err(|e| io_error(&path, e))?;
    Ok(Some(Patch { path, data }))
}

impl Patch {
    pub(crate) fn apply(&self, original: &[u8]) -> Result<Vec<u8>> {
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
