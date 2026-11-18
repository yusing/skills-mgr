use std::{
    collections::BTreeMap,
    fs, io,
    path::{Path, PathBuf},
};

use serde::Deserialize;

use crate::{
    Error, Harness, ListedSkill, Result, SkillsManager,
    content::{self, MANIFEST, Metadata},
    invalid_data, io_error, remote,
    selection::Selections,
};

#[derive(Clone, Copy)]
pub(crate) enum Source {
    Project,
    Shared,
    Native(Harness),
}

impl Source {
    pub(crate) fn is_project(self) -> bool {
        matches!(self, Self::Project)
    }

    fn visible_to(self, harness: Option<Harness>) -> bool {
        !matches!((self, harness), (Self::Native(owner), Some(active)) if owner != active)
    }
}

pub(crate) struct Skill {
    pub metadata: Metadata,
    pub root: PathBuf,
    pub remote_key: Option<String>,
    pub source: Source,
}

pub(crate) fn list(manager: &SkillsManager, harness: Option<Harness>) -> Result<Vec<ListedSkill>> {
    let skills = discover(manager, harness, None)?;
    let mut selected = Selections::load(manager)?;
    let serves_claude = harness == Some(Harness::Claude) && claude_skill_tool_denied(manager)?;
    let visible = if serves_claude {
        BTreeMap::new()
    } else {
        native_visible(manager, harness)?
    };
    let mut listed = BTreeMap::new();

    for (name, skill) in skills {
        if !skill.metadata.hide
            && !visible.contains_key(&name)
            && selected.enabled(manager, &skill)?
        {
            listed.insert(name, skill.listed(manager)?);
        }
    }
    if serves_claude {
        for (name, skill) in claude_plugins(manager, None)? {
            if !skill.metadata.hide && !listed.contains_key(&name) {
                listed.insert(name, skill.listed(manager)?);
            }
        }
    }
    Ok(listed.into_values().collect())
}

pub(crate) fn find(manager: &SkillsManager, name: &str) -> Result<Skill> {
    let mut skills = discover(manager, None, Some(name))?;
    let disabled = match skills.remove(name) {
        Some(skill) if Selections::load(manager)?.enabled(manager, &skill)? => return Ok(skill),
        Some(_) => true,
        None => false,
    };
    match claude_plugins(manager, Some(name)) {
        Ok(mut plugins) => {
            if let Some(plugin) = plugins.remove(name) {
                return Ok(plugin);
            }
        }
        Err(err) if !disabled => return Err(err),
        Err(_) => {}
    }
    if disabled {
        Err(Error::NotEnabled(name.into()))
    } else {
        Err(Error::NotFound(name.into()))
    }
}

impl Skill {
    fn listed(self, manager: &SkillsManager) -> Result<ListedSkill> {
        let dir = content::open_root(&self.root)?;
        let mut description = self.metadata.description;
        if let Some(key) = &self.remote_key
            && let Some(patch) = remote::patch(manager, key).or_else(ignore_stale)?
        {
            let path = self.root.join(MANIFEST);
            let mut file =
                content::open_file(&dir, Path::new(MANIFEST)).map_err(|e| io_error(&path, e))?;
            let mut bytes = Vec::new();
            io::Read::read_to_end(&mut file, &mut bytes).map_err(|e| io_error(&path, e))?;
            if let Some(patched) = patch.apply(&bytes).map(Some).or_else(ignore_stale)?
                && let Some(metadata) = content::patched_metadata(&patched)
            {
                description = metadata.description;
            }
        }
        let mut references = Vec::new();
        reference_files(&self.root, &dir, Path::new(""), &mut references)?;
        references.sort();
        Ok(ListedSkill {
            name: self.metadata.name,
            description,
            references,
        })
    }
}

fn ignore_stale<T>(error: Error) -> Result<Option<T>> {
    match error {
        Error::StaleRemotePatch(_) => Ok(None),
        other => Err(other),
    }
}

fn reference_files(
    root: &Path,
    dir: &cap_std::fs::Dir,
    parent: &Path,
    output: &mut Vec<String>,
) -> Result<()> {
    let top = parent.as_os_str().is_empty();
    let folder = root.join(parent);
    let entries = dir
        .read_dir(if top { Path::new(".") } else { parent })
        .map_err(|e| io_error(&folder, e))?;
    for entry in entries {
        let entry = entry.map_err(|e| io_error(&folder, e))?;
        let suffix = entry.file_name();
        let Some(name) = suffix.to_str() else {
            continue;
        };
        if name.starts_with(['.', '_']) || (top && name == MANIFEST) {
            continue;
        }
        let relative = parent.join(name);
        let kind = entry
            .file_type()
            .map_err(|e| io_error(&root.join(&relative), e))?;
        if kind.is_dir() && (!top || name == "references") {
            reference_files(root, dir, &relative, output)?;
        } else if name.ends_with(".md") && dir.metadata(&relative).is_ok_and(|info| info.is_file())
        {
            output.push(
                relative
                    .to_string_lossy()
                    .replace(std::path::MAIN_SEPARATOR, "/"),
            );
        }
    }
    Ok(())
}

fn discover(
    manager: &SkillsManager,
    harness: Option<Harness>,
    name: Option<&str>,
) -> Result<BTreeMap<String, Skill>> {
    let mut discovery = Discovery::new(name);
    let project = &manager.project;
    let home = &manager.home;
    for (root, source, system) in [
        (project.join(".agents/skills"), Source::Project, false),
        (home.join(".agents/skills"), Source::Shared, false),
        (manager.manager_home().join("skills"), Source::Shared, false),
        (
            project.join(".claude/skills"),
            Source::Native(Harness::Claude),
            false,
        ),
        (
            project.join(".grok/skills"),
            Source::Native(Harness::Grok),
            false,
        ),
        (
            project.join(".codex/skills"),
            Source::Native(Harness::Codex),
            true,
        ),
        (
            manager.codex_home.join("skills"),
            Source::Native(Harness::Codex),
            true,
        ),
    ] {
        if source.visible_to(harness) && discovery.root(&root, source, system)? {
            return Ok(discovery.skills);
        }
    }
    let codex = Source::Native(Harness::Codex);
    if codex.visible_to(harness) && discovery.codex_extras(manager)? {
        return Ok(discovery.skills);
    }
    for entry in remote::entries(manager, name)? {
        if discovery.add(entry.root, Source::Shared, Some(entry.key), entry.hide)? {
            break;
        }
    }
    Ok(discovery.skills)
}

struct Discovery<'a> {
    target: Option<&'a str>,
    skills: BTreeMap<String, Skill>,
}

impl<'a> Discovery<'a> {
    fn new(target: Option<&'a str>) -> Self {
        Self {
            target,
            skills: BTreeMap::new(),
        }
    }

    fn root(&mut self, root: &Path, source: Source, system: bool) -> Result<bool> {
        for entry in entries(root, true)? {
            let path = entry.path();
            let kind = entry.file_type().map_err(|e| io_error(&path, e))?;
            let found = if system && entry.file_name() == ".system" {
                self.root(&path, source, false)?
            } else if (kind.is_dir() || kind.is_symlink())
                && let Ok(root) = fs::canonicalize(&path)
            {
                self.add(root, source, None, None)?
            } else {
                false
            };
            if found {
                return Ok(true);
            }
        }
        Ok(false)
    }

    fn codex_extras(&mut self, manager: &SkillsManager) -> Result<bool> {
        let codex = Source::Native(Harness::Codex);
        if let Some(admin) = &manager.admin_skills
            && self.root(admin, codex, false)?
        {
            return Ok(true);
        }
        self.plugin_cache(&manager.codex_home.join("plugins/cache"), 0)
    }

    fn plugin_cache(&mut self, root: &Path, depth: usize) -> Result<bool> {
        if root.file_name().is_some_and(|name| name == "skills") {
            return self.root(root, Source::Native(Harness::Codex), false);
        }
        if depth < 10 {
            for entry in entries(root, true)? {
                if entry
                    .file_type()
                    .map_err(|e| io_error(&entry.path(), e))?
                    .is_dir()
                    && self.plugin_cache(&entry.path(), depth + 1)?
                {
                    return Ok(true);
                }
            }
        }
        Ok(false)
    }

    fn add(
        &mut self,
        root: PathBuf,
        source: Source,
        remote_key: Option<String>,
        hidden: Option<bool>,
    ) -> Result<bool> {
        let dir = match content::open_root(&root) {
            Ok(dir) => dir,
            Err(Error::Io { source, .. }) if skippable_root(&source) => return Ok(false),
            Err(err) => return Err(err),
        };
        const MARKER: &[u8] = b"skills-mgr remote placeholder\n";
        if let Ok(file) = content::open_file(&dir, Path::new(".skills-mgr-placeholder")) {
            let mut marker = Vec::new();
            use io::Read;
            if file
                .take(MARKER.len() as u64 + 1)
                .read_to_end(&mut marker)
                .is_ok()
                && marker == MARKER
            {
                return Ok(false);
            }
        }
        let manifest = root.join(MANIFEST);
        let Some(mut metadata) = content::discover(&dir).map_err(|e| io_error(&manifest, e))?
        else {
            return Ok(false);
        };
        if self.target.is_some_and(|name| name != metadata.name)
            || self.skills.contains_key(&metadata.name)
        {
            return Ok(false);
        }
        metadata.hide = hidden.unwrap_or(metadata.hide);
        self.skills.insert(
            metadata.name.clone(),
            Skill {
                metadata,
                root,
                remote_key,
                source,
            },
        );
        Ok(self.target.is_some())
    }
}

fn native_visible(
    manager: &SkillsManager,
    harness: Option<Harness>,
) -> Result<BTreeMap<String, Skill>> {
    let mut native = Discovery::new(None);
    let project = &manager.project;
    let home = &manager.home;
    match harness {
        Some(Harness::Claude) => {
            native.root(&project.join(".claude/skills"), Source::Shared, false)?;
            native.root(&home.join(".claude/skills"), Source::Shared, false)?;
        }
        Some(Harness::Grok) => {
            for kind in [".grok", ".agents"].into_iter().chain(
                (!std::env::var("GROK_CLAUDE_SKILLS_ENABLED")
                    .is_ok_and(|v| v.eq_ignore_ascii_case("false")))
                .then_some(".claude"),
            ) {
                native.root(&project.join(kind).join("skills"), Source::Shared, false)?;
                native.root(&home.join(kind).join("skills"), Source::Shared, false)?;
            }
        }
        Some(Harness::Codex) => {
            native.root(&project.join(".codex/skills"), Source::Shared, true)?;
            native.root(&manager.codex_home.join("skills"), Source::Shared, true)?;
            native.codex_extras(manager)?;
        }
        None => {}
    }
    Ok(native.skills)
}

#[derive(Deserialize)]
struct ClaudePlugins {
    #[serde(default)]
    plugins: BTreeMap<String, Vec<ClaudeInstall>>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct ClaudeInstall {
    install_path: PathBuf,
}

fn claude_skill_tool_denied(manager: &SkillsManager) -> Result<bool> {
    #[derive(Deserialize)]
    struct Settings {
        permissions: Option<Permissions>,
    }

    #[derive(Deserialize)]
    struct Permissions {
        deny: Option<Vec<String>>,
    }

    for path in [
        manager.home.join(".claude/settings.json"),
        manager.project.join(".claude/settings.json"),
        manager.project.join(".claude/settings.local.json"),
    ] {
        if read_json::<Settings>(&path)?
            .and_then(|s| s.permissions?.deny)
            .is_some_and(|deny| deny.iter().any(|v| v == "Skill"))
        {
            return Ok(true);
        }
    }
    Ok(false)
}

fn claude_plugins(manager: &SkillsManager, name: Option<&str>) -> Result<BTreeMap<String, Skill>> {
    #[derive(Deserialize)]
    #[serde(rename_all = "camelCase")]
    struct Settings {
        enabled_plugins: Option<BTreeMap<String, bool>>,
    }

    let claude = manager.home.join(".claude");
    let mut discovery = Discovery::new(name);
    if let Some(settings) = read_json::<Settings>(&claude.join("settings.json"))?
        && let Some(plugins) =
            read_json::<ClaudePlugins>(&claude.join("plugins/installed_plugins.json"))?
    {
        for (id, installs) in plugins.plugins {
            if settings
                .enabled_plugins
                .as_ref()
                .and_then(|plugins| plugins.get(&id))
                == Some(&true)
            {
                for install in installs {
                    if discovery.root(
                        &install.install_path.join("skills"),
                        Source::Native(Harness::Claude),
                        false,
                    )? {
                        return Ok(discovery.skills);
                    }
                }
            }
        }
    }
    Ok(discovery.skills)
}

fn read_json<T: serde::de::DeserializeOwned>(path: &Path) -> Result<Option<T>> {
    let data = match fs::read(path) {
        Ok(data) => data,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(io_error(path, e)),
    };
    serde_json::from_slice(&data)
        .map(Some)
        .map_err(|e| invalid_data(path, e))
}

pub(crate) fn entries(root: &Path, skip_unreadable: bool) -> Result<Vec<fs::DirEntry>> {
    let entries = match fs::read_dir(root) {
        Ok(entries) => entries,
        Err(e)
            if e.kind() == io::ErrorKind::NotFound || (skip_unreadable && skippable_root(&e)) =>
        {
            return Ok(Vec::new());
        }
        Err(e) => return Err(io_error(root, e)),
    };
    let mut entries = entries
        .collect::<io::Result<Vec<_>>>()
        .map_err(|e| io_error(root, e))?;
    entries.sort_by_key(|entry| entry.file_name());
    Ok(entries)
}

fn skippable_root(error: &io::Error) -> bool {
    matches!(
        error.kind(),
        io::ErrorKind::NotFound | io::ErrorKind::NotADirectory | io::ErrorKind::PermissionDenied
    )
}
