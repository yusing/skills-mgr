use std::path::Path;

use super::{Frontmatter, MANIFEST, Metadata, Parser, decode_yaml, frontmatter};
use crate::{Result, invalid_data, io_error};
use serde_saphyr::granit_parser::{Event, ScalarStyle};

// Change one scalar or insert one key. Re-encoding the YAML would rewrite
// comments, anchors, unrelated metadata, and possibly line endings.
pub(crate) fn toggled_invocation(data: &[u8]) -> Result<(Vec<u8>, bool)> {
    const KEY: &str = "disable-model-invocation";
    let path = Path::new(MANIFEST);
    let bad = |message: &str| invalid_data(path, message);
    let Frontmatter::Valid(yaml) = frontmatter(&mut &*data).map_err(|err| io_error(path, err))?
    else {
        return Err(bad("invalid frontmatter"));
    };
    let yaml = std::str::from_utf8(&yaml).map_err(|err| invalid_data(path, err))?;
    let metadata: Metadata = decode_yaml(yaml.as_bytes()).map_err(|err| invalid_data(path, err))?;
    let disabled = !metadata.hide;
    let missing = || bad("cannot locate disable-model-invocation in frontmatter");
    let start = data
        .iter()
        .position(|byte| *byte == b'\n')
        .ok_or_else(missing)?
        + 1;
    let end = start + yaml.len();
    let line_ending = if data[..start].ends_with(b"\r\n") {
        "\r\n"
    } else {
        "\n"
    };
    let mut edit = None;
    let mut closing = None;
    let mut depth = 0;
    let mut key = true;
    let mut policy = false;
    let mut indent = 0;
    for event in Parser::new_from_str(yaml) {
        let (event, span) = event.map_err(|err| invalid_data(path, err))?;
        match &event {
            Event::MappingStart(..) if depth == 0 => {
                indent = span.start.col();
            }
            Event::MappingEnd if depth == 1 => {
                closing = span
                    .start
                    .byte_offset()
                    .filter(|index| yaml.as_bytes().get(*index) == Some(&b'}'));
                break;
            }
            _ => {}
        }
        let node = matches!(
            event,
            Event::Scalar(..)
                | Event::Alias(_)
                | Event::MappingStart(..)
                | Event::SequenceStart(..)
        );
        if depth == 1 && node {
            if key {
                policy = matches!(&event, Event::Scalar(value, ..) if value == KEY);
            } else if policy {
                let Event::Scalar(value, style, _, tag) = &event else {
                    return Err(bad("disable-model-invocation must be a boolean value"));
                };
                let tagged = tag
                    .as_ref()
                    .is_some_and(|tag| tag.core_suffix() == Some("bool"));
                if !tagged && *style != ScalarStyle::Plain {
                    return Err(bad("disable-model-invocation must be a boolean value"));
                }
                let offset = span.start.byte_offset().ok_or_else(missing)?;
                let terminal = span.end.byte_offset().ok_or_else(missing)?;
                let token = yaml.get(offset..terminal).ok_or_else(missing)?;
                let scalar = offset + token.find(value.as_ref()).ok_or_else(missing)?;
                let replacement = if tagged {
                    tagged_replacement(value, disabled).ok_or_else(|| {
                        bad("unsupported !!bool value for disable-model-invocation")
                    })?
                } else if value.eq_ignore_ascii_case("true") || value.eq_ignore_ascii_case("false")
                {
                    disabled.to_string()
                } else {
                    return Err(bad("disable-model-invocation must be a boolean value"));
                };
                if edit
                    .replace((scalar, scalar + value.len(), replacement))
                    .is_some()
                {
                    return Err(bad("duplicate disable-model-invocation"));
                }
            }
            key = !key;
        }
        match event {
            Event::MappingStart(..) | Event::SequenceStart(..) => depth += 1,
            Event::MappingEnd | Event::SequenceEnd => depth -= 1,
            _ => {}
        }
    }
    let (from, to, replacement) = match (edit, closing) {
        (Some(edit), _) => edit,
        (None, Some(closing)) => {
            let separator = if yaml[..closing].trim_end().ends_with(',') {
                " "
            } else {
                ", "
            };
            (closing, closing, format!("{separator}{KEY}: {disabled}"))
        }
        (None, None) => (
            yaml.len(),
            yaml.len(),
            format!("{}{KEY}: {disabled}{line_ending}", " ".repeat(indent)),
        ),
    };
    let mut updated = Vec::with_capacity(data.len() + replacement.len());
    updated.extend_from_slice(&data[..start + from]);
    updated.extend_from_slice(replacement.as_bytes());
    updated.extend_from_slice(&data[start + to..]);
    let parsed: Metadata = decode_yaml(&updated[start..end + replacement.len() - (to - from)])
        .map_err(|err| invalid_data(path, err))?;
    if parsed.hide != disabled {
        return Err(bad("could not update disable-model-invocation"));
    }
    Ok((updated, disabled))
}

fn tagged_replacement(original: &str, disabled: bool) -> Option<String> {
    let replacement = match (original.to_ascii_lowercase().as_str(), disabled) {
        ("yes" | "no", true) => "yes",
        ("yes" | "no", false) => "no",
        ("true" | "false", true) => "true",
        ("true" | "false", false) => "false",
        ("t" | "f", true) => "t",
        ("t" | "f", false) => "f",
        ("1" | "0", true) => "1",
        ("1" | "0", false) => "0",
        _ => return None,
    };
    if original == original.to_ascii_uppercase() {
        Some(replacement.to_ascii_uppercase())
    } else if original
        .as_bytes()
        .first()
        .is_some_and(u8::is_ascii_uppercase)
        && original[1..] == original[1..].to_ascii_lowercase()
    {
        Some(format!(
            "{}{}",
            replacement[..1].to_ascii_uppercase(),
            &replacement[1..]
        ))
    } else {
        Some(replacement.into())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn replaces_only_the_policy_scalar_in_supported_yaml_forms() {
        for (before, after, disabled) in [
            (
                "\"disable-model-invocation\": false # retain",
                "\"disable-model-invocation\": true # retain",
                true,
            ),
            (
                "disable-model-invocation: &mode false\r\npolicy-copy: *mode",
                "disable-model-invocation: &mode true\r\npolicy-copy: *mode",
                true,
            ),
            (
                "disable-model-invocation: !!bool NO",
                "disable-model-invocation: !!bool YES",
                true,
            ),
            (
                "disable-model-invocation: !!bool 1",
                "disable-model-invocation: !!bool 0",
                false,
            ),
            (
                "disable-model-invocation: !!bool T",
                "disable-model-invocation: !!bool F",
                false,
            ),
        ] {
            let doc = |policy| {
                format!(
                    "\u{feff}---\r\nname: alpha\r\ndescription: Alpha\r\n{policy}\r\n---\r\nbody\r\n"
                )
            };
            assert_eq!(
                toggled_invocation(doc(before).as_bytes()).unwrap(),
                (doc(after).into_bytes(), disabled),
                "{before}"
            );
        }
        let before = b"---\n{name: alpha, description: Alpha} # keep }\n---\nbody\n";
        let after = b"---\n{name: alpha, description: Alpha, disable-model-invocation: true} # keep }\n---\nbody\n";
        assert_eq!(toggled_invocation(before).unwrap(), (after.to_vec(), true));
        let alias = b"---\nname: alpha\ndescription: Alpha\nother: &mode false\ndisable-model-invocation: *mode\n---\nbody\n";
        assert!(toggled_invocation(alias).is_err());
    }
}
