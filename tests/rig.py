# SPDX-License-Identifier: GPL-3.0-or-later
#
# ⚠️ GPL-3 by association rather than by import, as run_gate.py: it exists to place the files
# beside it that do import zigpy-znp. Nothing here is linked into the briard-tether binary or
# ships in any release artifact.
"""Where the tether under test runs, and how the emulator's wire reaches it.

Every harness in this directory is built the same way: a coordinator on one end, a tether in
the middle, a real client on the other. All of it runs on this machine. This module is the one
seam that lets the *middle* be somewhere else — today a Windows machine, which is the only way
anything here runs on the platform this project keeps possible beside Linux.

**Only tether moves.** The emulator, the clients and pytest stay on Linux, because the client
reaches tether over TCP and the coordinator reaches it over a serial port, and both of those
cross a machine boundary without caring. That is what makes this cheap rather than a port of
the whole harness.

Local is the default: a pty, a subprocess, `127.0.0.1`.
Setting `TETHER_WINDOWS_SSH` switches the middle to the far machine:

    TETHER_WINDOWS_SSH  the command that runs a program over there, e.g.
                        "ssh -i ~/.ssh/id_ed25519 user@192.0.2.10". Its presence is what
                        selects Windows mode; nothing else here is required.
    TETHER_WINDOWS_ADDR the address this machine reaches that one at (default: the host part
                        of the ssh command's last word, which is right for a plain target and
                        wrong for an ssh alias — say it out loud when in doubt).
    TETHER_WINDOWS_EXE  the tether binary over there  (default C:\\briard\\briard-tether.exe)
    TETHER_WINDOWS_LEASH the helper that runs it there, tests/leash (default C:\\briard\\leash.exe)
    TETHER_WINDOWS_DEV  what the coordinator is called there            (default COM1)
    TETHER_WINDOWS_WIRE the TCP port on *this* machine that the far COM port is bridged to
                        (default 7000). The emulator connects to it; see SerialWire.
    TETHER_WINDOWS_BRIDGE  the Linux bridge the far machine's NIC hangs off, for the one gate
                        that must share a segment with it — the `mdns://` client (run_gate.py).
                        Unset means that gate refuses.
    TETHER_WINDOWS_GATE_ADDR  the address that client takes on that segment (default: the far
                        machine's /24 with host .2 — outside a dnsmasq range that starts higher).

**Three things have to be true on the far side.** This module assumes them and says plainly
when one is missing, because a harness that silently tested the local tether instead would be
worse than one that refused to run:

1. **The binaries are over there** — a cross-built `briard-tether.exe` and the `leash.exe`
   built from tests/leash, at the two paths above. Nothing here pushes them.
2. **An inbound firewall rule allows tether's program**, or a client's connection is neither
   accepted nor refused, it simply never arrives:

       netsh advfirewall firewall add rule name="briard-tether" dir=in action=allow
           program="C:\\briard\\briard-tether.exe" enable=yes profile=any

3. **A TCP port on *this* machine is the far machine's COM1**, which is what `SerialWire`
   attaches to — connecting to it is plugging the dongle in. Under qemu that is a chardev
   socket the guest's first serial port is wired to:

       -chardev socket,id=ser0,host=127.0.0.1,port=7000,server=on,wait=off,nodelay=on
       -serial chardev:ser0

   `nodelay=on` is not decoration: it is worth 41 ms a frame, for the reason `SerialWire`
   gives.
"""

from __future__ import annotations

import json
import os
import pathlib
import shlex
import socket
import subprocess
import tempfile
import time

from ptylink import PtyLink, PtyTransport

SSH = os.environ.get("TETHER_WINDOWS_SSH", "")
WINDOWS = bool(SSH)

EXE = os.environ.get("TETHER_WINDOWS_EXE", r"C:\briard\briard-tether.exe")
# tests/leash: what actually runs over there, with tether under it. It takes the config on its
# stdin, streams tether's output back, and stops tether — orderly, CTRL_BREAK — when that stdin
# closes, so one ssh session carries a tether from start to finish (see Tether.stop).
LEASH = os.environ.get("TETHER_WINDOWS_LEASH", r"C:\briard\leash.exe")
DEVICE = os.environ.get("TETHER_WINDOWS_DEV", "COM1")
WIRE_PORT = int(os.environ.get("TETHER_WINDOWS_WIRE", "7000"))
# Where tether's config lands over there. Fixed rather than temporary: one tether runs at a
# time, and a name a human can read is worth more here than a unique one — when a run fails,
# the next thing anybody does is open that file.
CONFIG_PATH = r"C:\briard\tether-under-test.json"


def _addr() -> str:
    if explicit := os.environ.get("TETHER_WINDOWS_ADDR"):
        return explicit
    target = shlex.split(SSH)[-1]
    return target.rpartition("@")[2]


ADDR = _addr() if WINDOWS else "127.0.0.1"
BRIDGE = os.environ.get("TETHER_WINDOWS_BRIDGE", "")
GATE_ADDR = os.environ.get("TETHER_WINDOWS_GATE_ADDR", ADDR.rsplit(".", 1)[0] + ".2/24")


def ssh() -> list[str]:
    """The ssh command as argv, multiplexed if it is plain ssh.

    A session to Windows' sshd costs 400–560 ms to set up and ~155 ms over an existing
    connection (measured), and a suite run starts one per test. So when the command is `ssh`
    and the caller has not said otherwise, the first session opens a master that the rest
    share, and it lingers a minute past the last so a run never re-opens it.
    The socket lives in the temp dir under a short name: a ControlPath is a unix socket path,
    and those are limited to ~100 bytes.
    """
    argv = shlex.split(SSH)
    if argv and pathlib.PurePath(argv[0]).name == "ssh" and not any("ControlPath" in a for a in argv):
        path = pathlib.Path(tempfile.gettempdir()) / f"tether-ssh-{os.getuid()}-%C"
        argv[1:1] = ["-o", "ControlMaster=auto", "-o", f"ControlPath={path}",
                     "-o", "ControlPersist=60"]
    return argv


def binary(path: pathlib.Path) -> pathlib.Path:
    """The tether under test, checked before anything is started.

    Four harnesses asked the same question — is there a binary where you said? — and it has a
    second answer now: **when tether runs on the far machine, the local build is not the thing
    under test at all**, and a check that insisted on one would refuse to run for the wrong
    reason. What matters over there is that the exe was pushed in, which the first ssh call
    reports better than a guess from here could.
    """
    if WINDOWS:
        return pathlib.PureWindowsPath(EXE)
    if not path.is_file():
        raise SystemExit(
            f"no tether binary at {path}.\n"
            "  Build one first: go build -o briard-tether ./cmd/briard-tether"
        )
    return path.resolve()


def local_only(what: str) -> None:
    """Refuse, rather than quietly measure the wrong machine.

    A harness that cannot work over the seam calls this, so that setting `TETHER_WINDOWS_SSH`
    and running it says so plainly instead of failing somewhere deep with a pty path Windows
    cannot open.
    """
    if WINDOWS:
        raise SystemExit(
            f"{what} runs only against a local tether, and TETHER_WINDOWS_SSH is set.\n"
            "  Unset it to run this one here."
        )


# The marker `into_a_private_network` sets on the environment it re-execs into. It lives here
# rather than in `run_gate`, which defines the function, because `listen` below has to know and
# `run_gate` imports this module rather than the other way round.
PRIVATE_NETWORK = "TETHER_GATE_PRIVATE_NETWORK"

# The address of the one advertisable interface inside that namespace — `wire0`, the near end of
# the veth pair `into_a_private_network` builds. A tether in there binds every interface and
# announces this, so this is where a client reaches it. The far end is deliberately *not*
# multicast-capable, so `advertisable` leaves it out and exactly one address is announced; two
# would put the same "which address did the client take" coin-flip into the rig that a
# multi-homed host puts on a real LAN.
PRIVATE_ADDRESS = "10.86.0.1"


def reachable_host(bound: str | None) -> str:
    """Where a client reaches a tether that bound `bound`.

    Not the same question as what it bound, which is why this exists: inside the private
    namespace `listen` binds everything, and `0.0.0.0` is not somewhere to dial.
    """
    if WINDOWS:
        return ADDR
    if bound in (None, "", "0.0.0.0"):
        return PRIVATE_ADDRESS if os.environ.get(PRIVATE_NETWORK) else "127.0.0.1"
    return bound


def listen(port: int) -> str:
    """What tether's config should bind, on whichever machine it runs.

    Loopback here, because everything is here. Every interface on the far machine, because the
    client is *not* there — which is also what makes that machine's firewall part of the test
    rather than a thing it can quietly pass without.

    ⚠️ **And every interface inside the private namespace, because that namespace has a
    segment in it.** It holds a veth pair, so "everything is here" is still true and *here*
    has two interfaces: tether advertises the veth's address
    (`advertisedAddress` falls through to the library's per-interface set when the bound address
    is unspecified), and a client that dialled loopback would be dialling the wrong one. Binding
    everything is safe in there in a way it is not anywhere else — the namespace has no route off
    itself, so "every interface" is two that nobody else can reach.
    """
    if WINDOWS or os.environ.get(PRIVATE_NETWORK):
        return f"0.0.0.0:{port}"
    return f"127.0.0.1:{port}"


def where() -> str:
    """One line for a harness to print, so a run says which machine it measured."""
    if not WINDOWS:
        return "tether here, on this machine"
    return f"tether on {ADDR} ({EXE}, device {DEVICE}), reached over: {SSH}"


class SerialWire:
    """The emulator's end of a serial line whose other end is on the far machine.

    It is the counterpart of `PtyLink` and deliberately as small: what a coordinator needs is
    a descriptor to be read and a transport to answer into, and both of those are a connected
    socket here. The hypervisor listens on `WIRE_PORT` and hands whatever connects to it to
    the guest's COM port, so *connecting is plugging the dongle in* — and closing is the
    unplug, which is why `PtyLink` and this can stand in for each other at all.

    ⚠️ **Bytes written before the far side has the port open are dropped** (measured: 18
    bytes each way once tether had opened COM1, nothing at all before). That is a real
    UART's behaviour rather than a leak in the bridge, and it costs nothing here
    because the emulator only ever answers — but a future test that speaks first has to
    attach after tether, not before.
    """

    device = DEVICE

    def __init__(self, port: int = WIRE_PORT) -> None:
        self.port = port
        try:
            self.sock = socket.create_connection(("127.0.0.1", port), timeout=5)
        except OSError as exc:
            # The two failures look nothing alike and are worth telling apart. Refused means
            # the bridge is not there; a *timeout* means it is, and is occupied — one COM port
            # takes one coordinator, and the second connection is not refused, it waits in the
            # kernel's backlog while the hypervisor serves the first. The usual cause is an
            # emulator a killed run left attached (measured, and it cost the time this
            # message now saves).
            raise SystemExit(
                f"cannot attach to the far machine's {DEVICE} at 127.0.0.1:{port}: {exc}.\n"
                + ("  Something is already attached to it — an emulator left over from an\n"
                   "  interrupted run holds it until it exits: look for fake_coordinator.py.\n"
                   if isinstance(exc, TimeoutError) else
                   "  Nothing is bridging that port: the VM has to be booted with its first\n"
                   "  serial port wired to a socket chardev on this address — under qemu,\n"
                   f"  -chardev socket,id=ser0,host=127.0.0.1,port={port},server=on,"
                   "wait=off,nodelay=on\n"
                   "  plus -serial chardev:ser0.\n")
                + "  tests/rig.py's module docstring has the whole far-side setup."
            ) from exc
        # Blocking again, and this is not tidying: `create_connection(timeout=…)` leaves the
        # socket non-blocking, `PtyTransport` writes with `os.write` in a loop the way a pty
        # wants, and an `os.write` that returns EAGAIN instead of waiting would lose an
        # emulator's reply — which reaches the client as a coordinator that went quiet, the
        # single most misleading failure this harness could produce.
        self.sock.settimeout(None)
        # Nagle would coalesce a reply with whatever the emulator says next, which is the one
        # thing the thing under test must not depend on: tether is a byte pipe and a frame
        # arriving in two pieces has to be as ordinary here as it is on a real UART.
        #
        # ⚠️ **The hypervisor's end of this socket needs the same thing, and it is worth 41 ms.**
        # qemu's emulated 16550 hands its chardev one byte per write, so with Nagle on *its* side
        # the first byte of every frame went out alone and the rest waited for this side's
        # delayed ACK: a five-byte turnaround cost 41 ms without `nodelay=on` on the chardev and
        # 0.99 ms with it (measured, and reproduced on a Linux guest, which is what showed it was
        # the socket rather than Windows). Setting it here fixes only half of it — the chardev
        # line in this module's docstring is the other half.
        self.sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        self.fd = self.sock.fileno()
        self.loop = None

    def __str__(self) -> str:
        return f"bridged from 127.0.0.1:{self.port}"

    def transport(self) -> PtyTransport:
        # The same transport the pty uses: it writes with os.write in a loop, which is right
        # for a socket too — a short write is legal on both.
        return PtyTransport(self.fd)

    def close(self) -> None:
        if self.loop is not None:
            # Only if the loop is still there: pytest-asyncio closes its per-test loop before
            # a sync fixture's teardown runs, and `remove_reader` on a closed loop raises —
            # which would turn tidying up into a test error.
            if not self.loop.is_closed():
                self.loop.remove_reader(self.fd)
            self.loop = None
        self.sock.close()


def wire(link: pathlib.Path | None = None):
    """The wire an emulated coordinator speaks through, wherever tether is.

    A `PtyLink` here, a `SerialWire` when tether is on the far machine. Both answer `fd`,
    `transport()`, `close()` and `device` — that last one being what tether's config must
    call the coordinator, which is the whole difference between a pty path and `COM1`.
    """
    return SerialWire() if WINDOWS else PtyLink(link)


class Tether:
    """One tether under test, here or on the far machine, with its log kept either way.

    The four harnesses had four copies of "write a config, Popen the binary, keep the output,
    terminate it" — which is past the three real call sites an abstraction has to earn here,
    and is why this is a class rather than an if statement in each of them.
    """

    def __init__(self, tether: pathlib.Path, config: dict, workdir: pathlib.Path,
                 name: str = "tether") -> None:
        self.config = config
        self.name = name
        self.log_path = workdir / f"{name}.log"
        self.log = self.log_path.open("wb")
        self.host, _, port = config["listen"].rpartition(":")
        self.port = int(port)
        self.endpoint = (reachable_host(self.host), self.port)

        if WINDOWS:
            self._start_over_there()
        else:
            path = workdir / f"{name}.json"
            path.write_text(json.dumps(config))
            self.proc = subprocess.Popen(
                [str(tether), "run", "-config", str(path)],
                stdout=self.log, stderr=subprocess.STDOUT,
            )

    def _start_over_there(self) -> None:
        """One ssh session: leash over there takes the config on stdin, runs tether under it
        and streams tether's output back — so the log this class keeps is the same text the
        local path produces, and the session's stdin is the handle `stop()` lets go of.

        ⚠️ The config travels on stdin, as one line, and **not** as part of the command: OpenSSH
        on Windows runs the command through the account's default shell — PowerShell here —
        so anything in it is parsed twice, and a PowerShell in front of a native program wraps
        its stderr in error records.
        """
        self.proc = subprocess.Popen(
            ssh() + [LEASH, CONFIG_PATH, EXE, "run", "-config", CONFIG_PATH],
            stdout=self.log, stderr=subprocess.STDOUT, stdin=subprocess.PIPE,
        )
        self.proc.stdin.write(json.dumps(self.config).encode() + b"\n")
        self.proc.stdin.flush()

    def serving(self, timeout: float = 30.0) -> None:
        """Wait until a client could connect, or say why not.

        The check is a real connection to the address a client will use, which on Windows
        also means it is the first thing to fail when the far machine's firewall has not been
        told about tether — the error says so rather than timing out anonymously.
        """
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            with socket.socket() as s:
                s.settimeout(0.3)
                if s.connect_ex(self.endpoint) == 0:
                    return
            if self.died():
                raise RuntimeError(f"tether exited: {self.output()}")
            time.sleep(0.05)
        hint = ""
        if WINDOWS:
            hint = ("\n  Nothing there refused the connection either, which on Windows is "
                    "usually the firewall:\n"
                    '  netsh advfirewall firewall add rule name="briard-tether" dir=in '
                    "action=allow\n"
                    f'      program="{EXE}" enable=yes profile=any')
        raise TimeoutError(
            f"tether never accepted on {self.endpoint[0]}:{self.port}: {self.output()}{hint}"
        )

    def died(self) -> bool:
        return self.proc.poll() is not None

    def output(self) -> str:
        """Everything it has said so far — the same text the local path produces, because
        over ssh tether's stdout *is* the ssh client's stdout.

        Readable after `stop()` too: a failing run asks for this last, once everything has
        been taken down, and a closed handle is not a reason to lose the log.
        """
        if not self.log.closed:
            self.log.flush()
        return self.log_path.read_text(errors="replace").strip()

    def stop(self) -> None:
        """Take it down, and make sure it went.

        Locally that is SIGTERM — what a service manager sends (main.go), so the advert is
        withdrawn and the client closed on the way out.

        On Windows it is **closing the session's stdin**: leash sees EOF and raises CTRL_BREAK
        on tether, which Go delivers as SIGINT — the same shutdown path — and the session
        ends when tether has, so the wait on the ssh client *is* the wait for the port to be
        free. Nothing else reaches a console program there from outside (measured: sshd
        leaves a process running when the connection drops, `taskkill` without `/F` refuses
        a program with no window, and `/F` skips the shutdown path). A `taskkill /F` in a
        second ssh session is kept below as the fallback for a tether that does not stop
        when asked, because a test that starts against a tether still holding the port is a
        flake, not a slow test.
        """
        if WINDOWS and self.proc.poll() is None:
            try:
                self.proc.stdin.close()
            except OSError:
                pass  # the session is already gone; the wait below says so
            try:
                self.proc.wait(timeout=15)
            except subprocess.TimeoutExpired:
                self._kill_over_there()
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=10)
        self.log.close()

    def _kill_over_there(self) -> None:
        """The hard way, for a tether that ignored CTRL_BREAK: kill by name — anything holding
        the port, not just ours — and wait for it to be gone, which `taskkill /F` alone does
        not do (it returns before the process dies)."""
        name = pathlib.PureWindowsPath(EXE).name
        subprocess.run(
            ssh() + ["powershell", "-NoProfile", "-Command",
                     f"taskkill /F /IM {name} 2>$null; "
                     "$end = (Get-Date).AddSeconds(10); "
                     f"while ((Get-Process {pathlib.PureWindowsPath(EXE).stem} "
                     "-ErrorAction Ignore) -and (Get-Date) -lt $end) "
                     "{ Start-Sleep -Milliseconds 50 }"],
            capture_output=True,
        )
