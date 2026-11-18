use std::{collections::HashMap, fmt};

use crate::ListedSkill;

pub(crate) struct Catalog<'a>(pub &'a [ListedSkill]);

impl fmt::Display for Catalog<'_> {
    fn fmt(&self, out: &mut fmt::Formatter<'_>) -> fmt::Result {
        let skills = self.0;
        if skills.is_empty() {
            return writeln!(out, "<skills></skills>");
        }
        writeln!(out, "<skills>")?;
        for skill in skills {
            write!(
                out,
                "  <skill name=\"{}\" description=\"{}\">",
                xml_escape(&skill.name, true),
                xml_escape(&skill.description, true),
            )?;
            if !skill.references.is_empty() {
                writeln!(
                    out,
                    "\n    <references>{}</references>",
                    xml_escape(&reference_list(&skill.references), false)
                )?;
                write!(out, "  ")?;
            }
            writeln!(out, "</skill>")?;
        }
        writeln!(out, "</skills>")
    }
}

fn reference_list(files: &[String]) -> String {
    // Root files are walked before references/, then directory groups appear
    // in the order of their first path.
    let mut directories = Vec::new();
    let mut groups: HashMap<&str, Vec<String>> = HashMap::new();
    for file in files
        .iter()
        .filter(|path| !path.contains('/'))
        .chain(files.iter().filter(|path| path.contains('/')))
    {
        let (dir, base) = file.rsplit_once('/').unwrap_or(("", file));
        groups
            .entry(dir)
            .or_insert_with(|| {
                directories.push(dir);
                Vec::new()
            })
            .push(shell_quote(base.strip_suffix(".md").unwrap_or(base)));
    }
    directories
        .into_iter()
        .map(|dir| {
            let prefix = if dir.is_empty() {
                String::new()
            } else {
                format!("{}/", shell_quote(dir))
            };
            let names = &groups[dir];
            match names.as_slice() {
                [name] => format!("{prefix}{name}.md"),
                _ => format!("{prefix}{{{}}}.md", names.join(",")),
            }
        })
        .collect::<Vec<_>>()
        .join(" ")
}

fn shell_quote(value: &str) -> String {
    if value
        .bytes()
        .all(|b| b.is_ascii_alphanumeric() || b"_./-".contains(&b))
    {
        value.into()
    } else {
        format!("'{}'", value.replace('\'', "'\\''"))
    }
}

fn xml_escape(value: &str, attribute: bool) -> String {
    let mut output = String::with_capacity(value.len());
    for c in value.chars() {
        match c {
            '&' => output.push_str("&amp;"),
            '<' => output.push_str("&lt;"),
            '>' => output.push_str("&gt;"),
            '"' if attribute => output.push_str("&#34;"),
            '\'' if attribute => output.push_str("&#39;"),
            '\t' => output.push_str("&#x9;"),
            '\n' => output.push_str("&#xA;"),
            '\r' => output.push_str("&#xD;"),
            '\u{0}'..='\u{1f}' | '\u{fffe}' | '\u{ffff}' => output.push('\u{fffd}'),
            c => output.push(c),
        }
    }
    output
}
