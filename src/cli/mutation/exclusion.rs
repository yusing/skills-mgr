use std::{fmt::Write, iter::Peekable, str::Chars};

use regex::RegexSet;

use super::Result;

pub(super) struct Exclusion {
    pub(super) patterns: Vec<String>,
    compiled: RegexSet,
}

impl Exclusion {
    pub(super) fn new(mut patterns: Vec<String>) -> Result<Self> {
        patterns.sort();
        patterns.dedup();
        let expressions = patterns
            .iter()
            .map(|pattern| {
                if pattern.is_empty()
                    || pattern.starts_with('/')
                    || pattern.chars().any(char::is_control)
                {
                    return Err(format!("invalid exclude pattern {pattern:?}").into());
                }
                compile(pattern)
                    .ok_or_else(|| format!("invalid exclude pattern {pattern:?}").into())
            })
            .collect::<Result<Vec<_>>>()?;
        Ok(Self {
            patterns,
            compiled: RegexSet::new(expressions)?,
        })
    }

    pub(super) fn excludes(&self, path: &str) -> bool {
        let mut path = path;
        loop {
            if self.compiled.is_match(path) {
                return true;
            }
            let Some((parent, _)) = path.rsplit_once('/') else {
                return false;
            };
            path = parent;
        }
    }
}

// Go path.Match does not have recursive ** or brace expansion. In a character
// class ^ negates and escaped punctuation represents a literal rune.
fn compile(pattern: &str) -> Option<String> {
    let mut rx = String::from(r"\A");
    let mut chars = pattern.chars().peekable();
    while let Some(ch) = chars.next() {
        match ch {
            '*' => {
                while chars.next_if_eq(&'*').is_some() {}
                rx.push_str("[^/]*");
            }
            '?' => rx.push_str("[^/]"),
            '[' => character_class(&mut chars, &mut rx)?,
            '\\' => push_literal(&mut rx, chars.next()?),
            ch => push_literal(&mut rx, ch),
        }
    }
    rx.push_str(r"\z");
    Some(rx)
}

fn character_class(chars: &mut Peekable<Chars<'_>>, rx: &mut String) -> Option<()> {
    fn rune(chars: &mut Peekable<Chars<'_>>) -> Option<char> {
        match chars.next()? {
            '\\' => chars.next(),
            ']' | '-' => None,
            ch => Some(ch),
        }
    }
    let negate = chars.next_if_eq(&'^').is_some();
    let mut ranges = String::new();
    loop {
        let lo = rune(chars)?;
        let hi = if chars.next_if_eq(&'-').is_some() {
            rune(chars)?
        } else {
            lo
        };
        if lo <= hi {
            push_literal(&mut ranges, lo);
            if lo != hi {
                ranges.push('-');
                push_literal(&mut ranges, hi);
            }
        }
        if chars.next_if_eq(&']').is_some() {
            break;
        }
    }
    if ranges.is_empty() {
        rx.push_str(if negate { "(?s:.)" } else { r"[\x00&&[^\x00]]" });
    } else {
        rx.push('[');
        if negate {
            rx.push('^');
        }
        rx.push_str(&ranges);
        rx.push(']');
    }
    Some(())
}

fn push_literal(output: &mut String, ch: char) {
    write!(output, "\\u{{{:x}}}", u32::from(ch)).expect("String writes cannot fail");
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn follows_path_match_syntax_and_matches_ancestors() {
        let exclusion = Exclusion::new(vec![
            "examples".into(),
            "assets/*.html".into(),
            "data/[^a-c]?.txt".into(),
            r"data/\*.json".into(),
            "assets/**/demo".into(),
        ])
        .unwrap();
        for path in [
            "examples/one/two",
            "assets/index.html",
            "data/x1.txt",
            "data/*.json",
            "assets/a/demo",
        ] {
            assert!(exclusion.excludes(path), "{path}");
        }
        for path in [
            "assets/docs/index.html",
            "data/b1.txt",
            "data/other.json",
            "assets/a/b/demo",
        ] {
            assert!(!exclusion.excludes(path), "{path}");
        }
        for pattern in ["[", "", "/all", "[]", "[a-]", "x\\"] {
            assert!(Exclusion::new(vec![pattern.into()]).is_err(), "{pattern}");
        }
    }
}
