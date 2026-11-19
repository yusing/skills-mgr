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

pub(super) mod xml;

#[derive(Clone, Copy)]
pub enum Source {
    Project,
    User,
    Managed,
    Native(Harness),
    CodexDirectory,
    Admin,
    Bundled,
    CodexPlugin,
    ClaudePlugin,
    Remote(#[cfg_attr(not(feature = "cli-support"), allow(dead_code))] remote::Provider),
    #[cfg(feature = "cli-support")]
    GrokPlugin,
    #[cfg(feature = "cli-support")]
    GrokBundled,
}

impl Source {
    pub fn is_project(self) -> bool {
        matches!(self, Self::Project)
    }

    fn visible_to(self, harnesses: &[Harness]) -> bool {
        let harness = match self {
            Self::Native(harness) => harness,
            Self::CodexDirectory | Self::Admin | Self::Bundled | Self::CodexPlugin => {
                Harness::Codex
            }
            Self::ClaudePlugin => Harness::Claude,
            #[cfg(feature = "cli-support")]
            Self::GrokPlugin | Self::GrokBundled => Harness::Grok,
            _ => return true,
        };
        harnesses.iter().all(|active| *active == harness)
    }

    #[cfg(feature = "cli-support")]
    pub fn label(self) -> &'static str {
        match self {
            Self::Project => "project",
            Self::User => "user",
            Self::Managed => "managed",
            Self::Native(Harness::Codex) | Self::CodexDirectory => "codex",
            Self::Native(Harness::Claude) => "claude",
            Self::Native(Harness::Grok) => "grok",
            Self::Admin => "admin",
            Self::Bundled => "bundled",
            Self::CodexPlugin => "plugin",
            Self::ClaudePlugin => "claude-plugin",
            Self::GrokPlugin => "grok-plugin",
            Self::GrokBundled => "grok-bundled",
            Self::Remote(provider) => provider.label(),
        }
    }

    #[cfg(feature = "cli-support")]
    pub fn editable(self) -> bool {
        matches!(
            self,
            Self::Project
                | Self::User
                | Self::Managed
                | Self::Native(_)
                | Self::Remote(_)
                | Self::CodexDirectory
        )
    }
}

pub struct Skill {
    pub metadata: Metadata,
    pub root: PathBuf,
    pub remote_key: Option<String>,
    pub source: Source,
    #[cfg(feature = "cli-support")]
    pub entry_path: PathBuf,
    #[cfg(feature = "cli-support")]
    pub native: Option<Native>,
}

#[cfg(feature = "cli-support")]
#[derive(Default)]
pub struct Native {
    pub enabled: bool,
    pub plugin: String,
    pub vendor: String,
    pub user_invocable: bool,
    pub compatibility_status: String,
    pub manifest: Option<PathBuf>,
}

pub(crate) fn list(manager: &SkillsManager, harness: Option<Harness>) -> Result<Vec<ListedSkill>> {
    list_harnesses(manager, harness.as_slice(), Selections::load(manager)?)
}

pub(super) fn list_xml(manager: &SkillsManager, harnesses: &[Harness]) -> Result<String> {
    let listed = list_harnesses(manager, harnesses, Selections::load_for_xml(manager)?)?;
    Ok(xml::Catalog(&listed).to_string())
}

fn list_harnesses(
    manager: &SkillsManager,
    harnesses: &[Harness],
    selected: Selections,
) -> Result<Vec<ListedSkill>> {
    let mut loaded = Some(selected);
    let skills = discover_with_selection(manager, harnesses, None, &mut loaded)?;
    let selected = selections(manager, &mut loaded)?;
    let serves_claude = harnesses.contains(&Harness::Claude) && claude_skill_tool_denied(manager)?;
    let mut visible = std::collections::BTreeSet::new();
    for harness in harnesses.iter().copied() {
        if harness != Harness::Claude || !serves_claude {
            visible.extend(native_visible(manager, Some(harness))?.into_keys());
        }
    }
    let mut listed = BTreeMap::new();

    for (name, skill) in skills {
        if !skill.metadata.hide && !visible.contains(&name) && selected.enabled(manager, &skill)? {
            listed.insert(name, skill.listed(manager)?);
        }
    }
    if serves_claude && harnesses.iter().all(|harness| *harness == Harness::Claude) {
        for skill in claude_plugins(manager, None, false)? {
            let name = skill.metadata.name.clone();
            if !skill.metadata.hide && !listed.contains_key(&name) {
                listed.insert(name, skill.listed(manager)?);
            }
        }
    }
    Ok(listed.into_values().collect())
}

pub(crate) fn find(manager: &SkillsManager, name: &str) -> Result<Skill> {
    let disabled = match find_filesystem(manager, name)? {
        Some((skill, true)) => return Ok(skill),
        Some((_, false)) => true,
        None => false,
    };
    match claude_plugins(manager, Some(name), false) {
        Ok(plugins) => {
            if let Some(plugin) = plugins.into_iter().next() {
                return Ok(plugin);
            }
        }
        Err(err) if manager.strict || !disabled => return Err(err),
        Err(_) => {}
    }
    if disabled {
        Err(Error::NotEnabled(name.into()))
    } else {
        Err(Error::NotFound(name.into()))
    }
}

pub fn find_filesystem(manager: &SkillsManager, name: &str) -> Result<Option<(Skill, bool)>> {
    let mut loaded = None;
    let mut skills = discover_with_selection(manager, &[], Some(name), &mut loaded)?;
    let Some(skill) = skills.remove(name) else {
        return Ok(None);
    };
    let enabled = selections(manager, &mut loaded)?.enabled(manager, &skill)?;
    Ok(Some((skill, enabled)))
}

fn selections<'a>(
    manager: &SkillsManager,
    loaded: &'a mut Option<Selections>,
) -> Result<&'a mut Selections> {
    if loaded.is_none() {
        *loaded = Some(Selections::load(manager)?);
    }
    Ok(loaded.as_mut().unwrap())
}

impl Skill {
    fn listed(self, manager: &SkillsManager) -> Result<ListedSkill> {
        let dir = content::open_root(&self.root)?;
        let description = self
            .patched_description(manager, &dir)?
            .unwrap_or(self.metadata.description);
        let references = listed_references(&self.root, &dir, manager.strict)?;
        Ok(ListedSkill {
            name: self.metadata.name,
            description: summarize_description(&description),
            references,
        })
    }

    fn patched_description(
        &self,
        manager: &SkillsManager,
        dir: &cap_std::fs::Dir,
    ) -> Result<Option<String>> {
        if let Some(key) = &self.remote_key
            && let Some(patch) = remote::patch(manager, key).or_else(ignore_stale)?
        {
            let path = self.root.join(MANIFEST);
            let mut file =
                content::open_file(dir, Path::new(MANIFEST)).map_err(|e| io_error(&path, e))?;
            let mut bytes = Vec::new();
            io::Read::read_to_end(&mut file, &mut bytes).map_err(|e| io_error(&path, e))?;
            if let Some(patched) = patch.apply(&bytes).map(Some).or_else(ignore_stale)?
                && let Some(metadata) = content::patched_metadata(&patched)
            {
                return Ok(Some(metadata.description));
            }
        }
        Ok(None)
    }

    #[cfg(feature = "cli-support")]
    pub fn manifest_path(&self) -> PathBuf {
        self.native
            .as_ref()
            .and_then(|native| native.manifest.clone())
            .unwrap_or_else(|| self.root.join(MANIFEST))
    }

    #[cfg(feature = "cli-support")]
    pub fn description(&self, manager: &SkillsManager) -> Result<std::borrow::Cow<'_, str>> {
        if matches!(self.source, Source::GrokPlugin | Source::GrokBundled) {
            return Ok(self.metadata.description.as_str().into());
        }
        let patched = if self.remote_key.is_some() {
            self.patched_description(manager, &content::open_root(&self.root)?)?
        } else {
            None
        };
        Ok(summarize_description(patched.as_deref().unwrap_or(&self.metadata.description)).into())
    }
}

fn summarize_description(description: &str) -> String {
    description.split_whitespace().collect::<Vec<_>>().join(" ")
}

#[cfg(feature = "cli-support")]
pub fn references(root: &Path, dir: &cap_std::fs::Dir) -> Result<Vec<String>> {
    listed_references(root, dir, false)
}

fn listed_references(root: &Path, dir: &cap_std::fs::Dir, strict: bool) -> Result<Vec<String>> {
    let mut paths = Vec::new();
    reference_files(root, dir, Path::new(""), strict, &mut paths)?;
    paths.sort();
    Ok(paths)
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
    strict: bool,
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
            reference_files(root, dir, &relative, strict, output)?;
        } else if name.ends_with(".md")
            && optional_access(dir.metadata(&relative), strict)
                .map_err(|e| io_error(&root.join(&relative), e))?
                .is_some_and(|info| info.is_file())
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

#[cfg(feature = "cli-support")]
pub fn discover(
    manager: &SkillsManager,
    harnesses: &[Harness],
    name: Option<&str>,
) -> Result<BTreeMap<String, Skill>> {
    discover_with_selection(manager, harnesses, name, &mut None)
}

#[cfg(all(feature = "cli-support", unix))]
pub fn discover_local(manager: &SkillsManager, name: &str) -> Result<bool> {
    discovery_roots(manager, &[], Some(name), &mut None, false)
        .map(|(discovery, _)| !discovery.skills.is_empty())
}

#[cfg(all(feature = "cli-support", unix))]
pub fn user_skills(manager: &SkillsManager) -> Result<BTreeMap<String, Skill>> {
    let mut discovery = Discovery::new(None, manager.strict);
    discovery.root(&manager.home.join(".agents/skills"), Source::User, false)?;
    Ok(discovery.skills)
}

#[cfg(all(feature = "cli-support", unix))]
pub fn managed_skills(manager: &SkillsManager) -> Result<BTreeMap<String, Skill>> {
    let mut discovery = Discovery::new(None, manager.strict);
    discovery.root(
        &manager.manager_home().join("skills"),
        Source::Managed,
        false,
    )?;
    Ok(discovery.skills)
}

fn discover_with_selection(
    manager: &SkillsManager,
    harnesses: &[Harness],
    name: Option<&str>,
    loaded: &mut Option<Selections>,
) -> Result<BTreeMap<String, Skill>> {
    let (mut discovery, found) = discovery_roots(manager, harnesses, name, loaded, true)?;
    if found {
        return Ok(discovery.skills);
    }
    for entry in remote::entries(manager, name)? {
        let prefer = (discovery
            .skills
            .get(&entry.name)
            .is_some_and(|skill| matches!(skill.source, Source::CodexDirectory))
            && selections(manager, loaded)?.prefers_remote(&entry.name, &entry.key))
        .then_some(entry.name.as_str());
        if discovery.add(
            entry.root,
            Source::Remote(entry.provider),
            Some(entry.key),
            entry.hide,
            None,
            prefer,
        )? {
            break;
        }
    }
    Ok(discovery.skills)
}

fn discovery_roots<'a>(
    manager: &SkillsManager,
    harnesses: &[Harness],
    name: Option<&'a str>,
    loaded: &mut Option<Selections>,
    prefer_remote: bool,
) -> Result<(Discovery<'a>, bool)> {
    let mut discovery = Discovery::new(name, manager.strict);
    let project = &manager.project;
    let home = &manager.home;
    for (root, source, system) in [
        (project.join(".agents/skills"), Source::Project, false),
        (home.join(".agents/skills"), Source::User, false),
        (
            manager.manager_home().join("skills"),
            Source::Managed,
            false,
        ),
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
        (project.join(".codex/skills"), Source::CodexDirectory, true),
        (
            manager.codex_home.join("skills"),
            Source::CodexDirectory,
            true,
        ),
    ] {
        if source.visible_to(harnesses) && discovery.root(&root, source, system)? {
            let name = name.expect("root stops only for a named lookup");
            if prefer_remote
                && matches!(discovery.skills[name].source, Source::CodexDirectory)
                && selections(manager, loaded)?.may_prefer_remote(name)
            {
                // Keep the Codex copy if the persisted selection is unavailable.
                break;
            }
            return Ok((discovery, true));
        }
    }
    let codex = Source::Native(Harness::Codex);
    if (name.is_none() || discovery.skills.is_empty())
        && codex.visible_to(harnesses)
        && discovery.codex_extras(manager)?
    {
        return Ok((discovery, true));
    }
    Ok((discovery, false))
}

struct Discovery<'a> {
    strict: bool,
    target: Option<&'a str>,
    skills: BTreeMap<String, Skill>,
}

impl<'a> Discovery<'a> {
    fn new(target: Option<&'a str>, strict: bool) -> Self {
        Self {
            strict,
            target,
            skills: BTreeMap::new(),
        }
    }

    fn root(&mut self, root: &Path, source: Source, system: bool) -> Result<bool> {
        for entry in discovery_entries(root, self.strict)? {
            let path = entry.path();
            let kind = entry.file_type().map_err(|e| io_error(&path, e))?;
            let found = if system && entry.file_name() == ".system" {
                self.root(&path, Source::Bundled, false)?
            } else if (kind.is_dir() || kind.is_symlink())
                && let Some(root) = optional_access(fs::canonicalize(&path), self.strict)
                    .map_err(|e| io_error(&path, e))?
            {
                self.add(root, source, None, None, Some(&path), None)?
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
        if let Some(admin) = &manager.admin_skills
            && self.root(admin, Source::Admin, false)?
        {
            return Ok(true);
        }
        self.plugin_cache(&manager.codex_home.join("plugins/cache"), 0)
    }

    fn plugin_cache(&mut self, root: &Path, depth: usize) -> Result<bool> {
        if root.file_name().is_some_and(|name| name == "skills") {
            return self.root(root, Source::CodexPlugin, false);
        }
        if depth < 10 {
            for entry in discovery_entries(root, self.strict)? {
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
        _entry_path: Option<&Path>,
        preferred_remote: Option<&str>,
    ) -> Result<bool> {
        let dir = match content::open_root(&root) {
            Ok(dir) => dir,
            Err(Error::Io { source, .. })
                if skippable_root(&source)
                    && (!self.strict || source.kind() != io::ErrorKind::PermissionDenied) =>
            {
                return Ok(false);
            }
            Err(err) => return Err(err),
        };
        let marker_path = Path::new(content::PLACEHOLDER_MARKER_NAME);
        if let Some(file) = optional_access(content::open_file(&dir, marker_path), self.strict)
            .map_err(|e| io_error(&root.join(marker_path), e))?
        {
            let mut marker = Vec::new();
            use io::Read;
            if optional_access(
                file.take(content::PLACEHOLDER_MARKER.len() as u64 + 1)
                    .read_to_end(&mut marker),
                self.strict,
            )
            .map_err(|e| io_error(&root.join(marker_path), e))?
            .is_some()
                && marker == content::PLACEHOLDER_MARKER
            {
                return Ok(false);
            }
        }
        let manifest = root.join(MANIFEST);
        let Some(mut metadata) =
            content::discover_manifest(&dir, self.strict).map_err(|e| io_error(&manifest, e))?
        else {
            return Ok(false);
        };
        if self.target.is_some_and(|name| name != metadata.name)
            || self.skills.get(&metadata.name).is_some_and(|existing| {
                !matches!(existing.source, Source::CodexDirectory)
                    || preferred_remote != Some(metadata.name.as_str())
            })
        {
            return Ok(false);
        }
        metadata.hide = hidden.unwrap_or(metadata.hide);
        self.skills.insert(
            metadata.name.clone(),
            Skill {
                metadata,
                #[cfg(feature = "cli-support")]
                entry_path: _entry_path.unwrap_or(&root).to_path_buf(),
                root,
                remote_key,
                source,
                #[cfg(feature = "cli-support")]
                native: None,
            },
        );
        Ok(self.target.is_some())
    }
}

fn native_visible(
    manager: &SkillsManager,
    harness: Option<Harness>,
) -> Result<BTreeMap<String, Skill>> {
    let mut native = Discovery::new(None, manager.strict);
    let project = &manager.project;
    let home = &manager.home;
    match harness {
        Some(Harness::Claude) => {
            native.root(
                &project.join(".claude/skills"),
                Source::Native(Harness::Claude),
                false,
            )?;
            native.root(
                &home.join(".claude/skills"),
                Source::Native(Harness::Claude),
                false,
            )?;
        }
        Some(Harness::Grok) => {
            for kind in [".grok", ".agents"].into_iter().chain(
                (!std::env::var("GROK_CLAUDE_SKILLS_ENABLED")
                    .is_ok_and(|v| v.eq_ignore_ascii_case("false")))
                .then_some(".claude"),
            ) {
                native.root(
                    &project.join(kind).join("skills"),
                    Source::Native(Harness::Grok),
                    false,
                )?;
                native.root(
                    &home.join(kind).join("skills"),
                    Source::Native(Harness::Grok),
                    false,
                )?;
            }
        }
        Some(Harness::Codex) => {
            native.root(
                &project.join(".codex/skills"),
                Source::Native(Harness::Codex),
                true,
            )?;
            native.root(
                &manager.codex_home.join("skills"),
                Source::Native(Harness::Codex),
                true,
            )?;
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

pub fn claude_plugins(
    manager: &SkillsManager,
    name: Option<&str>,
    include_disabled: bool,
) -> Result<Vec<Skill>> {
    #[derive(Deserialize)]
    #[serde(rename_all = "camelCase")]
    struct Settings {
        enabled_plugins: Option<BTreeMap<String, bool>>,
    }

    let claude = manager.home.join(".claude");
    let mut skills = Vec::new();
    let mut seen = std::collections::HashSet::new();
    if let Some(settings) = read_json::<Settings>(&claude.join("settings.json"))?
        && let Some(plugins) =
            read_json::<ClaudePlugins>(&claude.join("plugins/installed_plugins.json"))?
    {
        for (id, installs) in plugins.plugins {
            let enabled = settings
                .enabled_plugins
                .as_ref()
                .and_then(|plugins| plugins.get(&id))
                == Some(&true);
            if !enabled && !include_disabled {
                continue;
            }
            for install in installs {
                let mut discovery = Discovery::new(name, manager.strict);
                discovery.root(
                    &install.install_path.join("skills"),
                    Source::ClaudePlugin,
                    false,
                )?;
                for (_name, skill) in discovery.skills {
                    let key = (id.clone(), skill.root.clone());
                    if !seen.insert(key) {
                        continue;
                    }
                    #[cfg(feature = "cli-support")]
                    let skill = Skill {
                        native: Some(Native {
                            enabled,
                            plugin: id.clone(),
                            ..Native::default()
                        }),
                        ..skill
                    };
                    skills.push(skill);
                }
                if name.is_some() && !skills.is_empty() && !include_disabled {
                    return Ok(skills);
                }
            }
        }
    }
    Ok(skills)
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

fn discovery_entries(root: &Path, strict: bool) -> Result<Vec<fs::DirEntry>> {
    match entries(root, !strict) {
        Err(Error::Io { source, .. }) if source.kind() == io::ErrorKind::NotADirectory => {
            Ok(Vec::new())
        }
        result => result,
    }
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

fn optional_access<T>(result: io::Result<T>, strict: bool) -> io::Result<Option<T>> {
    match result {
        Err(e) if strict && e.kind() == io::ErrorKind::PermissionDenied => Err(e),
        result => Ok(result.ok()),
    }
}

fn skippable_root(error: &io::Error) -> bool {
    matches!(
        error.kind(),
        io::ErrorKind::NotFound | io::ErrorKind::NotADirectory | io::ErrorKind::PermissionDenied
    )
}
