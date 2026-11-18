use std::{
    io::{self, BufRead, BufReader, Read},
    ops::RangeInclusive,
    path::{Component, Path},
};

use cap_std::{
    ambient_authority,
    fs::{Dir, File, OpenOptions},
};
use serde::{Deserialize, Serialize, de::DeserializeOwned};
use serde_saphyr::granit_parser::{Event, Parser};

use crate::{Error, Result, SkillsManager, catalog::Skill, invalid_data, io_error};

pub(crate) const MANIFEST: &str = "SKILL.md";
const MAX_FRONTMATTER: usize = 64 * 1024;

#[derive(Debug, Deserialize)]
pub(crate) struct Metadata {
    pub name: String,
    pub description: String,
    #[serde(default, rename = "disable-model-invocation")]
    pub hide: bool,
}

fn decode_yaml<T: DeserializeOwned>(yaml: &[u8]) -> std::result::Result<T, serde_saphyr::Error> {
    serde_saphyr::from_slice(yaml).or_else(|err| {
        // The Go CLI preserves short and numeric spellings on explicit !!bool
        // tags. Normalize only those scalar tokens, not text or untagged values.
        let canonical = normalize_tagged_bools(yaml).ok_or(err)?;
        serde_saphyr::from_str(&canonical)
    })
}

fn normalize_tagged_bools(yaml: &[u8]) -> Option<String> {
    let yaml = std::str::from_utf8(yaml).ok()?;
    let mut canonical = String::new();
    let mut cursor = 0;
    for event in Parser::new_from_str(yaml) {
        let (Event::Scalar(value, _, _, Some(tag)), span) = event.ok()? else {
            continue;
        };
        if tag.core_suffix() != Some("bool") {
            continue;
        }
        let replacement = match value.as_ref() {
            "t" | "T" | "1" => "true",
            "f" | "F" | "0" => "false",
            _ => continue,
        };
        canonical.push_str(yaml.get(cursor..span.start.byte_offset()?)?);
        canonical.push_str(replacement);
        cursor = span.end.byte_offset()?;
    }
    if cursor == 0 {
        return None;
    }
    canonical.push_str(yaml.get(cursor..)?);
    Some(canonical)
}

pub(crate) fn valid_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 80
        && name != "."
        && name != ".."
        && name
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"._-:".contains(&b))
}

pub(crate) fn split_target(target: &str) -> Result<(&str, &Path)> {
    let (name, relative) = target.split_once('/').unwrap_or((target, MANIFEST));
    let path = Path::new(relative);
    if !valid_name(name) || relative.is_empty() || !safe_relative(path) {
        return Err(Error::InvalidTarget(target.into()));
    }
    Ok((name, path))
}

pub(crate) fn safe_relative(path: &Path) -> bool {
    path.components()
        .all(|part| matches!(part, Component::Normal(_) | Component::CurDir))
}

pub(crate) fn open_root(root: &Path) -> Result<Dir> {
    Dir::open_ambient_dir(root, ambient_authority()).map_err(|e| io_error(root, e))
}

pub(crate) fn open_file(root: &Dir, relative: &Path) -> io::Result<File> {
    if !root.metadata(relative)?.is_file() {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "not a regular file",
        ));
    }
    let mut options = OpenOptions::new();
    options.read(true);
    #[cfg(unix)]
    {
        use cap_std::fs::OpenOptionsExt;
        options.custom_flags(libc::O_NONBLOCK);
    }
    let file = root.open_with(relative, &options)?;
    if !file.metadata()?.is_file() {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "not a regular file",
        ));
    }
    Ok(file)
}

pub(crate) fn discover(root: &Dir) -> io::Result<Option<Metadata>> {
    let file = match open_file(root, Path::new(MANIFEST)) {
        Ok(file) => file,
        Err(e)
            if matches!(
                e.kind(),
                io::ErrorKind::NotFound
                    | io::ErrorKind::PermissionDenied
                    | io::ErrorKind::InvalidInput
                    | io::ErrorKind::NotADirectory
            ) =>
        {
            return Ok(None);
        }
        Err(e) => return Err(e),
    };
    let header = frontmatter(&mut BufReader::with_capacity(1024, file))?;
    Ok(metadata(header))
}

// Discovery ignores unusable manifests, but get distinguishes a malformed file
// from Markdown with no frontmatter.
enum Frontmatter {
    Absent(Vec<u8>),
    Valid(Vec<u8>),
    Malformed,
}

fn frontmatter(input: &mut impl BufRead) -> io::Result<Frontmatter> {
    fn delimiter(line: &[u8]) -> bool {
        let line = line.strip_suffix(b"\n").unwrap_or(line);
        line.strip_suffix(b"\r").unwrap_or(line) == b"---"
    }

    let mut line = Vec::new();
    let mut input = input.take(MAX_FRONTMATTER as u64 + 1);
    input.read_until(b'\n', &mut line)?;
    if !delimiter(line.strip_prefix(b"\xef\xbb\xbf").unwrap_or(&line)) {
        return Ok(Frontmatter::Absent(line));
    }
    let mut yaml = Vec::new();
    loop {
        line.clear();
        if input.read_until(b'\n', &mut line)? == 0 || input.limit() == 0 {
            return Ok(Frontmatter::Malformed);
        }
        if delimiter(&line) {
            return Ok(Frontmatter::Valid(yaml));
        }
        yaml.extend_from_slice(&line);
    }
}

fn metadata(header: Frontmatter) -> Option<Metadata> {
    let Frontmatter::Valid(yaml) = header else {
        return None;
    };
    let mut metadata: Metadata = decode_yaml(&yaml).ok()?;
    metadata.name = metadata.name.trim().to_owned();
    metadata.description = metadata
        .description
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ");
    (valid_name(&metadata.name)
        && !metadata.description.is_empty()
        && !metadata.description.chars().any(char::is_control))
    .then_some(metadata)
}

pub(crate) fn patched_metadata(contents: &[u8]) -> Option<Metadata> {
    metadata(frontmatter(&mut &*contents).ok()?)
}

pub(crate) fn get(
    manager: &SkillsManager,
    skill: &Skill,
    relative: &Path,
    lines: Option<RangeInclusive<usize>>,
) -> Result<Vec<u8>> {
    let root = open_root(&skill.root)?;
    let path = skill.root.join(relative);
    let file = open_file(&root, relative).map_err(|e| io_error(&path, e))?;
    let manifest = relative
        .components()
        .filter(|part| *part != Component::CurDir)
        .eq([Component::Normal(MANIFEST.as_ref())]);

    if let Some(key) = &skill.remote_key
        && manifest
        && let Some(patch) = crate::remote::patch(manager, key)?
    {
        let mut original = Vec::new();
        (&file)
            .read_to_end(&mut original)
            .map_err(|e| io_error(&path, e))?;
        return serve(
            &path,
            &*patch.apply(&original)?,
            true,
            skill.metadata.hide,
            lines,
        );
    }
    serve(
        &path,
        BufReader::new(file),
        manifest,
        skill.metadata.hide,
        lines,
    )
}

fn serve(
    path: &Path,
    mut input: impl BufRead,
    manifest: bool,
    hidden: bool,
    lines: Option<RangeInclusive<usize>>,
) -> Result<Vec<u8>> {
    let mut output = Vec::new();
    let mut prefix = Vec::new();
    if path
        .extension()
        .is_some_and(|ext| ext.eq_ignore_ascii_case("md"))
    {
        match frontmatter(&mut input).map_err(|e| io_error(path, e))? {
            Frontmatter::Absent(first) if !manifest => prefix = first,
            Frontmatter::Valid(yaml) => {
                if manifest && hidden {
                    #[derive(Deserialize, Serialize)]
                    struct InvocationMetadata {
                        name: String,
                        description: String,
                    }
                    let metadata: InvocationMetadata =
                        decode_yaml(&yaml).map_err(|e| invalid_data(path, e))?;
                    let yaml =
                        serde_saphyr::to_string(&metadata).map_err(|e| invalid_data(path, e))?;
                    output.extend_from_slice(b"---\n");
                    output.extend_from_slice(yaml.as_bytes());
                    output.extend_from_slice(b"---\n");
                }
            }
            _ => return Err(invalid_data(path, "invalid Markdown frontmatter")),
        }
    }
    let mut body = prefix.as_slice().chain(input);
    if let Some(lines) = lines {
        let mut line = Vec::new();
        for number in 1..=*lines.end() {
            line.clear();
            if body
                .read_until(b'\n', &mut line)
                .map_err(|e| io_error(path, e))?
                == 0
            {
                if number <= *lines.start() {
                    return Err(Error::LineOutOfBounds(*lines.start()));
                }
                break;
            }
            if number >= *lines.start() {
                output.extend_from_slice(&line);
            }
        }
    } else {
        body.read_to_end(&mut output)
            .map_err(|e| io_error(path, e))?;
    }
    Ok(output)
}
