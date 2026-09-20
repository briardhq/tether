# SPDX-License-Identifier: GPL-3.0-or-later
#
# ⚠️ GPL-3 by association rather than by import: it runs the two files beside it that do import
# zigpy-znp, and keeping one licence across the directory is simpler to reason about than a
# boundary drawn through three files that are only ever used together. Nothing
# here is linked into the briard-tether binary or ships in any release artifact.
"""Run a client gate with one command, on a machine with no dongle.

Three processes have to line up for a gate to mean anything — an emulated coordinator on a
pty, a tether serving it, and a real client reaching that tether. Asking a person to arrange
those in three terminals with a hand-written config is asking for a test nobody runs, so this
arranges them, reports, and takes them down again.

    go build -o briard-tether ./cmd/briard-tether
    python3 tests/run_gate.py                     # zigpy, the client ZHA is built on
    python3 tests/run_gate.py --client herdsman   # zigbee2mqtt's stack, over mdns://

With `TETHER_WINDOWS_SSH` set, the tether in the middle runs on a Windows machine and the rest
of it stays here (see rig.py). The `mdns://` client then needs to share a segment with that
machine, which `TETHER_WINDOWS_BRIDGE` and root give it (see `OnTheGuestsSegment`).

Exit code is the gate's. On failure every process's output is printed, because the interesting
question is always *which* of the three broke.

Locally it needs no hardware and no root, which is what makes it the one tier-2 check CI could
hold.
"""

from __future__ import annotations

import argparse
import json
import os
import pathlib
import shlex
import socket
import subprocess
import sys
import tempfile
import time

import rig

HERE = pathlib.Path(__file__).resolve().parent

# Set once the runner has re-exec'd itself into a network namespace, so the second pass knows
# not to do it again.
# Defined in `rig` because `rig.listen` has to know about it too; see there.
NETNS_MARKER = rig.PRIVATE_NETWORK


def free_port() -> int:
    """A port nothing is using. Racy in principle and fine here: the window is microseconds and
    the alternative is a hardcoded number that collides with whatever else runs on a CI box."""
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def wait_for(predicate, what: str, timeout: float = 30.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.05)
    raise TimeoutError(f"timed out after {timeout:g}s waiting for {what}")


def into_a_private_network() -> None:
    """Re-exec the whole runner in a network namespace with nothing but loopback in it.

    Only a harness whose client browses over mDNS needs this — this gate's herdsman client and
    `z2m_scenario.py --discover` — and it needs it for two reasons that would each be enough.

    **Correctness.** herdsman's `mdns://` takes the *first* coordinator that answers, by
    service type, with no way to say which one it meant. On a LAN carrying a real
    SLZB-06 — or a second tether — the gate would be measuring whatever replied first, and it
    would pass or fail for reasons that have nothing to do with this checkout.

    **Courtesy.** The rest of this runner keeps the advert off the wire entirely, and a test
    has no business putting a coordinator on somebody's network for five seconds.

    ⚠️ And on this development machine it is the only way it works at all: multicast sent on
    the default-route interface is not looped back to other processes on the same host
    (measured — loopback and a second NIC both do, that one does not), and
    bonjour-service browses whichever interface the default route names. Nothing in
    herdsman's `findMdnsAdapter` takes an interface, so there is nothing to configure.

    A user namespace goes with it (`--user --map-root-user`) so this needs no root: an
    unprivileged user may create a network namespace inside one, and may then configure the
    interfaces it owns — including creating a veth pair, as long as both ends stay inside.

    ⚠️ **The veth pair is what keeps this from being a loopback-only namespace.** tether
    refuses to advertise on a loopback interface on purpose (`advertisable`), so in a namespace
    with nothing else in it there is no interface to register on and the responder falls through
    to the library's own filter, which does take `lo`. That announces once and never again: a
    browsing Home Assistant receives one packet in three minutes. With a real segment in here
    the responder announces the way it does anywhere, and `rig.listen` binds every interface so
    the address the advert carries is one a client can reach.

    Loopback still comes up, because plenty in these harnesses talks to itself over it.
    """
    if os.environ.get(NETNS_MARKER):
        return
    os.environ[NETNS_MARKER] = "1"
    # The *calling* script, not this one: `z2m_scenario.py --discover` needs this namespace for
    # the same two reasons, and re-execing run_gate.py would quietly run a different test.
    again = shlex.join([sys.executable, str(pathlib.Path(sys.argv[0]).resolve()), *sys.argv[1:]])
    os.execvp(
        "unshare",
        ["unshare", "--user", "--map-root-user", "--net", "--", "sh", "-c",
         "ip link set lo up"
         " && ip link add wire0 type veth peer name wire1"
         " && ip addr add 10.86.0.1/24 dev wire0 && ip link set wire0 up"
         " && ip addr add 10.86.0.2/24 dev wire1 && ip link set wire1 up"
         # The far end carries frames but must not be *advertisable*: `advertisable` keeps any
         # interface that is Up and Multicast, so leaving the flag on would announce two
         # addresses for one tether and hand the client the same coin flip a multi-homed host
         # hands it on a real LAN.
         " && ip link set wire1 multicast off"
         # Without a route for the group there is nowhere to send to, and the failure is an
         # obscure "network is unreachable" from inside the responder rather than about mDNS.
         " && ip route add 224.0.0.0/4 dev wire0"
         f" && exec {again}"],
    )


class OnTheGuestsSegment:
    """A network namespace with one leg on the bridge the far machine is attached to, for the
    `mdns://` client when tether is over there.

    The local gate re-execs the whole runner into a namespace with nothing but loopback,
    because tether and the client have to share a segment and loopback is the one nobody else
    is on. Over the seam tether is on the far machine's LAN already, so the segment exists —
    what is missing is a way for the client to be on it *and on nothing else*: browsing from
    this machine's own namespace would answer with whatever coordinator the office LAN has
    (correctness), and bonjour-service browses the default-route interface, which is not the
    bridge (the measured reason the local gate needs a namespace at all). So: a veth pair, one
    end enslaved to the bridge, the other alone in a fresh namespace with an address on the
    guest's subnet and a route for the multicast group. Only the *client* runs in it — the
    emulator reaches the guest's COM port over a socket on this machine's loopback, and the
    tether is started over ssh from here — which is why this wraps one command rather than
    re-executing the runner.

    Root, unavoidably — but for the bridge rather than for the veth, which is the part
    worth saying. A user namespace carries CAP_NET_ADMIN over the network namespace it owns, so
    a veth pair with *both ends inside* needs nothing (`scripts/wire-tests.sh` does exactly that).
    What needs root is enslaving one end to a bridge the **host** owns, which is a change to the
    host and cannot be done from inside, so run this under `sudo -E`.
    """

    def __init__(self, bridge: str, address: str) -> None:
        self.bridge, self.address = bridge, address
        # Names are bounded at 15 bytes by the kernel; a pid keeps two runs apart.
        self.ns = f"tether-gate-{os.getpid()}"
        self.veth, self.peer = f"tg{os.getpid()}", f"tgb{os.getpid()}"

    def __enter__(self) -> list[str]:
        if os.geteuid() != 0:
            raise SystemExit(
                f"the mdns:// gate over the seam needs a namespace with a leg on {self.bridge},\n"
                "  which means creating a veth pair and enslaving it — root's work: run this\n"
                "  under `sudo -E`."
            )
        self._ip("netns", "del", self.ns, check=False)
        self._ip("netns", "add", self.ns)
        try:
            self._ip("link", "add", self.veth, "type", "veth", "peer", "name", self.peer)
            self._ip("link", "set", self.peer, "master", self.bridge, "up")
            self._ip("link", "set", self.veth, "netns", self.ns)
            self._ip("-n", self.ns, "link", "set", "lo", "up")
            self._ip("-n", self.ns, "addr", "add", self.address, "dev", self.veth)
            self._ip("-n", self.ns, "link", "set", self.veth, "up")
            # As on loopback locally: without a route for the group nothing can send to it,
            # and bonjour-service sends to the group on whichever interface the route names.
            self._ip("-n", self.ns, "route", "add", "224.0.0.0/4", "dev", self.veth)
        except BaseException:
            self.__exit__(None, None, None)
            raise
        return ["ip", "netns", "exec", self.ns]

    def __exit__(self, *_) -> None:
        # Deleting the namespace takes its veth with it, and the veth takes its peer.
        self._ip("netns", "del", self.ns, check=False)
        self._ip("link", "del", self.peer, check=False)

    @staticmethod
    def _ip(*args: str, check: bool = True) -> None:
        done = subprocess.run(["ip", *args], capture_output=True, text=True)
        if check and done.returncode != 0:
            raise SystemExit(f"ip {' '.join(args)}: {done.stderr.strip()}")


class Process:
    """A child process whose output is kept, so a failure can say what it said."""

    def __init__(self, name: str, argv: list[str], log: pathlib.Path) -> None:
        self.name = name
        self.log_path = log
        self.handle = log.open("wb")
        self.proc = subprocess.Popen(argv, stdout=self.handle, stderr=subprocess.STDOUT)

    def died(self) -> bool:
        return self.proc.poll() is not None

    def stop(self) -> None:
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=10)
        self.handle.close()

    def output(self) -> str:
        # Readable after stop() as well: the failure dump runs last, when the handle is closed.
        if not self.handle.closed:
            self.handle.flush()
        return self.log_path.read_text(errors="replace").strip()


def client_argv(client: str, port: int) -> list[str]:
    """How each client is told where the coordinator is — which is the difference between them.

    zigpy is handed the address, because ZHA is: Home Assistant does the zeroconf discovery and
    hands `socket://host:port` down to the library, so a zigpy harness that browsed for itself
    would be testing code no ZHA user runs. herdsman is handed the *service type*, because
    zigbee2mqtt is: `port: mdns://zigbee-coordinator` is a real line in a real Z2M config, and
    the browsing happens inside the client.
    """
    if client == "zigpy":
        return [sys.executable, str(HERE / "client_gate.py"), f"{rig.ADDR}:{port}"]
    # Written out rather than imported from internal/discovery, deliberately and for the same
    # reason the wire test writes it out: `_zigbee-coordinator._tcp.local.` is the string in
    # somebody else's manifest, and a test borrowing our constant would follow us through
    # exactly the change that would strand every client.
    return ["node", str(HERE / "herdsman_gate.js"), "mdns://zigbee-coordinator", str(port)]


def coordinator_argv(device: str, client: str, link: pathlib.Path) -> list[str]:
    """The emulator, plus the five replies the herdsman client needs and it does not have.

    `--herdsman` is off for every other caller on purpose: the zigpy gate and `run_suite.py`
    must go on meeting zigpy-znp's emulator exactly as shipped, which is most of what makes
    borrowing their suite worth anything. See `HerdsmanReplies` in fake_coordinator.py.

    The wire is the one difference between the two machines this gate can measure: a pty under
    a stable name here, and a socket the hypervisor bridges to the far machine's COM port when
    tether is over there (rig.py).
    """
    argv = [sys.executable, str(HERE / "fake_coordinator.py"), "--device", device]
    argv += ["--serial", str(rig.WIRE_PORT)] if rig.WINDOWS else ["--link", str(link)]
    if client == "herdsman":
        argv.append("--herdsman")
    return argv


class Coordinator:
    """What tether has on its serial end, and the two halves of pulling it out again.

    Two bodies behind one interface, which is `unplug_scenario.Stick`'s shape and for the same
    reason: the scenario is identical either way — a client holding the port, the device going,
    the device coming back under the same name — so only the staging differs. The emulator's
    plug event is a pty closing and reopening; a real one's is whatever `--unplug`/`--replug`
    name, because nothing in a test process can pull a dongle by itself.

    ⚠️ **What a replug proves on the emulator is narrower than on a real stick, and the
    difference is worth stating.** A restarted emulator loads the same captured NVRAM image, so
    "the network came back identical" is partly true by construction — what is genuinely under
    test there is that the client takes its own recovery path — Z2M exits for a supervisor,
    ZHA reloads the config entry — that tether reopens the stable name on its own, and that what
    follows resumes rather than re-forms. On a real stick the NVRAM continuity is the stick's
    own, and nothing here arranges it.

    **It lives here rather than in a scenario because a third harness needed it** — the three
    real call sites an abstraction has to earn here — and here is where `coordinator_argv` and
    `Process` already are. Which client's extra replies the emulator carries is the caller's to
    say: ZHA gets zigpy-znp's emulator exactly as shipped, and Z2M gets it plus the five
    herdsman asks for.

    ⚠️ **The staging command is the one thing in these harnesses that still wants root** — a USB
    deauthorize/reauthorize is a write to sysfs, which no group confers — so it is passed in
    rather than built here, as `sudo sh -c …` or a hypervisor's monitor or a hand.
    """

    def __init__(self, device: str | None, link: pathlib.Path, workdir: pathlib.Path,
                 client: str, stick: str | None = None, unplug: str | None = None,
                 replug: str | None = None) -> None:
        self.device, self.link, self.workdir, self.client = device, link, workdir, client
        self.stick, self.unplug_cmd, self.replug_cmd = stick, unplug, replug
        self.generation = 0
        self.proc: Process | None = None
        if stick is None:
            self.plug_in()

    def plug_in(self) -> None:
        if self.stick is not None:
            self._stage(self.replug_cmd)
            return
        self.generation += 1
        self.proc = Process("coordinator", coordinator_argv(self.device, self.client, self.link),
                            self.workdir / f"coordinator-{self.generation}.log")
        wait_for(lambda: "ctrl-c to stop" in self.proc.output() or self.proc.died(),
                 "the emulator to open its wire")

    def unplug(self) -> None:
        """A real one is whatever the staging command does. The emulator's takes both halves, as
        the Go test at tier 1 and `unplug_scenario.py` do: the pty goes and so does the name
        that pointed at it. Removing only one would stage something that cannot happen to a real
        adapter — a hot-removed stick takes both by itself."""
        if self.stick is not None:
            self._stage(self.unplug_cmd)
            return
        self.proc.stop()
        self.proc = None
        if self.link.is_symlink() or self.link.exists():
            self.link.unlink()

    @staticmethod
    def _stage(command: str) -> None:
        """Run one staging command. Deliberately not shared with `unplug_scenario.Stick`: what
        the two have in common is a `subprocess.run` and a shell, which is not logic worth a
        helper — the interface above is the thing that was worth copying."""
        subprocess.run(command, shell=True, check=True, stdout=subprocess.DEVNULL)

    def stop(self) -> None:
        if self.proc is not None:
            self.proc.stop()

    def output(self) -> str:
        return self.proc.output() if self.proc is not None else ""


def run(tether: pathlib.Path, device: str, client: str, workdir: pathlib.Path,
        client_prefix: list[str] = ()) -> int:
    port = free_port()
    link = workdir / "coordinator"

    coordinator = served = None
    failed = True
    try:
        coordinator = Process(
            "fake coordinator",
            coordinator_argv(device, client, link),
            workdir / "coordinator.log",
        )
        # Its own line rather than a filesystem artefact, because the serial route has no
        # symlink to watch for — and because it is also what catches an emulator that died
        # on the way up, which a symlink that never appears cannot tell you.
        wait_for(lambda: "ctrl-c to stop" in coordinator.output(),
                 "the emulator to open its wire")

        served = rig.Tether(
            tether,
            {
                "device": rig.DEVICE if rig.WINDOWS else str(link),
                # A pty carries no USB descriptors, so the family cannot be detected and has to
                # be named — the same path a Windows host or an unrecognised stick takes.
                "radio": "znp",
                "listen": rig.listen(port),
                # Off unless the gate is the one that browses for it. Nothing about this run
                # belongs on anybody's LAN — and when the advert *is* on, the namespace this
                # runner put itself in is what keeps that true.
                "advertise": client == "herdsman",
            },
            workdir,
        )
        served.serving()

        # Flushed, or it arrives after the child's output: the child writes straight to the
        # terminal and this does not.
        print(coordinator.output().splitlines()[0], flush=True)
        print(f"{rig.where()}, serving {served.endpoint[0]}:{port}\n", flush=True)
        gate = subprocess.run([*client_prefix, *client_argv(client, port)])
        failed = gate.returncode != 0
        return gate.returncode
    except TimeoutError as exc:
        print(f"\nsetup failed: {exc}", file=sys.stderr)
        return 1
    finally:
        for process in (served, coordinator):
            if process is not None:
                process.stop()
        # On failure, everything. The interesting question is always *which* of the three
        # broke, and that is unanswerable from the client's side alone: a client that cannot
        # reach a coordinator says the same thing whether tether corrupted a byte, the
        # emulator died, or nothing ever opened the port. A passing run prints none of it.
        if failed:
            for process in (coordinator, served):
                if process is None:
                    continue
                print(f"\n--- {process.name} ({process.log_path.name}) ---", file=sys.stderr)
                print(process.output() or "(said nothing)", file=sys.stderr)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument(
        "--tether",
        type=pathlib.Path,
        default=pathlib.Path("./briard-tether"),
        help="the tether binary to put in front of the emulator (default: ./briard-tether)",
    )
    parser.add_argument(
        "--device",
        default="FormedLaunchpadCC26X2R1",
        help="which emulated coordinator to run (see fake_coordinator.py --help)",
    )
    parser.add_argument(
        "--client",
        default="zigpy",
        choices=("zigpy", "herdsman"),
        help="which client stack drives the coordinator (default: zigpy, the one ZHA uses)",
    )
    args = parser.parse_args()

    segment = None
    if args.client == "herdsman":
        if not rig.WINDOWS:
            into_a_private_network()
        elif rig.BRIDGE:
            segment = OnTheGuestsSegment(rig.BRIDGE, rig.GATE_ADDR)
        else:
            print(
                "the herdsman gate browses for the coordinator over mDNS, so the client has to\n"
                "share a segment with the far machine. Name the bridge its NIC hangs off in\n"
                "TETHER_WINDOWS_BRIDGE and run this under sudo -E (see rig.py).",
                file=sys.stderr,
            )
            return 2

    tether = rig.binary(args.tether)

    with tempfile.TemporaryDirectory(prefix="tether-gate-") as tmp:
        if segment is None:
            return run(tether, args.device, args.client, pathlib.Path(tmp))
        with segment as prefix:
            return run(tether, args.device, args.client, pathlib.Path(tmp), prefix)


if __name__ == "__main__":
    sys.exit(main())
