# SPDX-License-Identifier: GPL-3.0-or-later
#
# ⚠️ GPL-3 with the rest of this directory: it drives zigpy, which is GPL-3.
# Nothing here is linked into the briard-tether binary or ships in any release artifact.
"""The dongle goes away while a real client is using it.

One of the three survival scenarios in this repo's definition of done — *dongle unplug/replug*
— is otherwise proved only against a raw TCP client. This runs it with a real Zigbee stack on
the end, which matters because INV 3 is not only a claim about tether. "A clean close is the
trigger for recovery paths the clients already test" is a claim about **the client's**
behaviour, and a belief about somebody else's side is exactly the kind that turns out to be
load-bearing.

So this asserts tether's contract and *measures* the client's:

  tether is held to  — INV 3, the client's connection closes promptly rather than stalling;
                       INV 2 backwards, no listener exists while there is no device;
                       the same tether reopens the same stable name and keeps its counters.
  the client is      — how long it takes to notice, what it reports, and whether it recovers on
  observed             its own. Whatever it does is written down rather than asserted, because
                       it is their behaviour and this is the first look at it.

**Both clients meet the same event, and that is why `--client` lives here rather than in a
second scenario file.** The measurement worth having is the *comparison* — zigpy and herdsman
share no code, and INV 3 leans on both behaving well — so the pty, its trap, and the emulator
that keeps its NVRAM across the unplug have to be identical for the two. The zigpy client runs
in this process; the herdsman client is a Node subprocess that reports what it saw over its
stdout (tests/herdsman_client.js).

The replug is the *same emulator* bound to a *new pty* under the same name: same NVRAM, same
network, new node. That is what a stick that was unplugged and put back in is, and it is what
makes "the network survived" a question worth asking.

    go build -o briard-tether ./cmd/briard-tether
    python3 tests/unplug_scenario.py                     # zigpy, the client ZHA is built on
    python3 tests/unplug_scenario.py --client herdsman   # zigbee2mqtt's stack

**Or a real stick, pulled by whatever can pull it** — `--device` names the port as tether sees
it, `--unplug`/`--replug` are the commands that remove and restore it, and tether runs wherever
`rig` says (here, or on the Windows machine `TETHER_WINDOWS_SSH` names):

    python3 tests/unplug_scenario.py --device COM3 \\
        --unplug "printf 'device_del usb0\\n' | socat - unix-connect:monitor.sock" \\
        --replug "printf 'device_add usb-host,hostbus=1,hostaddr=4,id=usb0\\n' | socat - unix-connect:monitor.sock"

That form is the one scenario no emulator can stage on Windows, because a COM port the
hypervisor keeps alive never goes away. ⚠️ It also wants the stick passed through as a
*device* (`usb-host`) rather than the guest holding its whole controller: a PCIe `device_del`
is a request Windows refuses while the port is open, and forcing it still does not fail the
open handle, so nothing here would ever see the device go.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import pathlib
import socket
import subprocess
import sys
import tempfile
import time

HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

import zigpy.config
from zigpy_znp.zigbee.application import ControllerApplication
import zigpy_znp.config as conf

from fake_coordinator import HerdsmanReplies, bind, build_server, znp_fixtures
import rig
from ptylink import PtyLink


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def refused(endpoint: tuple[str, int]) -> bool:
    with socket.socket() as s:
        s.settimeout(0.2)
        return s.connect_ex(endpoint) != 0


async def until(predicate, what: str, timeout: float = 30.0) -> float:
    """Wait for something, and return how long it took — the number is half the point here."""
    started = time.monotonic()
    while time.monotonic() - started < timeout:
        if predicate():
            return time.monotonic() - started
        await asyncio.sleep(0.05)
    raise TimeoutError(f"timed out after {timeout:g}s waiting for {what}")


async def client_for(endpoint: tuple[str, int]) -> ControllerApplication:
    app = ControllerApplication(
        {
            conf.CONF_DEVICE: {conf.CONF_DEVICE_PATH: "socket://%s:%d" % endpoint},
            zigpy.config.CONF_NWK_BACKUP_ENABLED: False,
        }
    )
    await app.startup(auto_form=False)
    return app


class HerdsmanObserver:
    """A zigbee-herdsman client, attached and watching, in a Node process of its own.

    It exists because the client is TypeScript and the staging is Python: the plug has to be
    pulled by whoever owns the pty. So this starts the client, waits for it to say it is
    attached, and then reads back what it saw once the unplug has happened. Everything it
    reports is the client's own account — see tests/herdsman_client.js.

    ⚠️ **Everything here is async because the emulator shares this event loop.** A blocking
    read on the client's pipe stops the coordinator from answering, and what that looks like is
    a client that cannot read NVRAM through a tether that is plainly connected — a tether bug,
    to all appearances. It cost an afternoon once; it should not cost another.
    """

    def __init__(self, proc: asyncio.subprocess.Process) -> None:
        self.proc = proc
        self.seen: dict[str, object] = {}

    @classmethod
    async def start(cls, endpoint: tuple[str, int], watch: bool) -> HerdsmanObserver:
        argv = ["node", str(HERE / "herdsman_client.js"), "tcp://%s:%d" % endpoint]
        if watch:
            argv.append("--watch")
        # Only stdout is a pipe. stderr is herdsman's own logging and is left on the terminal,
        # where it is useful when a step times out and ignorable otherwise.
        return cls(await asyncio.create_subprocess_exec(*argv, stdout=asyncio.subprocess.PIPE))

    async def until(self, key: str, timeout: float = 60.0) -> object:
        """Read lines until the client emits `key`, and return what it carried.

        The client's own timeouts are shorter than this one, so this fires only if it died.
        """
        async def read_until() -> object:
            while True:
                line = await self.proc.stdout.readline()
                if not line:
                    raise TimeoutError(f"the herdsman client exited before saying {key!r}")
                message = json.loads(line)
                self.seen.update(message)
                if "error" in message:
                    raise TimeoutError(f"the herdsman client failed: {message['error']}")
                if key in message:
                    return message[key]

        try:
            return await asyncio.wait_for(read_until(), timeout=timeout)
        except asyncio.TimeoutError:
            raise TimeoutError(f"the herdsman client never said {key!r}") from None

    async def stop(self) -> None:
        if self.proc.returncode is None:
            self.proc.terminate()
            try:
                await asyncio.wait_for(self.proc.wait(), timeout=15)
            except asyncio.TimeoutError:
                self.proc.kill()
                await self.proc.wait()


def facts(app: ControllerApplication) -> dict[str, str]:
    net, node = app.state.network_info, app.state.node_info
    return {
        "ieee": str(node.ieee),
        "pan_id": f"0x{net.pan_id:04X}",
        "channel": str(net.channel),
    }


class Stick:
    """What gets unplugged and put back: the emulator on a pty, or a real one somebody pulls.

    The scenario is the same either way — a client holding the port, the device going, the
    device coming back under the same name — so the two are one interface with two bodies.
    The emulator keeps its NVRAM across the unplug because that is the entire claim being
    tested; a real stick keeps it because it is a real stick.
    """

    def __init__(self, workdir: pathlib.Path, client: str, device: str | None,
                 unplug: str | None, replug: str | None) -> None:
        self.unplug_cmd, self.replug_cmd = unplug, replug
        if device:
            self.name = device
            self.server = self.pty = self.link = None
            return
        self.link = workdir / "coordinator"
        self.name = str(self.link)
        self.pty = PtyLink(self.link)
        fixture = znp_fixtures.FormedLaunchpadCC26X2R1
        if client == "herdsman":
            # The five replies that client needs and zigpy-znp's emulator does not have;
            # opt-in, so the zigpy run still meets their emulator exactly as shipped. See
            # `HerdsmanReplies` in fake_coordinator.py.
            fixture = type(f"Herdsman{fixture.__name__}", (HerdsmanReplies, fixture), {})
        self.server = build_server(fixture, self.pty.path)
        bind(self.server, self.pty)

    def unplug(self) -> str:
        """Both halves of it, as the Go test at tier 1 does: the node goes and so does the
        name that pointed at it. A test that removed only one would be staging something that
        cannot happen to a real adapter — and a real one, hot-removed by a hypervisor or pulled
        by a hand, removes both by itself."""
        if self.pty is not None:
            self.pty.close()
            return "closing the pty"
        subprocess.run(self.unplug_cmd, shell=True, check=True, stdout=subprocess.DEVNULL)
        return self.unplug_cmd

    def replug(self) -> str:
        """The same coordinator, a new node, the same stable name: what udev does — and what
        Windows may or may not do, which is one of the things a real run measures."""
        if self.pty is not None:
            self.pty = PtyLink(self.link)
            bind(self.server, self.pty)
            return "a new pty under the same name"
        subprocess.run(self.replug_cmd, shell=True, check=True, stdout=subprocess.DEVNULL)
        return self.replug_cmd

    def close(self) -> None:
        if self.pty is not None:
            self.pty.close()


async def run(tether_binary: pathlib.Path, client: str, workdir: pathlib.Path,
              device: str | None = None, unplug: str | None = None,
              replug: str | None = None) -> int:
    port = free_port()
    stick = Stick(workdir, client, device, unplug, replug)

    tether = rig.Tether(
        tether_binary,
        {"device": stick.name, "radio": "znp", "listen": rig.listen(port), "advertise": False},
        workdir,
    )
    endpoint = tether.endpoint

    findings: list[str] = []
    try:
        await until(lambda: not refused(endpoint), "tether to accept")
        if client == "zigpy":
            app = await client_for(endpoint)
            before = facts(app)
        else:
            # The Node client attaches, reports the network, and then blocks on the
            # disconnect. `ready` is what says it is holding the port and the plug can go.
            observer = await HerdsmanObserver.start(endpoint, watch=True)
            before = await observer.until("facts")
            await observer.until("ready")
        print(f"a real {client} client is attached and the network is {before}\n", flush=True)

        # ─── the unplug ───────────────────────────────────────────────────────────────────
        print("unplugging the dongle while the client holds it: " + stick.unplug(), flush=True)
        pulled = time.monotonic()

        # INV 2 read backwards: while there is no device there is no listener, so a client
        # retrying during the outage is refused outright rather than accepted into a silence.
        # Measured on its own, and claimed on its own — an earlier version of this waited on
        # three conditions at once and then reported the result as though it had measured the
        # client, which would have been a passing test making a claim it never checked.
        gone = await until(lambda: refused(endpoint), "the listener to go", timeout=15.0)
        findings.append(f"the listener went {gone:.2f}s after the device did (INV 2 backwards)")
        for _ in range(10):
            if not refused(endpoint):
                findings.append("FAULT: tether accepted a client while the device was absent")
                break
            await asyncio.sleep(0.05)

        # INV 3 from the only side that can judge it: the client. A stall here would be the
        # real failure — a request that neither completes nor fails is what a bridge that goes
        # quiet does to a client's timers, and it is what INV 3 says must never happen.
        if client == "zigpy":
            try:
                await asyncio.wait_for(app.load_network_info(), timeout=10)
                findings.append("FAULT: zigpy answered a request with no dongle behind it")
            except asyncio.TimeoutError:
                findings.append(
                    "FAULT: zigpy's request neither completed nor failed — the client was left "
                    "stalled, which is exactly what INV 3 exists to prevent"
                )
            except Exception as exc:  # noqa: BLE001 — the kind is the finding, and it was unknown
                # Observed, not asserted: this is somebody else's error surface and the first
                # look at it. INV 3 leans on "a clean close is the trigger for recovery paths
                # the clients already test" — zigpy does notice, and the finding below is *how*.
                findings.append(
                    f"zigpy fails a request after the close with {type(exc).__name__}: "
                    f"{exc or '(no message)'}"
                )
        else:
            # herdsman's own account, read off the client that was holding the port. Two
            # separate questions, and the second is the one INV 3 is about.
            notice = await observer.until("disconnected")
            if notice["fired"]:
                findings.append(
                    "herdsman's adapter emitted `disconnected` "
                    f"{notice['after_ms']}ms after the dongle went — the event zigbee2mqtt "
                    "hangs its restart on"
                )
            else:
                findings.append(
                    "FAULT: herdsman's adapter never emitted `disconnected`, so nothing above "
                    "it would ever learn the coordinator had gone"
                )
            after_close = await observer.until("request_after_close")
            if after_close["outcome"] == "hung":
                findings.append(
                    "FAULT: herdsman's request neither completed nor failed — the client was "
                    "left stalled, which is exactly what INV 3 exists to prevent"
                )
            elif after_close["outcome"] == "answered":
                findings.append("FAULT: herdsman answered a request with no dongle behind it")
            else:
                findings.append(
                    f"herdsman fails a request after the close with {after_close['detail']}"
                )
            await observer.stop()

        # ─── the replug ───────────────────────────────────────────────────────────────────
        print("plugging it back in — same stick, new node, same name: " + stick.replug(), flush=True)
        back = time.monotonic()

        # On Windows the name is a COM number, assigned rather than intrinsic, so this wait is
        # also the measurement of whether a replug keeps it: a tether that never reopens here
        # is one whose configured name now points at nothing.
        served = await until(lambda: not refused(endpoint), "tether to reopen and serve", timeout=30.0)
        findings.append(
            f"tether reopened {stick.name} on its own and served again, {served:.1f}s after the "
            f"replug ({back - pulled:.1f}s after the unplug)"
        )

        if client == "zigpy":
            after = await client_for(endpoint)
            recovered = facts(after)
            await after.shutdown(db=False)
        else:
            # A fresh client, not the one that watched the unplug: herdsman never reconnects an
            # adapter by itself — the controller above it emits `disconnected` and zigbee2mqtt
            # exits so its supervisor restarts it — so a new process *is* what
            # recovery looks like on this side.
            fresh = await HerdsmanObserver.start(endpoint, watch=False)
            try:
                recovered = await fresh.until("facts")
            finally:
                await fresh.stop()
        if recovered != before:
            findings.append(f"FAULT: the network changed across the replug: {before} → {recovered}")
        else:
            findings.append(
                "the network came back identical, every fact the client can read: "
                + ", ".join(sorted(before))
            )
        return report(findings, tether.output())
    except (TimeoutError, subprocess.CalledProcessError) as exc:
        findings.append(f"FAULT: {exc or 'a step timed out with no message'}")
        return report(findings, tether.output())
    finally:
        tether.stop()
        stick.close()


def report(findings: list[str], tether_said: str) -> int:
    print()
    faults = [f for f in findings if f.startswith("FAULT")]
    for finding in findings:
        print(("  ✗ " if finding.startswith("FAULT") else "  · ") + finding)
    print("\n--- what tether said ---")
    print(tether_said.strip())
    if faults:
        print(f"\n{len(faults)} fault(s)", file=sys.stderr)
        return 1
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--tether", type=pathlib.Path, default=pathlib.Path("./briard-tether"))
    parser.add_argument(
        "--client",
        default="zigpy",
        choices=("zigpy", "herdsman"),
        help="which client stack holds the dongle when it is pulled (default: zigpy)",
    )
    parser.add_argument(
        "--device",
        help="a REAL coordinator's port as tether will see it (a /dev/serial/by-id/… path here, "
             "a COMn name on the Windows machine) instead of the emulator on a pty; needs "
             "--unplug and --replug",
    )
    parser.add_argument(
        "--unplug",
        help="a shell command that removes that device — `device_del` on a hypervisor's "
             "monitor, a smart hub's port off, or `read -p 'pull it, then Enter'`",
    )
    parser.add_argument("--replug", help="a shell command that puts it back")
    args = parser.parse_args()

    if args.device and not (args.unplug and args.replug):
        parser.error("--device needs --unplug and --replug: something has to pull a real stick")
    if not args.device:
        # The unplug it stages is a pty closing, which has no equivalent on the far machine's
        # COM1: over there it is a real device, hot-removed by the hypervisor —
        # which is the --device form.
        rig.local_only("the unplug scenario's emulator form")
    tether = rig.binary(args.tether)

    with tempfile.TemporaryDirectory(prefix="tether-unplug-") as tmp:
        return asyncio.run(run(tether, args.client, pathlib.Path(tmp),
                               args.device, args.unplug, args.replug))


if __name__ == "__main__":
    sys.exit(main())
