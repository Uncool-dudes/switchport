"""Run a command under a pty, type into it, and report output and exit code.

usage: pty_run.py <delay-seconds> <keys> <command...>
"""
import os
import pty
import select
import signal
import sys
import time

delay, keys = float(sys.argv[1]), sys.argv[2].encode().decode("unicode_escape").encode()
pid, fd = pty.fork()
if pid == 0:
    os.execvp(sys.argv[3], sys.argv[3:])

output, sent = b"", False
start = time.time()
status = None
while time.time() - start < 10:
    if not sent and time.time() - start >= delay:
        os.write(fd, keys)
        sent = True
    ready, _, _ = select.select([fd], [], [], 0.05)
    if ready:
        try:
            data = os.read(fd, 4096)
        except OSError:
            data = b""
        if data:
            output += data
    done, st = os.waitpid(pid, os.WNOHANG)
    if done:
        status = st
        break
if status is None:
    os.kill(pid, signal.SIGKILL)
    print("TIMEOUT")
sys.stdout.write(output.decode(errors="replace").replace("\r", ""))
print("EXIT", os.waitstatus_to_exitcode(status) if status is not None else "none")
