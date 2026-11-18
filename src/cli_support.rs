//! Internal interface shared with the executable workspace member.

use std::path::{Path, PathBuf};

use crate::{ConditionFallback, SkillsManager};

pub use crate::{invalid_data, io_error};

pub mod catalog {
    pub use crate::catalog::{
        Native, Skill, Source, claude_plugins, discover, find_filesystem, references,
    };
    #[cfg(unix)]
    pub use crate::catalog::{discover_local, managed_skills, user_skills};
}

pub mod condition {
    pub use crate::condition::Conditions;
}

pub mod content {
    #[cfg(unix)]
    pub use crate::content::{
        MANIFEST, PLACEHOLDER_MARKER, PLACEHOLDER_MARKER_NAME, discover, edited_metadata,
        patched_metadata, toggled_invocation,
    };
    pub use crate::content::{
        Metadata, get_with_patch_error, is_manifest, open_file, open_root, split_target,
        valid_name, validate_body,
    };
}

pub mod remote {
    #[cfg(unix)]
    pub use crate::remote::{Override, Record, invocation_override, record_by_key};
    pub use crate::remote::{
        Provider, RemoteRef, content_root, hex_digest, patch, patch_path, records, store_path,
    };
}

pub mod selection {
    pub use crate::selection::{Enabled, LOCK_NAME, Lock, Selections, evaluate, global_path, load};
    #[cfg(unix)]
    pub use crate::selection::{REVISION, Selection, load_with_revision};
}

impl SkillsManager {
    #[doc(hidden)]
    pub fn with_condition_fallback(mut self, fallback: ConditionFallback) -> Self {
        self.condition_fallback = Some(fallback);
        self
    }

    #[doc(hidden)]
    pub fn project(&self) -> &Path {
        &self.project
    }

    #[doc(hidden)]
    pub fn home(&self) -> &Path {
        &self.home
    }

    #[doc(hidden)]
    pub fn cache(&self) -> &Path {
        &self.cache
    }

    #[doc(hidden)]
    pub fn codex_home(&self) -> &Path {
        &self.codex_home
    }

    #[doc(hidden)]
    pub fn managed_home(&self) -> PathBuf {
        self.manager_home()
    }
}
