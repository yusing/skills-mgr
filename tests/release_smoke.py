"""Exercise an extracted release bundle using disposable skill state."""

import errno
import fcntl
import json
import os
from pathlib import Path
import pty
import select
import shutil
import signal
import struct
import subprocess
import sys
import tempfile
import termios
import time


def exercise_tui(core, project, env):
    steps = [
        (b"release-smoke", b" "),
        (b"disabled release-smoke", b"q"),
    ]
    pid, fd = pty.fork()
    if pid == 0:
        try:
            os.chdir(project)
            os.execve(core, [str(core)], env)
        except OSError as exc:
            print(exc, file=sys.stderr, flush=True)
            os._exit(1)

    output = bytearray()
    try:
        fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 30, 120, 0, 0))
        deadline = time.monotonic() + 15
        eof = False
        while time.monotonic() < deadline:
            if not select.select([fd], [], [], 0.25)[0]:
                continue
            try:
                chunk = os.read(fd, 65536)
            except OSError as exc:
                if exc.errno != errno.EIO:
                    raise
                chunk = b""
            if not chunk:
                eof = True
                break
            output.extend(chunk)
            for probe, reply in (
                (b"\x1b[6n", b"\x1b[1;1R"),
                (b"\x1b]11;?", b"\x1b]11;rgb:0000/0000/0000\x1b\\"),
                (b"\x1b[c", b"\x1b[?1;2c"),
            ):
                if probe in chunk:
                    os.write(fd, reply)
            if steps and steps[0][0] in output:
                _, key = steps.pop(0)
                os.write(fd, key)
        finished, status = os.waitpid(pid, 0 if eof else os.WNOHANG)
        if finished:
            pid = None
        assert finished and os.waitstatus_to_exitcode(status) == 0, bytes(output)
        assert not steps, bytes(output)
        assert b"\x1b[?1049h" in output and b"\x1b[?1049l" in output, bytes(output)
    finally:
        os.close(fd)
        if pid is not None:
            os.kill(pid, signal.SIGKILL)
            os.waitpid(pid, 0)


def smoke(bin_dir):
    core = bin_dir / "skills-mgr"
    assert os.access(bin_dir / "skills-mgr-tui", os.X_OK)
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        home = root / "home"
        project = root / "project"
        manifest = project / ".agents/skills/release-smoke/SKILL.md"
        home.mkdir()
        manifest.parent.mkdir(parents=True)
        manifest.write_text(
            "---\nname: release-smoke\ndescription: Release fixture.\n---\nSmoke-test body.\n"
        )
        config = project / ".skills-mgr.json"
        original = b'{"schema_revision":2,"skills":{"release-smoke":{"enabled":true}}}\n'
        config.write_bytes(original)
        env = dict(
            os.environ,
            HOME=str(home),
            CODEX_HOME=str(home / ".codex"),
            XDG_CACHE_HOME=str(root / "cache"),
            SKILLS_MGR_NO_REFRESH="1",
            TERM="xterm-256color",
        )
        for key in ("CLAUDECODE", "GROK_AGENT", "GROK_SESSION_ID", "CODEX_THREAD_ID"):
            env.pop(key, None)

        # The public CLI must not look for the companion on a noninteractive path.
        standalone = root / "skills-mgr"
        shutil.copy2(core, standalone)

        def call(binary, *args):
            return subprocess.check_output([binary, *args], cwd=project, env=env, timeout=10)

        assert b"skills-mgr" in call(standalone, "--help")
        assert b'name="release-smoke"' in call(standalone, "list")
        assert b"Smoke-test body." in call(standalone, "get", "release-smoke")
        assert config.read_bytes() == original

        exercise_tui(core, project, env)
        selection = json.loads(config.read_bytes())
        assert selection["skills"]["release-smoke"]["enabled"] is False, selection
        assert b'name="release-smoke"' not in call(standalone, "list")
    print("Standalone CLI and archived TUI passed; selection updated and terminal restored.")


if __name__ == "__main__":
    smoke(Path(sys.argv[1]).resolve())
