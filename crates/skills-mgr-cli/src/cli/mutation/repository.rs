use std::{
    ffi::OsString,
    io::{self, Read, Seek, Write},
    os::unix::process::CommandExt,
    path::{Path, PathBuf},
    process::{Command, Stdio},
    thread,
    time::Duration,
};

use cap_std::fs::{Dir, PermissionsExt};
use percent_encoding::{AsciiSet, NON_ALPHANUMERIC, percent_decode_str, utf8_percent_encode};
use tempfile::TempDir;
use url::Url;

use crate::{
    cli::cancel,
    content::{self, MANIFEST, Metadata},
    io_error,
    remote::{Provider, RemoteRef},
};

use super::{Result, exclusion::Exclusion};

pub(super) const MAX_FILES: usize = 4096;
pub(super) const MAX_BYTES: usize = 64 * 1024 * 1024;
const MAX_MANIFESTS: usize = 1024;
const MAX_MANIFEST_BYTES: u64 = 32 * 1024 * 1024;

pub(super) struct RemoteFile {
    pub path: String,
    pub data: Vec<u8>,
    pub executable: bool,
    pub claude_alias: bool,
}

struct Location {
    clone_url: String,
    locator: String,
    branch: String,
    path: String,
}

impl Location {
    fn parse(address: &str) -> Result<Self> {
        let address = address.trim();
        let address = if address.starts_with("github.com/") {
            format!("https://{address}")
        } else if !address.contains(':')
            && address.bytes().filter(|byte| *byte == b'/').count() == 1
        {
            format!("https://github.com/{address}")
        } else {
            address.to_owned()
        };
        let raw = address
            .strip_prefix("https://")
            .filter(|address| {
                !address.contains(['?', '#', '\\']) && !address.chars().any(char::is_control)
            })
            .ok_or("expected an HTTPS Git repository address or GitHub owner/repo")?;
        let parsed = Url::parse(&address).map_err(|_| "invalid HTTPS Git repository address")?;
        let (authority, raw_path) = raw
            .split_once('/')
            .ok_or("repository address is missing its path")?;
        if authority.is_empty() || authority.contains('@') || parsed.host_str().is_none() {
            return Err("expected an HTTPS Git repository address without credentials".into());
        }
        let github = authority.eq_ignore_ascii_case("github.com");
        let parts = raw_path
            .trim_matches('/')
            .split('/')
            .enumerate()
            .map(|(index, part)| {
                for (offset, byte) in part.bytes().enumerate() {
                    if byte == b'%'
                        && !part
                            .as_bytes()
                            .get(offset + 1..offset + 3)
                            .is_some_and(|suffix| suffix.iter().all(u8::is_ascii_hexdigit))
                    {
                        return Err("repository address has an invalid escape".into());
                    }
                }
                let part = percent_decode_str(part).decode_utf8()?;
                if matches!(part.as_ref(), "" | "." | "..")
                    || part.contains('\\')
                    || part.chars().any(char::is_control)
                    || (part.contains('/') && !(github && index == 3))
                {
                    return Err("repository address has an invalid path".into());
                }
                Ok(part)
            })
            .collect::<Result<Vec<_>>>()?;
        if !github {
            let clone_url = address.trim_end_matches('/').to_owned();
            return Ok(Self {
                locator: clone_url.clone(),
                clone_url,
                branch: String::new(),
                path: String::new(),
            });
        }
        let [owner, repo, suffix @ ..] = parts.as_slice() else {
            return Err("GitHub address must include an owner and repository".into());
        };
        let repo = repo.strip_suffix(".git").unwrap_or(repo);
        if !github_part(owner) || !github_part(repo) {
            return Err("invalid GitHub owner or repository".into());
        }
        let base = format!("https://github.com/{owner}/{repo}");
        let clone_url = format!("{base}.git");
        if suffix.is_empty() {
            return Ok(Self {
                clone_url,
                locator: base,
                branch: String::new(),
                path: String::new(),
            });
        }
        let [kind, branch, directories @ ..] = suffix else {
            return Err("expected a GitHub repository root, /tree/<ref>/<path>, or /blob/<ref>/<path>/SKILL.md".into());
        };
        let directories = match kind.as_ref() {
            "tree" => directories,
            "blob" => match directories.split_last() {
                Some((file, directories)) if file == MANIFEST => directories,
                _ => return Err("GitHub file address must point to SKILL.md".into()),
            },
            _ => return Err("expected a GitHub repository root, /tree/<ref>/<path>, or /blob/<ref>/<path>/SKILL.md".into()),
        };
        if branch.starts_with('-') {
            return Err("invalid Git branch or tag".into());
        }
        const SEGMENT: &AsciiSet = &NON_ALPHANUMERIC
            .remove(b'-')
            .remove(b'_')
            .remove(b'.')
            .remove(b'~')
            .remove(b'$')
            .remove(b'&')
            .remove(b'+')
            .remove(b':')
            .remove(b'=')
            .remove(b'@');
        let escaped = std::iter::once(branch)
            .chain(directories)
            .map(|part| utf8_percent_encode(part, SEGMENT).to_string())
            .collect::<Vec<_>>()
            .join("/");
        Ok(Self {
            clone_url,
            locator: format!("{base}/tree/{escaped}"),
            branch: branch.to_string(),
            path: directories.join("/"),
        })
    }
}

fn github_part(part: &str) -> bool {
    !matches!(part, "" | "." | "..")
        && part
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || b"-_.".contains(&byte))
}

pub(super) fn install_files(
    address: &str,
    name: Option<&str>,
    exclusion: &Exclusion,
    diagnostics: &mut impl Write,
) -> Result<(RemoteRef, Vec<RemoteFile>)> {
    let location = Location::parse(address)?;
    writeln!(diagnostics, "Cloning skill repository...")?;
    let checkout = Checkout::new(&location.clone_url, &location.branch)?;
    writeln!(diagnostics, "Discovering repository skills...")?;
    let (selected, source) = checkout.locate_repository_skill(&location.path, name)?;
    let reference = RemoteRef {
        provider: Provider::Repository,
        id: format!("{}#{selected}", location.locator),
        name: selected,
        locator: location.locator,
    };
    if !reference.valid() {
        return Err("invalid repository skill identity".into());
    }
    Ok((reference, checkout.files(source, exclusion)?))
}

pub(super) fn fetch(reference: &RemoteRef, exclusion: &Exclusion) -> Result<Vec<RemoteFile>> {
    match reference.provider {
        Provider::Repository => {
            let (resolved, files) = install_files(
                &reference.locator,
                Some(&reference.name),
                exclusion,
                &mut io::sink(),
            )?;
            if resolved != *reference {
                return Err("repository skill identity changed".into());
            }
            Ok(files)
        }
        Provider::SkillsSh => {
            let parts: Vec<_> = reference.id.split('/').collect();
            if reference.id != reference.locator
                || parts.len() < 3
                || !parts.iter().all(|part| github_part(part))
            {
                return Err("invalid skills.sh skill identifier".into());
            }
            let checkout = Checkout::new(
                &format!("https://github.com/{}/{}.git", parts[0], parts[1]),
                "",
            )?;
            let path = parts[2..].join("/");
            let prefix = format!("{path}/");
            let explicit = parts.len() > 3
                || checkout
                    .tracked
                    .iter()
                    .any(|file| file.starts_with(&prefix));
            let manifests = || {
                checkout
                    .tracked
                    .iter()
                    .map(String::as_str)
                    .filter(|file| leaf(file) == MANIFEST)
            };
            let mut candidates: Vec<_> = manifests()
                .filter(|file| {
                    if explicit {
                        file.starts_with(&prefix)
                    } else {
                        leaf(parent(file)) == reference.name
                    }
                })
                .collect();
            if !explicit && candidates.is_empty() {
                candidates.extend(manifests());
            }
            let source = checkout.locate_catalog_skill(candidates, &reference.name, true)?;
            checkout.files(source, exclusion)
        }
        Provider::SkillsMp => {
            let location = skills_mp_location(&reference.locator)?;
            let checkout = Checkout::new(&location.clone_url, &location.branch)?;
            let source = if location.path.is_empty() {
                let candidates = checkout.tracked.iter().map(String::as_str).filter(|file| {
                    *file == MANIFEST
                        || matches!(file.split('/').collect::<Vec<_>>().as_slice(),
                            [root, "skills", name, MANIFEST] if root.starts_with('.') && root.len() > 1 && *name == reference.name)
                });
                checkout.locate_catalog_skill(candidates.collect(), &reference.name, false)?
            } else {
                &location.path
            };
            checkout.files(source, exclusion)
        }
    }
}

fn skills_mp_location(locator: &str) -> Result<Location> {
    let invalid = "unsupported SkillsMP GitHub URL";
    let locator = locator.split(['?', '#']).next().unwrap_or_default();
    let (authority, path) = locator
        .strip_prefix("https://")
        .and_then(|value| value.split_once('/'))
        .ok_or(invalid)?;
    let url = Url::parse(locator)?;
    if !url
        .host_str()
        .is_some_and(|host| host.eq_ignore_ascii_case("github.com"))
        || authority.contains('@')
        || path.contains("%2f")
        || path.contains("%2F")
        || path
            .trim_matches('/')
            .split('/')
            .nth(2)
            .is_some_and(|kind| {
                percent_decode_str(kind).decode_utf8().ok().as_deref() != Some("tree")
            })
    {
        return Err(invalid.into());
    }
    Location::parse(&format!("https://github.com/{path}"))
}

struct Checkout {
    // The directory (including its Git data) dies only after the subprocess
    // group has exited or been canceled and the fetched skill has been read.
    _temporary: TempDir,
    base: PathBuf,
    root: Dir,
    tracked: Vec<String>,
}

impl Checkout {
    fn new(repository: &str, branch: &str) -> Result<Self> {
        let temporary = tempfile::Builder::new()
            .prefix("skills-mgr-clone-")
            .tempdir()?;
        let base = temporary.path().join("repository");
        let mut args: Vec<OsString> = ["clone", "--depth", "1", "--single-branch"]
            .into_iter()
            .map(Into::into)
            .collect();
        if !branch.is_empty() {
            args.extend([OsString::from("--branch"), branch.into()]);
        }
        args.extend([
            OsString::from("--"),
            repository.into(),
            base.as_os_str().to_owned(),
        ]);
        git(&args, temporary.path()).map_err(|err| format!("git clone {repository}: {err}"))?;
        let data = git(&["ls-files".into(), "-z".into()], &base)?;
        let text =
            String::from_utf8(data).map_err(|_| "Git repository contains a non-UTF-8 path")?;
        let tracked = text.split_terminator('\0').map(str::to_owned).collect();
        let root = content::open_root(&base)?;
        Ok(Self {
            _temporary: temporary,
            base,
            root,
            tracked,
        })
    }

    fn locate_repository_skill(&self, scope: &str, name: Option<&str>) -> Result<(String, &str)> {
        let prefix = format!("{scope}/");
        let direct = if scope.is_empty() {
            MANIFEST.to_owned()
        } else {
            format!("{prefix}{MANIFEST}")
        };
        let mut choices: Vec<_> = self
            .tracked
            .iter()
            .map(String::as_str)
            .filter(|path| {
                (scope.is_empty() || path.starts_with(&prefix)) && leaf(path) == MANIFEST
            })
            .collect();
        if name.is_none() && choices.contains(&direct.as_str()) {
            choices.retain(|path| *path == direct);
        }
        if choices.len() > MAX_MANIFESTS {
            return Err(
                format!("repository contains more than {MAX_MANIFESTS} skill manifests").into(),
            );
        }
        let mut matches = Vec::new();
        let mut available = Vec::new();
        let mut total = 0;
        for path in choices {
            let Some(metadata) = self.read_manifest(path, &mut total)? else {
                continue;
            };
            let parent = parent(path);
            available.push(format!(
                "{} ({})",
                metadata.name,
                if parent.is_empty() { "." } else { parent }
            ));
            if name.is_none_or(|name| metadata.name == name) {
                if path == direct {
                    return Ok((metadata.name, parent));
                }
                matches.push((metadata.name, parent));
            }
        }
        if matches.len() == 1 {
            return Ok(matches.pop().expect("one match"));
        }
        available.sort();
        Err(format!(
            "repository has {} matching skills; supply a skill name or a skill directory URL; available: {}",
            matches.len(), available.join(", ")
        ).into())
    }

    fn locate_catalog_skill<'a>(
        &self,
        candidates: Vec<&'a str>,
        name: &str,
        first: bool,
    ) -> Result<&'a str> {
        if candidates.len() > MAX_MANIFESTS {
            return Err(
                format!("repository contains more than {MAX_MANIFESTS} skill manifests").into(),
            );
        }
        let mut total = 0;
        let mut found = None;
        for path in candidates {
            if self
                .read_manifest(path, &mut total)?
                .is_some_and(|metadata| metadata.name == name)
            {
                if first {
                    return Ok(parent(path));
                }
                if found.replace(parent(path)).is_some() {
                    return Err(
                        format!("repository contains multiple skills named {name:?}").into(),
                    );
                }
            }
        }
        found.ok_or_else(|| format!("repository does not contain skill {name:?}").into())
    }

    fn read_manifest(&self, path: &str, total: &mut u64) -> Result<Option<Metadata>> {
        cancel::check()?;
        valid_file_path(path)?;
        let info = self
            .root
            .symlink_metadata(path)
            .map_err(|err| io_error(&self.base.join(path), err))?;
        if !info.is_file() {
            return Err(format!("repository skill manifest {path:?} is not a regular file").into());
        }
        *total += info.len();
        if *total > MAX_MANIFEST_BYTES {
            return Err(
                format!("repository skill manifests exceed {MAX_MANIFEST_BYTES} bytes").into(),
            );
        }
        let dir = if parent(path).is_empty() {
            self.root.try_clone()?
        } else {
            self.root.open_dir(parent(path))?
        };
        Ok(content::discover(&dir).map_err(|err| io_error(&self.base.join(path), err))?)
    }

    fn files(&self, skill_path: &str, exclusion: &Exclusion) -> Result<Vec<RemoteFile>> {
        let prefix = format!("{skill_path}/");
        let parts: Vec<_> = skill_path.split('/').collect();
        let only_skill_files = skill_path.is_empty()
            || matches!(parts.as_slice(), [root, "skills", _] if root.starts_with('.') && root.len() > 1);
        let mut files = Vec::new();
        let mut total: usize = 0;
        for path in &self.tracked {
            cancel::check()?;
            let relative = if skill_path.is_empty() {
                path.as_str()
            } else if let Some(relative) = path.strip_prefix(&prefix) {
                relative
            } else {
                continue;
            };
            if only_skill_files
                && relative != MANIFEST
                && !matches!(
                    relative.split_once('/'),
                    Some(("references" | "scripts" | "assets" | "data", _))
                )
                || exclusion.excludes(relative)
            {
                continue;
            }
            valid_file_path(relative)?;
            if files.len() == MAX_FILES {
                return Err(
                    format!("Git repository skill contains more than {MAX_FILES} files").into(),
                );
            }
            let error = |err| io_error(&self.base.join(path), err);
            let info = self.root.symlink_metadata(path).map_err(error)?;
            let max = MAX_BYTES - total;
            let mut data = Vec::new();
            let alias = relative == "CLAUDE.md" && info.is_symlink();
            if alias {
                if self.root.read_link(path).map_err(error)? != Path::new("AGENTS.md") {
                    return Err("CLAUDE.md may only link to AGENTS.md".into());
                }
                data.extend_from_slice(b"AGENTS.md");
            } else if !info.is_file() {
                return Err(
                    format!("Git repository skill contains non-regular path {relative:?}").into(),
                );
            } else if info.len() <= max as u64 {
                content::open_file(&self.root, Path::new(path))
                    .and_then(|file| file.take(max as u64 + 1).read_to_end(&mut data))
                    .map_err(error)?;
            } else {
                return Err(format!("Git repository skill exceeds {MAX_BYTES} bytes").into());
            }
            if data.len() > max {
                return Err(format!("Git repository skill exceeds {MAX_BYTES} bytes").into());
            }
            total += data.len();
            files.push(RemoteFile {
                path: relative.to_owned(),
                data,
                executable: info.permissions().mode() & 0o111 != 0,
                claude_alias: alias,
            });
        }
        Ok(files)
    }
}

fn leaf(path: &str) -> &str {
    path.rsplit_once('/').map_or(path, |(_, leaf)| leaf)
}

fn parent(path: &str) -> &str {
    path.rsplit_once('/').map_or("", |(parent, _)| parent)
}

pub(super) fn valid_file_path(path: &str) -> Result<()> {
    if path.contains('\\') || path.split('/').any(|part| matches!(part, "" | "." | "..")) {
        return Err(format!("unsafe file path {path:?}").into());
    }
    Ok(())
}

// Put Git and its transport helpers in one group. Redirect to anonymous files
// so a blocked child cannot stop cancellation by filling a pipe.
fn git(args: &[OsString], dir: &Path) -> Result<Vec<u8>> {
    cancel::check()?;
    let mut stdout = tempfile::tempfile()?;
    let mut stderr = tempfile::tempfile()?;
    let mut child = Command::new("git")
        .args(["-c", "core.hooksPath=/dev/null"])
        .args(args)
        .current_dir(dir)
        .env("GIT_TERMINAL_PROMPT", "0")
        .stdin(Stdio::null())
        .stdout(stdout.try_clone()?)
        .stderr(stderr.try_clone()?)
        .process_group(0)
        .spawn()?;
    let wait = (|| {
        loop {
            cancel::check()?;
            if let Some(status) = child.try_wait()? {
                return Ok::<_, std::io::Error>(status);
            }
            thread::sleep(Duration::from_millis(25));
        }
    })();
    let status = match wait {
        Ok(status) => status,
        Err(err) => {
            // The PID is also the process group created above. We haven't
            // reaped the leader, so the ID can't be reused for another group.
            unsafe { libc::kill(-(child.id() as libc::pid_t), libc::SIGKILL) };
            let _ = child.wait();
            return Err(err.into());
        }
    };
    if !status.success() {
        stderr.rewind()?;
        let mut message = Vec::new();
        stderr.take(8192).read_to_end(&mut message)?;
        return Err(format!("{status}: {}", String::from_utf8_lossy(&message).trim()).into());
    }
    stdout.rewind()?;
    let mut output = Vec::new();
    stdout.take(MAX_BYTES as u64 + 1).read_to_end(&mut output)?;
    if output.len() > MAX_BYTES {
        return Err("Git checkout file list is too large".into());
    }
    Ok(output)
}
