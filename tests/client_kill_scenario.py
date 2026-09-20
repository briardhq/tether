# SPDX-License-Identifier: GPL-3.0-or-later
#
# ⚠️ GPL-3 with the rest of this directory: it imports zigpy-znp's emulator, which is GPL-3
# Nothing here is linked into the briard-tether binary or ships in any artifact.
"""The client dies without warning while it is using the radio.

The third and last survival scenario in this repo's definition of done, and the only one that
had no real client behind it: *client kill*. Takeover is tested at tier 1 — over a `net.Pipe`,
and through `serve` with a scripted responder — but never with a real Zigbee stack
that was **killed mid-operation** — which is the case that actually happens. Z2M's container is
restarted, Home Assistant reloads, a supervisor OOM-kills something, and a successor arrives
seconds later while the radio is part-way through answering the corpse.

Two ways for a client to stop being there, and they are not the same event:

  `--event kill`    SIGKILL. ⚠️ Measured rather than assumed, and it is not the clean FIN you
                    would expect: the dead client's receive buffer still held frames it never
                    read, and Linux answers that with an **RST**. So a tether on Linux meets
                    a killed client on its *error* path — `read failed … connection reset by
                    peer` — and not on the orderly-shutdown path a quit client takes. ⚠️ **On
                    Windows the same kill arrived as an orderly close**, so which
                    path runs is not a property of the event; see `report` below. The radio
                    is still answering questions nobody will collect, and what the successor
                    must not get is the tail of somebody else's conversation.
  `--event freeze`  SIGSTOP. The process stops reading and stops answering, and its socket
                    stays wide open. This is a paused VM, a wedged host, a cable pulled at the
                    other end — and it is the case tether's kick-old takeover exists for
                    (INV 4, the lifecycle failure class): the incumbent cannot be waited out, so a
                    successor must be able to displace it. A forwarder that let the frozen
                    client keep the radio is the failure that makes people reboot the host.

What is asserted is tether's: the successor is accepted, it is served, and the coordinator it
finds is the one that was there before — same identity, same network, undisturbed. Then tether's
own log is read for the two things the clients cannot see — that the port was opened exactly
once, and that the frozen incumbent was *displaced* rather than waited out.

**The victim is a herdsman client** (`herdsman_client.js --hold`, kept busy with back-to-back
backups so the signal lands inside a multi-frame sequence). It is the one of the two stacks that
already runs as a subprocess, and a process is what you can kill. The successor may be either
stack — `--successor zigpy` is the more interesting run, since it asks whether a radio abandoned
by one implementation is clean for a different one.

    go build -o briard-tether ./cmd/briard-tether
    python3 tests/client_kill_scenario.py
    python3 tests/client_kill_scenario.py --event freeze --successor zigpy

The victim and its successor are always here; `TETHER_WINDOWS_SSH` moves the tether they fight
over onto a Windows machine. Killing a client is a thing this machine does to its
own process, so nothing about the scenario changes over there.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import pathlib
import signal
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


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


async def until(predicate, what: str, timeout: float = 30.0) -> float:
    """Wait for something, and return how long it took — the number is half the point here."""
    started = time.monotonic()
    while time.monotonic() - started < timeout:
        if predicate():
            return time.monotonic() - started
        await asyncio.sleep(0.05)
    raise TimeoutError(f"timed out after {timeout:g}s waiting for {what}")


async def read_until(proc, key: str, timeout: float = 120.0) -> object:
    """Read the client's JSON lines until it emits `key`.

    Async throughout because the emulator shares this event loop: a blocking read here stops the
    coordinator answering, which presents exactly like a tether bug. See `HerdsmanObserver` in
    unplug_scenario.py, where that cost an afternoon.
    """

    async def loop() -> object:
        while True:
            line = await proc.stdout.readline()
            if not line:
                raise TimeoutError(f"the herdsman client exited before saying {key!r}")
            message = json.loads(line)
            if "error" in message:
                raise TimeoutError(f"the herdsman client failed: {message['error']}")
            if key in message:
                return message[key]

    try:
        return await asyncio.wait_for(loop(), timeout=timeout)
    except asyncio.TimeoutError:
        raise TimeoutError(f"the herdsman client never said {key!r}") from None


async def herdsman_facts(port: int, hold: bool):
    """Start a herdsman client, wait for it to be attached, and hand back what it reported."""
    argv = ["node", str(HERE / "herdsman_client.js"), f"tcp://{rig.ADDR}:{port}"]
    if hold:
        argv.append("--hold")
    proc = await asyncio.create_subprocess_exec(*argv, stdout=asyncio.subprocess.PIPE)
    facts = await read_until(proc, "facts")
    return proc, facts


async def zigpy_facts(port: int) -> dict[str, str]:
    """The same question asked by the other stack, for the cross-implementation run."""
    app = ControllerApplication(
        {
            conf.CONF_DEVICE: {conf.CONF_DEVICE_PATH: f"socket://{rig.ADDR}:{port}"},
            zigpy.config.CONF_NWK_BACKUP_ENABLED: False,
        }
    )
    await app.startup(auto_form=False)
    try:
        net, node = app.state.network_info, app.state.node_info
        return {
            "ieee": str(node.ieee),
            "pan_id": f"0x{net.pan_id:04X}",
            "channel": str(net.channel),
        }
    finally:
        await app.shutdown(db=False)


def comparable(facts: dict) -> dict[str, str]:
    """The facts the two stacks both report, so a cross-stack run can compare at all.

    herdsman reports nine and zigpy three; the three are the ones that matter here — a
    coordinator that was reset would change its network, and one that was reopened would change
    nothing but would show on the card instead. Normalised because the two spell them
    differently: zigpy gives an IEEE as `00:12:4b:…`, herdsman as `0x00124b…`.
    """
    ieee = str(facts["ieee"]).lower().removeprefix("0x").replace(":", "")
    return {
        "ieee": ieee,
        "pan_id": str(facts["pan_id"]).upper().replace("0X", "0x"),
        "channel": str(facts["channel"]),
    }


async def run(tether_binary: pathlib.Path, event: str, successor: str,
              workdir: pathlib.Path) -> int:
    link = workdir / "coordinator"
    port = free_port()

    wire = rig.wire(link)
    # The herdsman replies, because the victim is a herdsman client (see the module docstring).
    device = type(
        "HerdsmanFormedLaunchpadCC26X2R1",
        (HerdsmanReplies, znp_fixtures.FormedLaunchpadCC26X2R1),
        {},
    )
    server = build_server(device, wire.device)
    bind(server, wire)

    tether = rig.Tether(
        tether_binary,
        {
            "device": wire.device,
            "radio": "znp",
            "listen": rig.listen(port),
            "advertise": False,
        },
        workdir,
    )

    findings: list[str] = []
    victim = None
    try:
        await until(lambda: not tether.died() and port_open(port), "tether to accept")

        victim, before = await herdsman_facts(port, hold=True)
        await read_until(victim, "ready")
        print(f"a real herdsman client holds the radio; the network is {comparable(before)}",
              flush=True)

        # Busy, and proved busy rather than assumed: the signal has to land inside a multi-frame
        # sequence for this to be a kill *mid-operation* and not a slow disconnect.
        await read_until(victim, "busy")
        print(f"it is mid-backup, so the {event} lands between frames\n", flush=True)

        # ─── the client stops being there ─────────────────────────────────────────────────
        if event == "kill":
            victim.send_signal(signal.SIGKILL)
            await victim.wait()
            findings.append("the client was killed mid-operation, leaving replies it never read")
        else:
            victim.send_signal(signal.SIGSTOP)
            findings.append(
                "the client was frozen mid-operation: its socket is still open and it will "
                "never read another byte, which is a paused host and not a disconnect"
            )

        # ─── the successor ────────────────────────────────────────────────────────────────
        # Immediately, and that is the point. Any keepalive is 15 s away, so for the frozen case
        # nothing can have reaped the incumbent yet: being served here *is* kick-old takeover
        # (INV 4), and the radio being unchanged is INV 1 across it.
        print(f"a {successor} client arrives while the incumbent is still there", flush=True)
        if successor == "zigpy":
            after = await zigpy_facts(port)
        else:
            proc, after = await herdsman_facts(port, hold=False)
            await proc.wait()

        if comparable(after) != comparable(before):
            findings.append(
                f"FAULT: the coordinator changed across the {event}: "
                f"{comparable(before)} → {comparable(after)}"
            )
        else:
            findings.append(
                f"the {successor} successor was served and found the same coordinator: "
                + ", ".join(f"{k} {v}" for k, v in comparable(after).items())
            )
        return report(findings, tether.output(), event)
    except TimeoutError as exc:
        findings.append(f"FAULT: {exc or 'a step timed out with no message'}")
        return report(findings, tether.output(), event)
    finally:
        if victim is not None and victim.returncode is None:
            # SIGCONT first, or a stopped process cannot act on anything else.
            victim.send_signal(signal.SIGCONT)
            victim.kill()
            await victim.wait()
        tether.stop()
        wire.close()


def port_open(port: int) -> bool:
    with socket.socket() as s:
        s.settimeout(0.2)
        return s.connect_ex((rig.ADDR, port)) == 0


def report(findings: list[str], tether_said: str, event: str) -> int:
    # What tether's own log settles, which the findings above cannot. The report card would say
    # it more directly, but its socket lives under /run and this scenario runs unprivileged —
    # so the log is the record, and it is enough for both questions.
    opens = tether_said.count("open at ")
    if opens == 1:
        findings.append("the port was opened once and never reopened: the client never reached "
                        "the radio (INV 1)")
    else:
        findings.append(f"FAULT: the device was opened {opens} times — a client lifecycle "
                        "reached the radio, which is INV 1 broken")

    took_over = "takes over from" in tether_said
    if event == "freeze" and not took_over:
        findings.append("FAULT: the successor was served without displacing the frozen "
                        "incumbent, so the takeover path never ran — INV 4 unproven here")
    elif event == "freeze":
        findings.append("the successor displaced the frozen incumbent rather than waiting for "
                        "it (INV 4, kick-old): no keepalive could have reaped it in this time")
    elif took_over:
        findings.append("the successor arrived before tether had processed the reset, so it "
                        "displaced the dead client instead of replacing it — both are correct")
    else:
        # **How it ended is read out of the log rather than assumed, because it differs by
        # platform** (measured): the same SIGKILL to the same client
        # reaches a Linux tether as `read failed … connection reset by peer` — the dead client
        # still held frames it never read, and Linux answers that with an RST — and reached a
        # tether on Windows as an ordinary orderly close. Whether that is the other stack
        # reporting an RST as end-of-stream or simply less data in flight over a slower wire is
        # not settled here. It changes nothing that this scenario asserts: either way the client
        # is closed, the radio is untouched and the successor is served.
        how = "with an error (RST)" if "read failed" in tether_said else "as an orderly close"
        findings.append(f"the killed client's connection ended {how} before the successor "
                        "arrived, so the successor was simply the next one in")

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
        "--event",
        default="kill",
        choices=("kill", "freeze"),
        help="how the client stops being there (default: kill, a SIGKILL mid-operation)",
    )
    parser.add_argument(
        "--successor",
        default="herdsman",
        choices=("herdsman", "zigpy"),
        help="which stack arrives next (default: herdsman; zigpy asks the cross-stack question)",
    )
    args = parser.parse_args()

    tether = rig.binary(args.tether)

    with tempfile.TemporaryDirectory(prefix="tether-kill-") as tmp:
        return asyncio.run(run(tether, args.event, args.successor, pathlib.Path(tmp)))


if __name__ == "__main__":
    sys.exit(main())
