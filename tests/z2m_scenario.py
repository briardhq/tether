# SPDX-License-Identifier: GPL-3.0-or-later
#
# ⚠️ GPL-3 by import, not only by association: the network read below drives
# zigpy and zigpy-znp, both GPL-3, and a module that imports a GPL-3 library is plausibly a
# derivative work. zigbee2mqtt itself is GPL-3 and is driven as a subprocess, at
# arm's length, never linked. Nothing here reaches the briard-tether binary or any release
# artifact.
"""A real zigbee2mqtt through tether: the run that forms a network, and every run after.

`--network` is the whole difference between the two arms, and it decides what the generated
`configuration.yaml` says about the network:

  `--network shipped`      **The default.** Z2M's own fixed literals — pan id 6754,
                           extended pan id `dd:dd:…`, channel 11, the key that ships in every
                           install — which is to say *nothing written at all*, because those are
                           the defaults. Point it at a blank coordinator and it commissions one
                           across tether's pipe. The first start must log `(reset)` and the
                           second, on the same data dir, `(resumed)`. **Both are passes.**
  `--network coordinator`  The harness reads the four values off the coordinator
                           first — through tether, with a zigpy client — and writes them into
                           the config, so herdsman's `configMatchesAdapter` is satisfied and
                           **`resumed` is the only branch that passes**. A `reset` or a
                           `restored` is a FAULT: nothing was necessarily broken by it, but the
                           run measured the wrong thing. Resumption is the branch under test,
                           because it is what every start after day one does.

And in `coordinator` mode two more things happen to a Z2M that is up and attached, because
"the network is still there afterwards" is the assertion they share:

  the client is killed     `SIGKILL`, no shutdown, and a fresh Z2M arrives while tether is
                           still holding the corpse's socket. INV 4 with a real client on it.
  the dongle is pulled     herdsman emits `disconnected`, Z2M logs `Adapter disconnected,
                           stopping` and exits **2** for its supervisor; this harness is that
                           supervisor. The stick comes back, tether reopens it, and the Z2M
                           that follows must resume the same network.

`--migrate` is a third arm rather than a third `--network`, because it is the only one that
stages a **before**: the coordinator on a pty with nothing in front of it, a real Z2M pointed
straight at that pty, two devices written into its database by hand, and only then a tether and
a changed `serial.port`. It is what an emulator can honestly say about a migration, which is
that the network resumed and the database survived, and not that a device still answers,
because there is no device. `run_migration` carries the order and the reasons.

`--discover` swaps `tcp://host:port` for `mdns://zigbee-coordinator`, which is the other half
of discovery: the client browses for the coordinator instead of being handed an address, and is
told the radio family by our TXT record rather than by its own config. It re-execs the whole
scenario into a network namespace with nothing but loopback in it, for the reasons
`run_gate.into_a_private_network` sets out — herdsman takes the *first* coordinator that
answers, so a run on a real LAN would be measuring somebody else's stick.

Order matters and is not obvious: **mosquitto, tether, the network read, then Z2M.** Z2M will
not start without a broker, and it opens the radio while starting.

Usage, no dongle needed:

    nix develop --command go build -o briard-tether ./cmd/briard-tether
    nix develop --command python3 tests/z2m_scenario.py
    nix develop --command python3 tests/z2m_scenario.py \\
        --network coordinator --device FormedLaunchpadCC26X2R1
    nix develop --command python3 tests/z2m_scenario.py --network coordinator --discover \\
        --device FormedLaunchpadCC26X2R1

And against a real stick, as yourself, with the tty group and nothing else:

    nix develop --command python3 tests/z2m_scenario.py --network coordinator --discover \\
        --stick /dev/serial/by-id/usb-ITead_Sonoff_…-port0

`--device` picks the emulator fixture; the default is the **blank** one, which is the only kind
the `shipped` arm is about, and `coordinator` wants a formed one.

⚠️ **Pointing the `shipped` arm at a *formed* fixture fails, and not in the way you would
guess.** herdsman does choose to commission — the vanilla config disagrees with the radio, which
is exactly the danger `coordinator` is built to avoid — but the commissioning then dies with
`timed out waiting for nib to settle`, because zigpy-znp's emulator answers
`BDBStartCommissioning` on an already-formed fixture with `NetworkRestored` and leaves the NIB
alone. So **the emulator cannot re-form a network it already has**, and that is also what a
mistake in `coordinator`'s network-reading step looks like: get one of the four values wrong,
herdsman decides to commission, and the run dies there rather than reporting a mismatch.
The cheaper falsification runs every time in the `shipped` arm anyway: the second start, against
the very same coordinator now carrying a network, reports `resumed` rather than `reset`, so the
strategy detection does tell the two states apart.

`--stick` is how either arm reaches **real hardware**: the coordinator is a dongle instead of
the emulator, and this harness starts the tether in front of it exactly as it does for the
emulator. It needs no privilege beyond the tty group, which is the whole reason it
can be one command — a *root* tether cannot be put inside the unprivileged namespace `--discover`
makes, so `--stick --discover` would otherwise be a four-part recipe done by hand. ⚠️ In the
`shipped` arm it re-forms that stick's network: fine on a disposable test dongle and on
nobody else's.

`--unplug`/`--replug` are what let a real stick reach case (5), since nothing in a test process
can pull a dongle by itself. The plug event a Linux host can stage without hands is a USB
deauthorize/reauthorize, and it is the **one thing in a run of this harness that wants root**,
a write to sysfs being something no group confers:

    --unplug "sudo sh -c 'echo 0 > /sys/bus/usb/devices/<node>/authorized'"
    --replug "sudo sh -c 'echo 1 > /sys/bus/usb/devices/<node>/authorized'"

Without them a `--stick` run does cases (1) to (4) and says case (5) was skipped.

`--radio`/`--port` are the other way round — Z2M and the network read are pointed at a tether
*somebody else* already started, which is what reaches a tether on the far machine, or one
started by hand for a case this file does not have.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import pathlib
import random
import shutil
import socket
import subprocess
import sys
import tempfile

HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

import zigpy.config  # noqa: E402
from zigpy.exceptions import NetworkNotFormed  # noqa: E402
import zigpy_znp.config as conf  # noqa: E402
from zigpy_znp.zigbee.application import ControllerApplication  # noqa: E402

import rig  # noqa: E402
from run_gate import (  # noqa: E402
    Coordinator,
    Process,
    coordinator_argv,
    free_port,
    into_a_private_network,
    wait_for,
)

# The line Z2M logs at *info* once herdsman is up, carrying the strategy it chose. herdsman itself
# logs the strategy at debug only, so this is the one that matters and the reason no debug logging
# is configured here (measured against 2.14.0 / herdsman 10.9.1).
STARTED = "zigbee-herdsman started"

# What `--wrong-radio` makes tether claim in front of the Z-Stack emulator (case (c)). `ezsp` and
# not `deconz` on purpose: herdsman maps `ezsp` onto its `ember` driver, which is a *supported*
# driver doing real reads over the wrong protocol, and nothing on this path ever asks the radio
# what it actually is — which is what makes it the case worth staging. `deconz` would be caught
# earlier and by something else, and would prove less.
WRONG_RADIO = "ezsp"

# The last line of a Z2M start, logged after every extension is up and the bridge topics have
# been published. It is the only *per-process* way to tell "this run is serving" from "the
# previous run's retained topics are still on the broker" — which matters wherever a case
# restarts Z2M over a data dir and a broker that both remember the run before.
UP = "Zigbee2MQTT started!"

# What Z2M says on its way out when the adapter went away under it, and the exit code it leaves
# for a supervisor. ⚠️ **Not to be confused with the exit code of a clean stop**, which is also
# non-zero for a reason that has nothing to do with anything (see `stop` below): this one is the
# restart trigger and that one is winston losing a race.
DISCONNECTED = "Adapter disconnected, stopping"
SUPERVISOR_RESTART_CODE = 2


def broker_config(port: int) -> str:
    """The smallest broker that will serve one client on loopback and write nothing down.

    ⚠️ **`user root` is `--discover`'s tax and not a security opinion.** That arm re-execs into a
    user namespace with `--map-root-user`, so everything inside it has euid 0 — and mosquitto
    started as root drops privileges to `mosquitto`, or to `nobody`, neither of which is mapped
    in that namespace. It logs `Error setting groups whilst dropping privileges` and *terminates
    after opening its listener*, so the harness's wait on "Opening ipv4 listen socket" sails past
    it and Z2M dies two minutes later with `MQTT failed to connect … ECONNREFUSED` — a fault
    reported three steps from its cause. Outside the namespace this line does nothing at all:
    mosquitto only drops privileges when it is actually root.
    """
    user = "user root\n" if os.geteuid() == 0 else ""
    return f"listener {port} 127.0.0.1\nallow_anonymous true\npersistence false\n{user}"


# ⚠️ **`reset` is herdsman's own word, and on one adapter it is a lie by omission.**
# `Adapter.start()` is typed `StartResult = "resumed" | "reset" | "restored"`, and six of the seven
# implementations report all three honestly — z-stack from `determineStrategy`, and ember, ezsp,
# zboss, zigate and zoh each from their own "form from config" branch.
#
# **deconz re-points the radio and says `resumed` anyway.** Its driver compares the firmware's
# parameters against `configuration.yaml` and, on any mismatch, runs `reconfigureNetwork()`:
# disconnect, write `NWK_PANID`, `APS_USE_EXTENDED_PANID`, `APS_CHANNEL_MASK` and `STK_NETWORK_KEY`
# from the config, reconnect, which stores them to NVRAM. The start result never mentions it.
# **Measured on a ConBee:** a radio carrying pan 8738 / ext `0x1122334455667788`,
# handed a fresh Z2M asking for pan 13107 / ext `0xaabbccddeeff0099`, came back on the new network
# with `bridge/info` confirming it — and `zigbee-herdsman started (resumed)` in the log.
#
# So on deconz the start result is **not a safety signal**, and a harness that reads it as one is
# measuring nothing. Comparing the network before and after is the only check that works there.
# This scenario therefore does not assert on the strategy for such an adapter; what it still
# measures is that a real Z2M drives that radio through tether at all.
#
# ⚠️ It is also why `--network coordinator` refuses such an adapter outright (see `main`): that
# arm's entire design is `resumed`-or-FAULT, and here that check passes while the network is
# being rewritten underneath it. Giving this scenario a non-zstack case means replacing the log
# line with a before-and-after comparison of the network — which is the check that caught deconz
# in the first place, and is not the check this file makes.
SILENT_ABOUT_FORMING = {"deconz"}

# Channel 20 for the real-radio path: it is the one the test Sonoff already lived on, the
# emulator's is 15 and Z2M's default is 11, so all three stay distinct and a mix-up fails
# loudly.
ON_AIR_CHANNEL = 20

# The extended PAN id every Z2M that never changed one has. ⚠️ It is not just a default here, it
# is a hole in the assertion: `parseConfigNetworkOptions` replaces it with the adapter's own IEEE
# address and sets `hasDefaultExtendedPanId`, which makes `configMatchesAdapter` **skip the
# extended-PAN-id comparison entirely**. So a coordinator that genuinely carries it leaves the
# `coordinator` arm comparing three values instead of four, and the run says so rather than
# quietly claiming the fourth.
DEFAULT_EXT_PAN_ID = [0xDD] * 8


def on_air_network() -> str:
    """Fresh network identifiers for a real radio, generated per run.

    ⚠️ **Fresh, not fixed, and the reason is the assertion rather than tidiness.** Reuse the same
    pan id twice and the second run finds a coordinator already carrying it, so herdsman resumes
    and the first-run case silently stops testing anything — which is exactly what happened the
    first time this was tried on hardware. Generating them means every hardware run is a genuine
    commissioning. The 64-bit extended pan id makes a clash with a neighbour effectively
    impossible; the 16-bit pan id can still collide, and if it ever does the symptom is the
    100-second `stateChangeInd` timeout documented below.
    """
    pan = random.randrange(1, 0xFFFE)
    extended = [random.randrange(0, 0x100) for _ in range(8)]
    return ("advanced:\n"
            f"  pan_id: {pan}\n"
            f"  ext_pan_id: [{', '.join(hex(b) for b in extended)}]\n"
            f"  channel: {ON_AIR_CHANNEL}\n")


async def _interrogate(device: str) -> dict:
    """One zigpy client reading what herdsman is about to compare against.

    ⚠️ **It takes a zigpy device path rather than an address**, and the caller builds it:
    `socket://host:port` for a coordinator behind a tether, a plain `/dev/…` path for one that
    has nothing in front of it. That second form is what the migration arm needs — its whole
    premise is an install that predates tether — and hardcoding `socket://` here would make the
    reader unable to see such a coordinator at all.

    `startup(auto_form=False)` is `client_gate.py`'s call and carries its guarantee: it reads
    NVRAM and brings the coordinator up on the network that is already there. It cannot form
    one — `auto_form` is never anything but False — so this is safe in front of a stick that is
    carrying somebody's devices.
    """
    app = ControllerApplication({
        conf.CONF_DEVICE: {conf.CONF_DEVICE_PATH: device},
        # This run is not anybody's backup, and nothing here should write one into the repo.
        zigpy.config.CONF_NWK_BACKUP_ENABLED: False,
    })
    await app.startup(auto_form=False)
    try:
        net = app.state.network_info
        return {
            "pan_id": int(net.pan_id),
            # zigpy's `ExtendedPanId` is an EUI64, which holds its bytes in wire order and only
            # *prints* them reversed. Iterating it therefore gives the same byte order herdsman
            # reads out of the NIB. ⚠️ It would not matter much if it did not: herdsman tolerates
            # a reversed extended PAN id on purpose, so a genuine mismatch in the read will
            # surface in the pan id or the key and never here.
            "ext_pan_id": list(net.extended_pan_id),
            "channel": int(net.channel),
            # ⚠️ **The one value `describe` will not give us, and the run cannot do without.**
            # Both client gates redact the key deliberately, because their output is meant to be
            # pasted into a runbook. But `configMatchesAdapter` compares the configured key
            # against the adapter's *active and alternate* key info, so a config without it
            # never resumes at all. It stays inside this process and reaches only the generated
            # config in a temp dir — it is never printed.
            "network_key": list(net.network_key.key),
        }
    finally:
        await app.shutdown(db=False)


def read_network(device: str) -> dict:
    """The network the coordinator is actually carrying, read before Z2M is told anything.

    This is the step that makes the `coordinator` arm mean what it says. Without it the config
    carries Z2M's literals, herdsman finds they disagree with the radio, and it commissions —
    which is a pass in the other arm and the failure this one exists to rule out.
    """
    try:
        return asyncio.run(_interrogate(device))
    except NetworkNotFormed:
        # zigpy reads the NIB first and stops there, so a blank coordinator has nothing to say
        # about itself. Said here rather than left as a traceback, because it is the one
        # operator mistake this arm invites: `--network coordinator` against the blank fixture.
        raise RuntimeError(
            "this coordinator carries no network, so there is nothing for the config to resume. "
            "--network coordinator wants a formed one (FormedLaunchpadCC26X2R1, or a stick that "
            "has been commissioned); --network shipped is the arm that forms one."
        ) from None


def network_block(net: dict) -> str:
    """All four values, and **all four is not a stylistic choice.**

    A config left carrying the shipped `dd:dd:…` extended PAN id makes herdsman skip that
    comparison entirely, so a half-substituted config looks *closer* to matching than it is —
    and a config missing the network key never matches at all. Substituting three of the four
    is the failure mode this function exists to not have.
    """
    return ("advanced:\n"
            f"  pan_id: {net['pan_id']}\n"
            f"  ext_pan_id: [{', '.join(hex(b) for b in net['ext_pan_id'])}]\n"
            f"  channel: {net['channel']}\n"
            f"  network_key: [{', '.join(hex(b) for b in net['network_key'])}]\n")


def z2m_config(broker: int, radio: str, adapter: str, advanced: str) -> str:
    """Deployment wiring, and `advanced` is the only thing above it the arms disagree about.

    ⚠️ Resist adding to this. Every key that is not about *where the broker and the radio are*
    is a departure from what a new install has, and in the `shipped` arm the departure is the
    thing under test — which is why `advanced` is empty there and that emptiness is the test.

    ⚠️ **`serial.adapter` is required for `tcp://` and unread for `mdns://`, and that is the
    discovery contract showing through.** herdsman refuses a bare TCP path with
    "Cannot discover TCP adapters at this time. Specify valid 'adapter' and 'port'" — there is
    nothing on the wire to tell it what the radio is. Over mDNS there is: our TXT record carries
    `radio_type`, so the client is told rather than configured, which is the whole point of the
    advert. Measured.

    ⚠️ **Unread, not forbidden.** `discoverAdapter` documents the argument as *"mDNS: Unused"*
    and `findMdnsAdapter` never looks at it; a config carrying both an `mdns://` port and an
    `adapter` key starts normally. Measured by the migration arm, which leaves the key exactly
    where a migrating user leaves it. This function still omits
    it for an `mdns://` port, which is the right config to *generate* and not a requirement.
    """
    config = (
        "mqtt:\n"
        f"  server: mqtt://127.0.0.1:{broker}\n"
        "serial:\n"
        f"  port: {radio}\n"
    )
    if not radio.startswith("mdns://"):
        config += f"  adapter: {adapter}\n"
    return config + advanced


def advanced_block(network: str, on_air: bool, device: str) -> str:
    """Which network the config carries, which is what `--network` selects.

    ⚠️ **On a real radio the `shipped` arm cannot use the shipped defaults, and that is physics
    rather than anything tether does.** Z2M's defaults are fixed literals — pan id 6754,
    extended pan id `dd:dd:dd:dd:dd:dd:dd:dd`, channel 11 — which every install that never
    changed them also uses. Commissioning with them on that stick fails after 100 s with
    "network commissioning timed out - most likely network with the same panId or extendedPanId
    already exists nearby" (measured), and the same commissioning over the same
    tether succeeds *at once* with identifiers nobody else is using. So the vanilla-default run
    is the emulator's, where there is no air to share and the result is deterministic; the
    radio's job is to prove that forming a network across the pipe works at all.
    """
    if network == "shipped":
        return on_air_network() if on_air else ""
    net = read_network(device)
    print(f"  the coordinator carries pan 0x{net['pan_id']:04X} on channel {net['channel']}, "
          f"and the config will say so", flush=True)
    if net["ext_pan_id"] == DEFAULT_EXT_PAN_ID:
        print("  ⚠️ its extended pan id is the shipped dd:dd:…, which herdsman substitutes the "
              "adapter's IEEE for and then stops comparing — three values, not four", flush=True)
    return network_block(net)


class Zigbee2mqtt:
    """One Z2M run over one data dir. Started more than once on purpose: that is the test."""

    def __init__(self, data: pathlib.Path, log: pathlib.Path, name: str) -> None:
        self.name = name
        self.log_path = log
        self.handle = log.open("wb")
        self.proc = subprocess.Popen(
            [shutil.which("zigbee2mqtt") or "zigbee2mqtt"],
            stdout=self.handle, stderr=subprocess.STDOUT,
            # ⚠️ **`cwd` is load-bearing, and it is somebody else's bug.** Z2M's launcher decides
            # whether its prebuilt `dist` is stale by comparing `dist/.hash` against
            # `git rev-parse --short=8 HEAD` — run with **no cwd of its own**, so it inherits
            # ours. A packaged install ships `.hash` as the literal `unknown`, which is right for
            # a build made outside git; but run it from inside *any* git checkout and the command
            # succeeds, the hashes differ, and it tries to rebuild itself with `pnpm run prepack`
            # — which a packaged install has no pnpm for. Its own `build()` passes
            # `cwd: __dirname`; `currentHash()` forgot to. Running from the data dir puts us
            # outside a repo, git fails, and the comparison reaches the `unknown` that matches.
            # (Measured against 2.14.0.)
            cwd=str(data),
            env=dict(
                os.environ,
                ZIGBEE2MQTT_DATA=str(data),
                # ⚠️ Without this, a config Z2M rejects makes it serve a *failure page* on
                # 0.0.0.0:8080 and block there instead of exiting — a hung harness and a listener
                # on the machine that nobody asked for. Onboarding itself is already skipped,
                # because a configuration.yaml exists before this runs.
                Z2M_ONBOARD_NO_SERVER="1",
            ),
        )

    def output(self) -> str:
        if not self.handle.closed:
            self.handle.flush()
        return self.log_path.read_text(errors="replace")

    def strategy(self) -> str | None:
        """`reset`, `restored` or `resumed` — whichever herdsman chose, as Z2M reported it."""
        for line in self.output().splitlines():
            if STARTED in line and "(" in line:
                return line.rsplit("(", 1)[1].split(")")[0]
        return None

    def up(self) -> bool:
        """Finished starting: every extension running and the bridge topics published."""
        return UP in self.output()

    def died(self) -> bool:
        return self.proc.poll() is not None

    def kill(self) -> None:
        """No shutdown, no handler, no chance to close anything — a supervisor's OOM killer,
        or a container torn down. The socket to tether is left for the kernel to reset."""
        self.proc.kill()
        self.proc.wait(timeout=10)
        self.handle.close()

    def stop(self) -> int | None:
        """SIGTERM, which is what a supervisor sends and what Z2M installs a handler for."""
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=30)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=10)
        if not self.handle.closed:
            self.handle.close()
        return self.proc.returncode


def start_z2m(data: pathlib.Path, workdir: pathlib.Path, name: str,
              findings: list[str], runs: list[Zigbee2mqtt],
              expect_start: bool = True) -> tuple[Zigbee2mqtt, str]:
    """Start it and wait for the one line that says which branch herdsman took.

    `expect_start=False` is case (c), where **not starting is the pass**: a wrong `radio_type`
    must stop zigbee2mqtt before it writes anything. The faults below are the right answer
    everywhere else and the wrong one there, so the caller says which run it is rather than this
    function guessing from the outcome it is trying to measure.
    """
    z2m = Zigbee2mqtt(data, workdir / f"{name}.log", name)
    runs.append(z2m)
    try:
        wait_for(lambda: z2m.strategy() is not None or z2m.died(),
                 f"{name} to report a startup strategy", timeout=180.0)
    except TimeoutError:
        if expect_start:
            findings.append(f"FAULT: {name} never reported a startup strategy")
        return z2m, "none"
    chosen = z2m.strategy()
    if chosen is None:
        if expect_start:
            findings.append(f"FAULT: {name} exited before starting herdsman "
                            f"(exit {z2m.proc.returncode})")
        return z2m, "none"
    print(f"  {name}: herdsman started ({chosen})", flush=True)
    return z2m, chosen


def got_off_the_ground(z2m: Zigbee2mqtt, chose: str) -> None:
    """Stop the whole run when a Z2M never reached herdsman at all.

    Everything after such a start is a question about a process that is not there, and asking it
    anyway costs a timeout per step and buries the one finding that matters — which is what the
    first falsification run of this arm looked like: ten faults, nine of them consequences.
    """
    if chose == "none":
        raise RuntimeError(f"{z2m.name} never started; the fault above and its log say why")


def serving(z2m: Zigbee2mqtt, findings: list[str], what: str) -> bool:
    """Wait until that run has finished starting, before anything reads the broker.

    ⚠️ **Not optional bookkeeping, and it was a hole here.** `bridge/state` and `bridge/info` are
    published *retained*, so after the first run they are on the broker whether or not the run
    being asked about ever got anywhere — and a subscriber reading the last payload cannot tell
    which run wrote it. Reading them only once the process itself says it is up is what makes
    "it came back" a claim that can fail.
    """
    try:
        wait_for(lambda: z2m.up() or z2m.died(), f"{z2m.name} to finish starting", timeout=180.0)
    except TimeoutError:
        findings.append(f"FAULT: {what}: {z2m.name} never finished starting")
        return False
    if z2m.died():
        findings.append(f"FAULT: {what}: {z2m.name} exited while starting "
                        f"(exit {z2m.proc.returncode})")
        return False
    return True


def resumed_or_fault(findings: list[str], name: str, chose: str, what: str) -> None:
    """The `coordinator` arm's whole assertion, in the one place every case makes it."""
    if chose == "resumed":
        findings.append(f"{what}: herdsman **resumed** the network the coordinator was already "
                        f"carrying")
    else:
        findings.append(
            f"FAULT: {what}: herdsman chose `{chose}`, not `resumed` — the config carries the "
            f"radio's own network, so anything else means the two disagreed and Z2M went on to "
            f"form or restore one. `none` means it never got that far, and {name}'s log says why")


def bridge_says(watcher: Process, topic: str) -> str | None:
    """The last payload seen on one bridge topic.

    Read through `mosquitto_sub` rather than a Python MQTT client, which is the reason this
    harness adds one tool to the shell instead of two. Both topics are published **retained**, so
    a subscriber that starts after Z2M still sees them.
    """
    found = None
    for line in watcher.output().splitlines():
        if line.startswith(f"{topic} "):
            found = line[len(topic) + 1:]
    return found


def refused(endpoint: tuple[str, int]) -> bool:
    with socket.socket() as s:
        s.settimeout(0.2)
        return s.connect_ex(endpoint) != 0


def survive(z2m: Zigbee2mqtt, data: pathlib.Path, workdir: pathlib.Path, watcher: Process,
            tether, coordinator: Coordinator | None, findings: list[str],
            runs: list[Zigbee2mqtt]) -> None:
    """The two events a client of this thing actually meets, on a Z2M that is up and attached.

    Both end the same way — a Z2M is started again and must find the same network — because
    that is the only claim either of them is really about. What differs is what tether had to
    survive in between, and its own log is read for that.
    """
    # ─── (4) the client is killed ────────────────────────────────────────────────────────
    # ⚠️ Not *mid-frame*, and the difference is worth being honest about: a Z2M that has
    # finished starting is mostly idle, so this is a live attached client dying without
    # warning rather than one killed part-way through a multi-frame sequence. The mid-frame
    # case has a real client behind it in `client_kill_scenario.py`, which holds a herdsman
    # client in back-to-back backups precisely so the signal lands between frames. What this
    # adds is that the successor is a *whole zigbee2mqtt*, MQTT and converters and all.
    print("\n── SIGKILL, and a fresh zigbee2mqtt arrives ──", flush=True)
    z2m.kill()
    findings.append("the attached zigbee2mqtt was killed outright, with no chance to close "
                    "its socket or tell the radio anything")
    # The broker publishes Z2M's last will the moment that socket resets, so `bridge/state` goes
    # `offline` on its own here — which is what makes the `online` below this run's and not the
    # retained leftover of the one that was killed.
    wait_for(lambda: "offline" in (bridge_says(watcher, "zigbee2mqtt/bridge/state") or ""),
             "the broker to publish the killed run's last will", timeout=60.0)
    killed, chose = start_z2m(data, workdir, "z2m-after-kill", findings, runs)
    resumed_or_fault(findings, "z2m-after-kill", chose, "a fresh Z2M after a SIGKILL")
    got_off_the_ground(killed, chose)
    if serving(killed, findings, "a fresh Z2M after a SIGKILL"):
        wait_for(lambda: "online" in (bridge_says(watcher, "zigbee2mqtt/bridge/state") or ""),
                 "bridge/state to go online again", timeout=60.0)
        findings.append("bridge/state went offline on the kill and online again for the "
                        "successor: a whole zigbee2mqtt recovered, MQTT and converters and all")
    if tether is not None:
        # What the clients cannot see, read out of tether's own log. **Which of the two paths
        # ran is not this scenario's to choose** — an idle Z2M has nothing queued when it dies,
        # so the kernel sends an orderly FIN and the successor is simply the next one in; a
        # client with unread frames in its buffer gets an RST instead, and a client that is
        # merely wedged has to be displaced. `client_kill_scenario.py` stages all three
        # deliberately. Both outcomes are correct here, and both are reported rather than
        # asserted — what *is* asserted is the one thing that would be wrong either way.
        said = tether.output()
        opens = said.count("open at ")
        findings.append(
            "the device was opened once and never reopened across the kill: a client dying "
            "never reached the radio (INV 1)"
            if opens == 1 else
            f"FAULT: the device was opened {opens} times before the unplug — a client "
            f"lifecycle reached the radio, which is INV 1 broken")
        findings.append(
            "the successor displaced the dead client rather than replacing it (INV 4, kick-old)"
            if "takes over from" in said else
            "the killed client's connection had already ended when the successor arrived, so "
            "the successor was simply the next one in — an idle Z2M has nothing queued, so the "
            "kernel closes it rather than resetting it")

    # ─── (5) the dongle is pulled ────────────────────────────────────────────────────────
    if coordinator is None:
        findings.append(
            "the unplug case did not run: this run's coordinator is a real dongle and no "
            "--unplug/--replug was given, so nothing here can stage a plug event"
            if tether is not None else
            "the unplug case did not run: with --radio the stick is behind a tether this harness "
            "did not start, and nothing here can reach round to pull it")
        return
    print("\n── the dongle is pulled while zigbee2mqtt holds it ──", flush=True)
    coordinator.unplug()

    # INV 2 read backwards: while there is no device there is no listener. Asserted here as
    # well as in unplug_scenario.py because this is the first time the client on the other end
    # of it is a real Z2M, which reacts to the close by exiting.
    wait_for(lambda: refused(tether.endpoint), "the listener to go", timeout=15.0)
    findings.append("tether withdrew its listener while the device was absent (INV 2 backwards)")

    wait_for(lambda: killed.died(), "zigbee2mqtt to exit for its supervisor", timeout=60.0)
    code = killed.proc.returncode
    saw = DISCONNECTED in killed.output()
    findings.append(
        f"zigbee2mqtt saw the adapter go, said `{DISCONNECTED}` and exited {code} for a "
        f"supervisor to restart it — which is the contract, and is a different exit code from "
        f"the one a clean stop leaves"
        if saw and code == SUPERVISOR_RESTART_CODE else
        f"FAULT: zigbee2mqtt did not take its supervisor path after the unplug "
        f"(`{DISCONNECTED}` {'seen' if saw else 'absent'}, exit {code}, expected "
        f"{SUPERVISOR_RESTART_CODE})")
    killed.stop()

    print("  plugging it back in — same stick, new node, same name", flush=True)
    coordinator.plug_in()
    wait_for(lambda: not refused(tether.endpoint), "tether to reopen the device and serve",
             timeout=60.0)
    findings.append("tether reopened the stable name on its own and served again")

    back, chose = start_z2m(data, workdir, "z2m-after-replug", findings, runs)
    resumed_or_fault(findings, "z2m-after-replug", chose, "the supervisor's restart after a "
                                                          "replug")
    got_off_the_ground(back, chose)
    if serving(back, findings, "the supervisor's restart after a replug"):
        wait_for(lambda: "online" in (bridge_says(watcher, "zigbee2mqtt/bridge/state") or ""),
                 "bridge/state to go online again", timeout=60.0)
        findings.append("bridge/state is online again, on the same network, after the stick had "
                        "been out and come back")


# ─── The migration, and the database that has to survive it ──────────────────────────────

# Two devices, written into the database by hand, because the emulator has none to offer: it is
# the coordinator and only the coordinator — every ZDO descriptor handler returns early unless
# the address is 0x0000, there is no `Mgmt_Lqi` handler, and the ZDO dispatcher is bound to
# endpoint 0 so ZCL reaches nothing. Scripting the frames a device would have sent was
# considered and rejected: a device whose every answer we wrote proves our reply table, not the
# migration.
#
# So these are *rows*, and rows are honestly all this arm claims. What is under test is that a
# populated database survives a change of `serial.port`, and a row is what a populated database
# is made of. That a device still *answers* after the move needs a real device, and is not
# staged here.
#
# ⚠️ **The key names are herdsman's, and one of them is a trap.** The manufacturer name is
# `manufName` here and `manufacturerName` everywhere in herdsman's own API — `toDatabaseRecord`
# is the authority (`dist/controller/model/device.js`), and it serialises exactly its own key set
# and silently drops everything else. A row seeded with the API spelling loses that field on the
# first rewrite, which the control run catches before tether is ever in the picture — and that
# is the reason the control run exists.
#
# ⚠️ **`modelId` and `manufName` are deliberately not a real device's.** With them absent
# Z2M re-interviews on startup and the row changes underneath the assertion; with a *supported*
# pair it builds entities and starts reading attributes from something that is not there. An
# unrecognised pair is the quiet middle: the row is complete, the device is `supported: false`,
# and nothing is asked of the air.
SEEDED_DEVICES = (
    # id, type, IEEE, network address, device id, input clusters
    (2, "Router", "0x00124b0001020304", 0x1234, 256, [0, 3, 6]),
    (3, "EndDevice", "0x00124b0005060708", 0x5678, 770, [0, 1, 1026]),
)

# Keys herdsman rewrites on its own schedule. Comparing them would make the assertion fail for
# the passage of time rather than for anything a migration did.
VOLATILE_KEYS = frozenset({"lastSeen", "checkinInterval"})


def seed_devices(data: pathlib.Path) -> list[str]:
    """Append the hand-crafted rows to a database Z2M has already written its coordinator into.

    Appended rather than written from scratch, and that is not laziness: the coordinator row
    carries the adapter's own IEEE and its fifteen registered endpoints, so a hand-written one
    would be a second, worse copy of something Z2M produces correctly. Let it write that, then
    add what it cannot.
    """
    db = data / "database.db"
    existing = db.read_text()
    if existing and not existing.endswith("\n"):
        existing += "\n"
    rows = []
    for ident, kind, ieee, nwk, dev_id, clusters in SEEDED_DEVICES:
        rows.append(json.dumps({
            "id": ident,
            "type": kind,
            "ieeeAddr": ieee,
            "nwkAddr": nwk,
            "manufId": 4476,
            "manufName": "briard-tether",
            "modelId": "tether-test-device",
            "powerSource": "Mains (single phase)" if kind == "Router" else "Battery",
            "epList": [1],
            "endpoints": {"1": {"profId": 260, "epId": 1, "devId": dev_id,
                                "inClusterList": clusters, "outClusterList": [],
                                "clusters": {}, "binds": [], "configuredReportings": [],
                                "meta": {}}},
            "interviewCompleted": True,
            "interviewState": "SUCCESSFUL",
            "meta": {},
        }, separators=(",", ":")))
    db.write_text(existing + "\n".join(rows) + "\n")
    return [row[2] for row in SEEDED_DEVICES]


def devices_in(data: pathlib.Path) -> dict[str, dict]:
    """Every non-coordinator row in `database.db`, by IEEE address.

    The file is newline-delimited JSON, one entity per line, which is herdsman's own format and
    is why this needs no client to read it.
    """
    rows: dict[str, dict] = {}
    for line in (data / "database.db").read_text().splitlines():
        line = line.strip()
        if not line:
            continue
        row = json.loads(line)
        if row.get("type") != "Coordinator":
            rows[row["ieeeAddr"]] = {k: v for k, v in row.items() if k not in VOLATILE_KEYS}
    return rows


def database_intact(before: dict, data: pathlib.Path, findings: list[str], what: str) -> bool:
    """The assertion the whole tier exists for, made the same way every time it is made."""
    after = devices_in(data)
    if after == before:
        findings.append(f"{what}: the database still carries both devices, unchanged — "
                        f"{', '.join(sorted(before))}")
        return True
    gone = sorted(set(before) - set(after))
    arrived = sorted(set(after) - set(before))
    changed = sorted(k for k in set(before) & set(after) if before[k] != after[k])
    # Which *keys* moved, not just which rows — a row that came back with an extra field Z2M
    # fills in is a different finding from a row that lost its endpoints, and an assertion that
    # cannot tell them apart sends the reader to the wrong place.
    detail = []
    for ieee in changed:
        keys = sorted(k for k in set(before[ieee]) | set(after[ieee])
                      if before[ieee].get(k) != after[ieee].get(k))
        detail.append(f"{ieee} ({', '.join(f'{k}: {before[ieee].get(k)!r} → {after[ieee].get(k)!r}' for k in keys)})")
    findings.append(
        f"FAULT: {what}: the device database did not survive"
        + (f" — lost {', '.join(gone)}" if gone else "")
        + (f" — gained {', '.join(arrived)}" if arrived else "")
        + (f" — rewrote {'; '.join(detail)}" if detail else ""))
    return False


def config_keys(text: str) -> dict[str, str]:
    """Every `key: value` in a generated config, flattened, by key.

    Flat is enough and is not a shortcut: `z2m_config` writes one level of nesting with unique
    leaf names, so a key is unambiguous without tracking its parent. Anything richer would be
    parsing YAML to check a file this module wrote three lines above.
    """
    keys = {}
    for line in text.splitlines():
        if ":" in line and not line.strip().startswith("#"):
            name, _, value = line.partition(":")
            keys[name.strip()] = value.strip()
    return keys


def what_changed(before: str, after: str) -> tuple[set[str], list[str]]:
    """Which config keys the migration touched, and a line per touch for the log.

    ⚠️ **Keys, not diff lines.** A migration is `port` and nothing else, in both arms — the
    `mdns://` one included, since a user changes the line they must and leaves `adapter` where it
    is. Lines cannot say that as cleanly: a changed value is a removal and an addition, and a
    key that stayed put is neither, so the count answers a different question from the one being
    asked.

    A migration whose config differs in more than `port` is not the migration this arm claims
    to stage, and a harness that writes both halves is exactly the thing that could get it wrong
    without noticing: regenerate the second config instead of editing the first, and the
    `mdns://` half quietly drops `adapter` and changes two keys.
    """
    was, now = config_keys(before), config_keys(after)
    touched = {k for k in set(was) | set(now) if was.get(k) != now.get(k)}
    rendered = []
    for key in sorted(touched):
        if key not in now:
            rendered.append(f"{key}: {was[key]} → (removed)")
        elif key not in was:
            rendered.append(f"{key}: (added) → {now[key]}")
        else:
            rendered.append(f"{key}: {was[key]} → {now[key]}")
    return touched, rendered

def broker_and_watcher(workdir: pathlib.Path, port: int, processes: list) -> Process:
    """mosquitto, and a subscriber on every bridge topic. Before anything that wants one.

    Factored out when the migration arm became a second caller. Both arms need the same
    three things in the same order and for the same reasons, and the middle one is a scar.

    ⚠️ **The broker is asked about twice, because mosquitto can open its listener and then die**
    — it does exactly that when it cannot drop privileges (see `broker_config`), and a harness
    that only watched for the listener reported that as Z2M failing to reach MQTT two steps
    later. A broker that is gone is worth saying out loud, here, where it happened.
    """
    print("── mosquitto ──", flush=True)
    conf_path = workdir / "mosquitto.conf"
    conf_path.write_text(broker_config(port))
    broker = Process("mosquitto", ["mosquitto", "-c", str(conf_path)], workdir / "mosquitto.log")
    processes.append(broker)
    wait_for(lambda: not refused(("127.0.0.1", port)) or broker.died(),
             "mosquitto to open its listener")
    if broker.died():
        raise RuntimeError(f"mosquitto exited on the way up: {broker.output()}")
    watcher = Process("mqtt", ["mosquitto_sub", "-h", "127.0.0.1", "-p", str(port),
                               "-t", "zigbee2mqtt/bridge/#", "-v"], workdir / "mqtt.log")
    processes.append(watcher)
    print(f"  serving on 127.0.0.1:{port}, and we are subscribed", flush=True)
    return watcher


def run(tether_binary: pathlib.Path, args: argparse.Namespace, workdir: pathlib.Path) -> int:
    findings: list[str] = []
    data = workdir / "z2m-data"
    data.mkdir()
    port = args.port or free_port()
    broker_port = free_port()
    link = workdir / "coordinator"
    resuming = args.network == "coordinator"

    processes: list = []
    runs: list[Zigbee2mqtt] = []
    tether = None
    coordinator = None
    try:
        watcher = broker_and_watcher(workdir, broker_port, processes)

        # ─── the coordinator, and tether in front of it ──────────────────────────────────
        if args.port is None:
            print("\n── the coordinator ──", flush=True)
            config = {"device": args.stick or str(link),
                      "listen": rig.listen(port),
                      # On only for the arm that browses for it. Nothing about this run belongs
                      # on anybody's LAN — and when it *is* on, the namespace this scenario put
                      # itself in is what keeps that true.
                      "advertise": args.discover}
            if args.stick:
                # No `radio` key, and that is the difference from the emulator rather than an
                # omission: a dongle carries USB descriptors, so the family table names it
                # rather than being told. Which also means that `--stick` reaches the ConBee
                # with no help. `--adapter deconz` is then what *Z2M* needs, with the caveat
                # SILENT_ABOUT_FORMING carries.
                print(f"  a real one, on {args.stick}", flush=True)
                if args.unplug:
                    coordinator = Coordinator(None, link, workdir, "herdsman",
                                              stick=args.stick, unplug=args.unplug,
                                              replug=args.replug)
            else:
                coordinator = Coordinator(args.device, link, workdir, "herdsman")
                print(f"  {args.device} is on {link}", flush=True)
                # A pty carries no USB identity, so the emulator's family has to be named.
                config["radio"] = "znp"

            tether = rig.Tether(tether_binary, config, workdir)
            tether.serving()
            print(f"  tether is serving it at {tether.endpoint}", flush=True)

        # ─── the config, and what it says about the network ──────────────────────────────
        print(f"\n── the configuration.yaml, carrying the {args.network} network ──", flush=True)
        advanced = advanced_block(args.network, args.port is not None,
                                  f"socket://127.0.0.1:{port}")
        serial_port = args.radio or (
            "mdns://zigbee-coordinator" if args.discover else f"tcp://127.0.0.1:{port}")
        (data / "configuration.yaml").write_text(
            z2m_config(broker_port, serial_port, args.adapter, advanced))
        print(f"  serial.port is {serial_port}", flush=True)

        # ─── the first run ───────────────────────────────────────────────────────────────
        print("\n── zigbee2mqtt, on a data dir that did not exist ──", flush=True)
        first, chose = start_z2m(data, workdir, "z2m-first", findings, runs)
        if resuming:
            resumed_or_fault(findings, "z2m-first", chose, "the first run")
        elif args.adapter not in SILENT_ABOUT_FORMING:
            findings.append(
                "the first run **commissioned** the coordinator through tether: herdsman "
                "chose `reset`, which is a new network formed across the pipe"
                if chose == "reset" else
                f"FAULT: the first run should have commissioned a blank coordinator, but herdsman "
                f"chose `{chose}` — `resumed` means the radio was not blank and the run measured "
                f"nothing; `none` means it never got that far, and the log below says why")
        else:
            findings.append(
                f"`{args.adapter}` re-points the radio to match the config and reports `{chose}` "
                f"either way, so the start result proves nothing here; what this run does "
                f"measure is a real zigbee2mqtt driving a `{args.adapter}` radio through tether"
                if chose in ("resumed", "restored") else
                f"FAULT: the first run never started on `{args.adapter}` (chose `{chose}`)")
        got_off_the_ground(first, chose)

        serving(first, findings, "the first run")
        wait_for(lambda: bridge_says(watcher, "zigbee2mqtt/bridge/state") is not None
                 or first.died(), "bridge/state to be published", timeout=120.0)
        state = bridge_says(watcher, "zigbee2mqtt/bridge/state")
        findings.append(
            f"bridge/state says {state}"
            if state and "online" in state else
            f"FAULT: bridge/state never went online (last: {state})")
        # ⚠️ `bridge/info` lands a moment after `bridge/state`, so waiting on state alone and
        # then reading info is a race the harness loses about half the time.
        wait_for(lambda: bridge_says(watcher, "zigbee2mqtt/bridge/info") is not None
                 or first.died(), "bridge/info to be published", timeout=60.0)
        info = bridge_says(watcher, "zigbee2mqtt/bridge/info")
        findings.append(
            "bridge/info carries the coordinator Z2M is talking to"
            if info else "FAULT: bridge/info was never published")

        # ─── (3) the restart ─────────────────────────────────────────────────────────────
        print("\n── zigbee2mqtt again, same data dir ──", flush=True)
        code = first.stop()
        # ⚠️ **The exit code is not the evidence of a clean stop, and cannot be.** Z2M shuts down
        # properly and *then* node dies with `write after end` from inside winston: its file
        # transport is closed while the logger still has queued lines, so the last log line kills
        # the process with a non-zero code. That is somebody else's shutdown race (2.14.0,
        # measured), and reading it as a failure here would make this
        # harness red for a reason that has nothing to do with tether. So the log line is the
        # assertion and the code is an observation beside it.
        stopped = "Stopped Zigbee2MQTT" in first.output()
        findings.append(
            f"the first run stopped cleanly on SIGTERM, and said so (exit {code}, which is "
            f"winston's shutdown race rather than a failed stop)"
            if stopped else
            f"FAULT: the first run did not report a clean stop on SIGTERM (exit {code})")
        second, chose_again = start_z2m(data, workdir, "z2m-second", findings, runs)
        if resuming:
            resumed_or_fault(findings, "z2m-second", chose_again, "the restart")
        else:
            findings.append(
                "the second run **resumed** what the first one formed, which is what every start "
                "after day one does"
                if chose_again == "resumed" else
                f"FAULT: the second run chose `{chose_again}`, not `resumed` — the network the "
                f"first run formed did not survive, on the radio or in the data dir")
        got_off_the_ground(second, chose_again)

        up = serving(second, findings, "the restart")
        if up:
            findings.append(
                "bridge/info came back on the second run"
                if bridge_says(watcher, "zigbee2mqtt/bridge/info") else
                "FAULT: bridge/info was not republished on the second run")

        # ─── (4) and (5), which only mean anything where `resumed` is the bar ─────────────
        # And only on a Z2M that is actually up: both cases are about what happens *to* an
        # attached client, so staging them against one that never attached measures nothing.
        if resuming and up:
            survive(second, data, workdir, watcher, tether, coordinator, findings, runs)
    except (TimeoutError, RuntimeError, OSError) as exc:
        findings.append(f"FAULT: {exc or 'a step failed with no message'}")
    finally:
        for z2m in reversed(runs):
            z2m.stop()
        if tether is not None:
            tether.stop()
        if coordinator is not None:
            coordinator.stop()
        for proc in reversed(processes):
            proc.stop()

    said = tether.output() if tether is not None else "(no tether: a real stick was given)"
    return report(findings, said, runs)


def run_migration(tether_binary: pathlib.Path, args: argparse.Namespace,
                  workdir: pathlib.Path) -> int:
    """The migration, Z2M half: an install that predates tether, then one config line changes.

    Every step of it is there to make one assertion able to fail:

      1. the coordinator on a pty, with **nothing in front of it** — a Z2M that has never heard
         of tether, which is the state every real migration starts from;
      2. Z2M, pointed straight at that pty, resuming the network the coordinator carries;
      3. two devices written into its database by hand (`seed_devices` says why by hand);
      4. Z2M again, **still direct** — the control. Without it a lost row could as easily be Z2M
         forgetting on any restart as Z2M forgetting on *this* one, and the run could not tell
         which it had measured;
      5. tether, started in front of the same coordinator;
      6. Z2M again, with `serial.port` changed and nothing else changed.

    Steps 4 and 6 are the comparison. They differ by one line of configuration, which the run
    prints rather than asserts in the abstract — `one_line_apart` is what stops this harness
    from quietly changing two things and calling the result a migration.

    ⚠️ **The pty takes one holder at a time**, so the order above is not arrangeable differently:
    the network read, each Z2M, and tether all want the same device, and the only reason this
    works is that none of them overlap. A step that fails to stop leaves the next one timing out
    on a busy port, which reads as the coordinator being unreachable.
    """
    findings: list[str] = []
    data = workdir / "z2m-data"
    data.mkdir()
    port = free_port()
    broker_port = free_port()
    link = workdir / "coordinator"

    processes: list = []
    runs: list[Zigbee2mqtt] = []
    tether = None
    coordinator = None
    try:
        watcher = broker_and_watcher(workdir, broker_port, processes)

        # ─── the coordinator, with nothing in front of it ────────────────────────────────
        print("\n── the coordinator, and no tether anywhere ──", flush=True)
        coordinator = Coordinator(args.device, link, workdir, "herdsman")
        print(f"  {args.device} is on {link}", flush=True)

        # ─── the install as it was before anybody had heard of tether ────────────────────
        print("\n── the configuration.yaml of an install on USB ──", flush=True)
        net = read_network(str(link))
        print(f"  the coordinator carries pan 0x{net['pan_id']:04X} on channel {net['channel']}",
              flush=True)
        before_config = z2m_config(broker_port, str(link), args.adapter, network_block(net))
        (data / "configuration.yaml").write_text(before_config)
        print(f"  serial.port is {link}, which is a serial port and not an address", flush=True)

        # ─── (1) it works on USB, which is the premise and not yet the test ──────────────
        print("\n── zigbee2mqtt, straight at the coordinator ──", flush=True)
        first, chose = start_z2m(data, workdir, "z2m-on-usb", findings, runs)
        resumed_or_fault(findings, "z2m-on-usb", chose, "the install before tether")
        got_off_the_ground(first, chose)
        serving(first, findings, "the install before tether")
        first.stop()

        # ─── (2) a populated install, which the emulator cannot populate for us ──────────
        print("\n── two devices, into the database by hand ──", flush=True)
        seeded = seed_devices(data)
        before = devices_in(data)
        print(f"  {', '.join(seeded)}", flush=True)

        # ─── (3) the control: the same install, restarted, still on USB ──────────────────
        print("\n── zigbee2mqtt again, still on USB — the control ──", flush=True)
        control, chose = start_z2m(data, workdir, "z2m-control", findings, runs)
        resumed_or_fault(findings, "z2m-control", chose, "the restart before tether")
        got_off_the_ground(control, chose)
        serving(control, findings, "the restart before tether")
        control.stop()
        if not database_intact(before, data, findings, "the restart before tether"):
            # Everything after this measures a database that was already gone, and the migration
            # would be blamed for it. Said here rather than left to look like tether's doing.
            raise RuntimeError("the seeded devices did not survive a plain restart on USB, so "
                               "nothing after this could have told you anything about tether")

        # ─── (4) tether, in front of the very same coordinator ───────────────────────────
        print("\n── tether, in front of the same coordinator ──", flush=True)
        # A pty carries no USB identity, so the emulator's family has to be named — which is
        # also the knob case (c) turns. `--wrong-radio` makes tether advertise `ezsp` in front of
        # a Z-Stack coordinator, which is what a family-table mistake looks like from the client's
        # side, and the question is whether herdsman notices before it writes anything.
        radio = WRONG_RADIO if args.wrong_radio else "znp"
        config = {"device": str(link), "listen": rig.listen(port), "advertise": args.discover,
                  "radio": radio}
        tether = rig.Tether(tether_binary, config, workdir)
        tether.serving()
        print(f"  serving it at {tether.endpoint}, claiming `{radio}`", flush=True)

        # ─── (5) the one line that changes ───────────────────────────────────────────────
        print("\n── the configuration.yaml, migrated ──", flush=True)
        serial_port = "mdns://zigbee-coordinator" if args.discover else f"tcp://127.0.0.1:{port}"
        # ⚠️ **Edited, not regenerated, and that is the case rather than the style.** An earlier
        # version built the second config with `z2m_config` like the first, which drops `adapter`
        # for an `mdns://` port — so the `mdns://` arm changed two keys and measured a config no
        # migrating user produces. A user edits one line of the file they have and leaves the
        # rest, `adapter` included. Editing is what that is.
        after_config = before_config.replace(f"port: {link}", f"port: {serial_port}")
        (data / "configuration.yaml").write_text(after_config)
        touched, rendered = what_changed(before_config, after_config)
        for line in rendered:
            print(f"  {line}", flush=True)
        findings.append(
            f"the migration is a change to `{'`, `'.join(sorted(touched))}` and nothing else: "
            f"{'; '.join(rendered)}"
            if touched == {"port"} else
            f"FAULT: the migration changed `{'`, `'.join(sorted(touched))}`, and a migration is "
            f"`port` — whatever the run measures after this, it is not one line of a config")
        stale = config_keys(after_config).get("adapter")
        if args.discover and stale:
            print(f"  (and `adapter: {stale}` stays in the file, where mDNS does not read it)",
                  flush=True)

        # ─── (6) and the same install comes up through it ────────────────────────────────
        print("\n── zigbee2mqtt, through tether ──", flush=True)
        migrated, chose = start_z2m(data, workdir, "z2m-migrated", findings, runs,
                                    expect_start=not args.wrong_radio)

        if args.wrong_radio:
            # Case (c). The failure *is* the pass, and the database surviving it is the claim:
            # herdsman's `readItem` returns null only when the device answers with zero length, so
            # a wrong driver's first reads time out and `adapter.start()` rejects before
            # `beginCommissioning` or `database.clear()` can run. Everything below
            # asks whether that reading holds with a real client and a real populated database.
            wait_for(lambda: migrated.died(), "the wrongly-typed zigbee2mqtt to give up",
                     timeout=300.0)
            said = next((line.split("Error: ", 1)[1] for line in migrated.output().splitlines()
                         if "Error: " in line), "(it said nothing this harness could quote)")
            findings.append(
                f"a wrong `radio_type` stopped zigbee2mqtt at startup, before it could reach a "
                f"database it cannot read: {said.strip()}"
                if chose in (None, "none") else
                f"FAULT: tether advertised `{WRONG_RADIO}` in front of a Z-Stack coordinator and "
                f"herdsman started anyway (`{chose}`) — a family-table mistake that a client does "
                f"not notice is one that reaches the radio")
            database_intact(before, data, findings, "the wrongly-typed start")
            return report(findings, tether.output(), runs)

        resumed_or_fault(findings, "z2m-migrated", chose, "the migrated install")
        got_off_the_ground(migrated, chose)
        if serving(migrated, findings, "the migrated install"):
            database_intact(before, data, findings, "the migrated install")
            if args.discover and stale:
                # ⚠️ What this can and cannot say. It cannot say the TXT record *overrode* the
                # stale key: both name this radio, so a herdsman that read either would reach
                # the same adapter, and the run could not tell which it had read. Proving the
                # override needs a stale value that is a different family, which is one word of
                # this line away and is not what a migrating user has. What it does say is the
                # thing a migrating user needs true — that leaving the key behind is harmless,
                # where a herdsman that refused the pair would have failed the start outright.
                findings.append(
                    f"`adapter: {stale}` was left in the config and `mdns://` started anyway, so "
                    f"a migration really is the one line: the leftover key is dead rather than "
                    f"rejected (calling it *overridden* would need a stale value of a different "
                    f"family to show, which is not what a real config carries)")
            listed = bridge_says(watcher, "zigbee2mqtt/bridge/devices") or ""
            missing = [ieee for ieee in seeded if ieee not in listed]
            findings.append(
                "bridge/devices lists both devices after the migration, so they are the running "
                "Z2M's and not only the file's"
                if not missing else
                f"FAULT: the migrated Z2M does not list {', '.join(missing)} on bridge/devices, "
                f"whatever the database says")

        if args.ip_change:
            # ─── (7) the address moves under an attached zigbee2mqtt ─────────────────────
            # The counterpart of ZHA's (d), and the question the README's advice rests on:
            # `mdns://` is a *service type*, not an address, so a coordinator that moves should
            # be found again by browsing. The reading says `findMdnsAdapter` builds a fresh
            # `Bonjour()` per call and `Adapter.create` calls it at every start, so there is no
            # cache to go stale — but nothing had joined that to the restart zigbee2mqtt performs
            # on itself when its adapter goes. This joins them.
            print("\n── the address changes under it ──", flush=True)
            gone = tether.endpoint
            tether.stop()
            wait_for(lambda: refused(gone), "the old listener to go", timeout=30.0)
            wait_for(lambda: migrated.died(), "zigbee2mqtt to exit for its supervisor",
                     timeout=120.0)
            code = migrated.proc.returncode
            saw = DISCONNECTED in migrated.output()
            findings.append(
                f"the address went and zigbee2mqtt said `{DISCONNECTED}` and exited {code} for a "
                f"supervisor to restart it — the same path an unplug takes, which is what makes "
                f"an address change recoverable without a human"
                if saw and code == SUPERVISOR_RESTART_CODE else
                f"FAULT: zigbee2mqtt did not hand an address change to its supervisor "
                f"(`{DISCONNECTED}` {'seen' if saw else 'absent'}, exit {code}, expected "
                f"{SUPERVISOR_RESTART_CODE})")

            moved_port = free_port()
            # ⚠️ A distinct name, or `rig.Tether` truncates the first tether's log: it opens
            # `<name>.log` for writing, and the evidence you want most is the old one's.
            tether = rig.Tether(tether_binary,
                                {"device": str(link), "listen": rig.listen(moved_port),
                                 "advertise": True, "radio": radio}, workdir,
                                name="tether-moved")
            tether.serving()
            print(f"  it is now at {tether.endpoint}, and {gone} answers nothing", flush=True)

            # The config is not touched. That is the whole point: `mdns://zigbee-coordinator` is
            # the same line it was, and everything that has to change is on the wire.
            print("\n── zigbee2mqtt again, with its configuration.yaml untouched ──", flush=True)
            moved, chose = start_z2m(data, workdir, "z2m-moved", findings, runs)
            resumed_or_fault(findings, "z2m-moved", chose, "after the address moved")
            got_off_the_ground(moved, chose)
            found = f"Coordinator Port: {moved_port}" in moved.output()
            findings.append(
                f"it browsed and found the coordinator at its new port ({moved_port}) with no "
                f"change to `serial.port` — `mdns://` re-resolves per start, so an address that "
                f"moves costs a restart and nothing else"
                if found else
                f"FAULT: zigbee2mqtt did not report the coordinator at {moved_port}; it either "
                f"cached the old address or found something else")
            if serving(moved, findings, "after the address moved"):
                database_intact(before, data, findings, "after the address moved")
    except (TimeoutError, RuntimeError, OSError) as exc:
        findings.append(f"FAULT: {exc or 'a step failed with no message'}")
    finally:
        for z2m in reversed(runs):
            z2m.stop()
        if tether is not None:
            tether.stop()
        if coordinator is not None:
            coordinator.stop()
        for proc in reversed(processes):
            proc.stop()

    said = tether.output() if tether is not None else "(the run never got as far as a tether)"
    return report(findings, said, runs)


def report(findings: list[str], tether_said: str, runs: list) -> int:
    print("\n--- what was measured ---", flush=True)
    for finding in findings:
        print(("  ✗ " if finding.startswith("FAULT") else "  · ") + finding)
    print("\n--- what tether said ---")
    print(tether_said)
    faults = [f for f in findings if f.startswith("FAULT")]
    if faults:
        for run_ in runs:
            print(f"\n--- what {run_.name} said ---", file=sys.stderr)
            print(run_.output(), file=sys.stderr)
        print(f"\n{len(faults)} fault(s)", file=sys.stderr)
        return 1
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--tether", type=pathlib.Path, default=pathlib.Path("./briard-tether"))
    parser.add_argument("--network", default="shipped", choices=("shipped", "coordinator"),
                        help="what the generated config says the network is: Z2M's shipped "
                             "defaults, so a blank coordinator gets commissioned (the "
                             "default), or the coordinator's own, read off it first, so "
                             "`resumed` is the only branch that passes")
    parser.add_argument("--device", default="ResetLaunchpadCC26X2R1",
                        help="the emulator fixture; the default is the blank one, which is what "
                             "--network shipped is about. --network coordinator wants a formed "
                             "one: FormedLaunchpadCC26X2R1")
    parser.add_argument("--stick",
                        help="a real coordinator for this harness to put a tether in front of, "
                             "instead of the emulator: a /dev/serial/by-id/… path. Needs the tty "
                             "group and no more, which is what lets it be combined "
                             "with --discover")
    parser.add_argument("--unplug",
                        help="with --stick, a shell command that removes the dongle — a USB "
                             "deauthorize, `device_del` on a hypervisor's monitor, a smart hub's "
                             "port off, or `read -p 'pull it, then Enter'`. It is the one thing "
                             "here that may want root; without it case (5) is "
                             "skipped and says so")
    parser.add_argument("--replug", help="with --unplug, the command that puts it back")
    parser.add_argument("--migrate", action="store_true",
                        help="stage an install that predates tether — Z2M straight "
                             "at the coordinator, a database with two devices hand-written into "
                             "it — then change `serial.port` and nothing else, and assert the "
                             "network resumed and the database survived")
    parser.add_argument("--ip-change", action="store_true",
                        help="the counterpart of ZHA's address-change case: move tether's "
                             "address out from under an attached zigbee2mqtt and measure what "
                             "it takes to recover")
    parser.add_argument("--wrong-radio", action="store_true",
                        help="the migration's case (c): have tether advertise the wrong radio "
                             "family and assert zigbee2mqtt refuses to start rather than writing "
                             "to a database it cannot read")
    parser.add_argument("--discover", action="store_true",
                        help="reach the coordinator over mdns:// instead of tcp://, which also "
                             "re-execs this into a private network namespace")
    parser.add_argument("--radio", help="a serial.port for Z2M that is not this harness's tether "
                                        "— for pointing it at a tether already serving a stick")
    parser.add_argument("--port", type=int, help="with --radio, the port that tether is on")
    parser.add_argument("--adapter", default="zstack",
                        help="what Z2M is told the radio is, which `tcp://` cannot discover for "
                             "itself; `deconz` for the ConBee. Ignored for `mdns://`, where our "
                             "TXT record says it instead")
    args = parser.parse_args()

    rig.local_only("the zigbee2mqtt scenario")
    if args.migrate and args.network != parser.get_default("network"):
        parser.error("--migrate decides the network for itself: the install it stages is on the "
                     "coordinator's own, because an install being migrated is by definition one "
                     "that already resumed. --network selects between the other two arms.")
    if args.ip_change and not (args.migrate and args.discover):
        parser.error("--ip-change needs --migrate --discover: an address can only be *browsed* "
                     "for, so the client has to be on `mdns://`, and the populated database is "
                     "what makes \"and it still has its devices\" worth asserting.")
    if args.ip_change and args.wrong_radio:
        parser.error("--ip-change and --wrong-radio are two different cases: one needs a "
                     "migration that succeeded and the other needs one that refused.")
    if args.wrong_radio and not (args.migrate and args.discover):
        parser.error("--wrong-radio needs --migrate --discover: the wrong family has to reach the "
                     "client, and the TXT record is the only thing that carries it. Over `tcp://` "
                     "the client is told the family by its own config and never asks us.")
    if args.migrate and args.device == parser.get_default("device"):
        parser.error("--migrate migrates an install that already has a network, so it wants the "
                     "formed fixture: --device FormedLaunchpadCC26X2R1. The default is the blank "
                     "one, which --network shipped is about. (Said here rather than three minutes "
                     "in, where the network read would have said it.)")
    if args.migrate and (args.stick or args.radio):
        parser.error("--migrate needs a coordinator it can reach with nothing in front of it and "
                     "then put a tether in front of, which is the emulator's pty. --stick is a "
                     "dongle this harness only ever serves through tether; --radio is a tether "
                     "somebody else started.")
    if args.migrate and args.adapter in SILENT_ABOUT_FORMING:
        parser.error(f"--migrate asserts `resumed` on both sides of the change, and "
                     f"`{args.adapter}` reports `resumed` whether or not it re-pointed the "
                     f"radio (see SILENT_ABOUT_FORMING). Both halves would pass vacuously.")
    if args.radio and not args.port:
        parser.error("--radio needs --port: the network read and the config both want to know "
                     "where that tether is listening")
    if args.discover and args.radio:
        parser.error("--discover and --radio are two answers to the same question: one browses "
                     "for the coordinator, the other is handed an address")
    if args.stick and args.radio:
        parser.error("--stick and --radio are two answers to the same question: one has this "
                     "harness start a tether on the dongle, the other points Z2M at a tether "
                     "somebody else started")
    if bool(args.unplug) != bool(args.replug):
        parser.error("--unplug and --replug come as a pair: a case that pulls the dongle and "
                     "never puts it back measures half of what it claims")
    if args.unplug and not args.stick:
        parser.error("--unplug/--replug are for --stick: the emulator's plug event is a pty "
                     "closing, which this harness stages itself")
    if args.stick and args.device != parser.get_default("device"):
        parser.error("--stick and --device are two answers to the same question: one is a real "
                     "coordinator, the other picks an emulated one")
    if args.network == "coordinator" and args.adapter in SILENT_ABOUT_FORMING:
        parser.error(f"--network coordinator asserts `resumed`, and `{args.adapter}` reports "
                     f"`resumed` whether or not it re-pointed the radio (see "
                     f"SILENT_ABOUT_FORMING). That arm would pass while measuring nothing.")
    if shutil.which("zigbee2mqtt") is None:
        print("no zigbee2mqtt on PATH — run this inside `nix develop`", file=sys.stderr)
        return 2
    if shutil.which("mosquitto") is None:
        print("no mosquitto on PATH — run this inside `nix develop`", file=sys.stderr)
        return 2

    if args.discover:
        # Before anything is started, because it re-execs this whole process.
        into_a_private_network()

    tether = rig.binary(args.tether)
    with tempfile.TemporaryDirectory(prefix="tether-z2m-") as tmp:
        arm = run_migration if args.migrate else run
        return arm(tether, args, pathlib.Path(tmp))


if __name__ == "__main__":
    sys.exit(main())
