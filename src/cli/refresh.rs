use std::{
    env,
    fs::{self, DirBuilder, File, OpenOptions, TryLockError},
    io::{self, Read, Seek, SeekFrom, Write},
    os::{
        fd::{AsRawFd, FromRawFd, RawFd},
        unix::{
            fs::{DirBuilderExt, MetadataExt, OpenOptionsExt},
            process::CommandExt,
        },
    },
    path::{Path, PathBuf},
    process::{Command, Stdio},
};

use jiff::Timestamp;

use crate::{SkillsManager, io_error};

use super::{Result, cancel, mutation, registry};

const INHERITED_LOCK: &str = "_SKILLS_MGR_REFRESH_FD";
const KEEP: u64 = 32 * 1024;

pub(super) fn maybe_start(manager: &SkillsManager) {
    let root = root(manager);
    if !due(&root) {
        return;
    }
    let launch = || -> Result<()> {
        let Some(lock) = try_lock(&root)? else {
            return Ok(());
        };
        if !due(&root) {
            return Ok(());
        }
        let mut command = Command::new(env::current_exe()?);
        let fd = lock.as_raw_fd();
        let log = open_log(&root)?;
        command
            .arg("refresh-runner")
            .env(INHERITED_LOCK, fd.to_string())
            .stdin(Stdio::null())
            .stdout(log.try_clone()?)
            .stderr(log);
        // Only async-signal-safe syscalls run after fork. The intermediate
        // process exits immediately, so the detached runner is not a child of
        // the foreground CLI or the Go program it can later exec.
        unsafe {
            command.pre_exec(move || {
                if libc::setsid() < 0 || libc::fcntl(fd, libc::F_SETFD, 0) < 0 {
                    return Err(io::Error::last_os_error());
                }
                match libc::fork() {
                    -1 => return Err(io::Error::last_os_error()),
                    0 => {}
                    _ => libc::_exit(0),
                }
                Ok(())
            });
        }
        let status = command.spawn()?.wait()?;
        if !status.success() {
            return Err(format!("refresh launcher terminated: {status}").into());
        }
        // Do not unlock explicitly; the child inherits the open description.
        drop(lock);
        Ok(())
    };
    if let Err(error) = launch()
        && let Ok(mut log) = open_log(&root)
    {
        let _ = writeln!(log, "start refresh runner: {error}");
    }
}

pub(super) fn run(manager: &SkillsManager) -> Result<()> {
    cancel::install()?;
    let root = root(manager);
    let lock = match env::var_os(INHERITED_LOCK) {
        Some(value) => Some(inherited_lock(&root, value.to_str().unwrap_or_default())?),
        None => try_lock(&root)?,
    };
    let Some(_lock) = lock else {
        return Ok(());
    };
    let mut log = open_log(&root)?;
    mutation::repair_managed(manager, &mut log);
    cancel::check()?;
    if let Err(error) = registry::refresh_topics(manager, Timestamp::now()) {
        writeln!(log, "refresh registry: {error}")?;
    }
    cancel::check()?;
    if let Err(error) = mutation::refresh_remotes(manager, &mut log) {
        writeln!(log, "refresh persisted remote skills: {error}")?;
    }
    cancel::check()?;
    let now = format!("{}\n", Timestamp::now());
    mutation::atomic_write(&root.join("refresh.completed"), now.as_bytes(), 0o600)
}

fn root(manager: &SkillsManager) -> PathBuf {
    manager.cache.join("skills-mgr")
}

fn due(root: &Path) -> bool {
    let at = File::open(root.join("refresh.completed")).and_then(|file| {
        let mut value = String::new();
        file.take(128).read_to_string(&mut value)?;
        Ok(value)
    });
    !at.is_ok_and(|value| registry::fresh(value.trim(), Timestamp::now()))
}

fn try_lock(root: &Path) -> Result<Option<File>> {
    DirBuilder::new()
        .mode(0o700)
        .recursive(true)
        .create(root)
        .map_err(|err| io_error(root, err))?;
    let path = root.join("refresh.lock");
    let file = OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .mode(0o600)
        .open(&path)
        .map_err(|err| io_error(&path, err))?;
    match file.try_lock() {
        Ok(()) => Ok(Some(file)),
        Err(TryLockError::WouldBlock) => Ok(None),
        Err(TryLockError::Error(err)) => Err(io_error(&path, err).into()),
    }
}

fn inherited_lock(root: &Path, value: &str) -> Result<File> {
    let fd: RawFd = value.parse()?;
    if fd < 3 || unsafe { libc::fcntl(fd, libc::F_SETFD, libc::FD_CLOEXEC) } < 0 {
        return Err("invalid inherited refresh lock descriptor".into());
    }
    // fcntl established that the descriptor is open and exclusive to this
    // runner. It must match the lock file at the configured cache path.
    let file = unsafe { File::from_raw_fd(fd) };
    let path = root.join("refresh.lock");
    let actual = file.metadata()?;
    let expected = fs::metadata(&path).map_err(|err| io_error(&path, err))?;
    if actual.ino() != expected.ino() || actual.dev() != expected.dev() {
        return Err("inherited descriptor does not match refresh lock".into());
    }
    file.try_lock()?;
    Ok(file)
}

fn open_log(root: &Path) -> Result<File> {
    let path = root.join("refresh.log");
    let mut file = OpenOptions::new()
        .create(true)
        .read(true)
        .append(true)
        .mode(0o600)
        .open(&path)
        .map_err(|err| io_error(&path, err))?;
    let length = file.metadata()?.len();
    if length > KEEP * 2 {
        file.seek(SeekFrom::Start(length - KEEP))?;
        let mut tail = Vec::new();
        (&mut file).take(KEEP).read_to_end(&mut tail)?;
        let tail = tail
            .iter()
            .position(|byte| *byte == b'\n')
            .filter(|index| *index + 1 < tail.len())
            .map_or(tail.as_slice(), |index| &tail[index + 1..]);
        file.set_len(0)?;
        file.write_all(tail)?;
    }
    Ok(file)
}
