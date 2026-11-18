use std::{
    io, mem,
    sync::atomic::{AtomicBool, Ordering},
};

static CANCELLED: AtomicBool = AtomicBool::new(false);

extern "C" fn request(_: libc::c_int) {
    CANCELLED.store(true, Ordering::Relaxed);
}

pub(super) fn install() -> io::Result<()> {
    // The single-threaded bridge process owns these handlers. The handler only
    // updates a lock-free atomic; cleanup happens on the normal execution path.
    let mut action: libc::sigaction = unsafe { mem::zeroed() };
    action.sa_sigaction = request as *const () as libc::sighandler_t;
    unsafe { libc::sigemptyset(&mut action.sa_mask) };
    for signal in [libc::SIGINT, libc::SIGTERM] {
        if unsafe { libc::sigaction(signal, &action, std::ptr::null_mut()) } < 0 {
            return Err(io::Error::last_os_error());
        }
    }
    Ok(())
}

pub(super) fn check() -> io::Result<()> {
    if CANCELLED.load(Ordering::Relaxed) {
        return Err(io::Error::new(
            io::ErrorKind::Interrupted,
            "operation canceled",
        ));
    }
    Ok(())
}
