# SPDX-License-Identifier: GPL-3.0-or-later
#
# ⚠️ GPL-3, and necessarily: this file imports zigpy-znp's own test suite, which is GPL-3, and a
# module that imports a GPL-3 library is plausibly a derivative work. The
# Apache-2.0 tree around it is unaffected — mere aggregation — and nothing here is linked into
# the briard-tether binary or ships in any release artifact. tether meets it the way it meets a
# real dongle: as a character device it opens by path.
"""A standalone fake Z-Stack coordinator on a serial line.

zigpy-znp's test suite carries a declarative Z-Stack emulator — a `ZNP` subclass speaking their
own MT framer in server mode, with `@reply_to` handlers for NV, AF, ZDO and BDB, over captured
NVRAM images of real sticks. It is exercised daily against the real driver, which is worth more
than anything we would write. What it has never done is speak to a *serial port*: their fixture
patches the client's transport factory and hands bytes between two objects in one process.

So this is the mirror of their seam. The client's transport is left entirely alone — it must be
a real connection, that being the whole point — and the *server's* protocol is given a pty
instead of an in-process pipe. tether then opens the slave by path and cannot tell it from a
dongle.

    python3 tests/fake_coordinator.py --link /tmp/fake-coordinator
    briard-tether run -config <pointing device at /tmp/fake-coordinator>
    python3 tests/client_gate.py 127.0.0.1:6638

The emulator's formed images sit on channel 15. A real Sonoff here was deliberately formed
on channel 20, so a test that mixes the two up says so instead of passing.

`--herdsman` adds the five replies that client needs and this one does not have; see
`HerdsmanReplies` below, which is the only behaviour in this file that is ours rather than
theirs, and is off unless asked for.
"""

from __future__ import annotations

import argparse
import asyncio
import os
import pathlib
import sys

import rig
from ptylink import PtyLink

# zigpy-znp's emulator lives in its test tree, which the PyPI package does not ship — the sdist
# carries no conftest.py and no tests/nvram at all. nixpkgs builds from the GitHub archive, so
# the dev shell points ZIGPY_ZNP_SRC at the unpacked source and the whole suite is importable
# from there, pinned by flake.lock.
_SRC = os.environ.get("ZIGPY_ZNP_SRC")
if not _SRC:
    sys.exit(
        "ZIGPY_ZNP_SRC is not set: run this inside `nix develop`, which points it at the\n"
        "unpacked zigpy-znp source. The emulator is in that source's test tree and in no\n"
        "released package."
    )
sys.path.insert(0, _SRC)
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import zigpy.config  # noqa: E402
import zigpy.zdo.types as zdo_t  # noqa: E402
from zigpy_znp.uart import ZnpMtProtocol  # noqa: E402
import zigpy_znp.commands as c  # noqa: E402
import zigpy_znp.config as conf  # noqa: E402
import zigpy_znp.types as t  # noqa: E402
from zigpy_znp.types.nvids import ExNvIds, OsalNvIds  # noqa: E402

from tests import conftest as znp_fixtures  # noqa: E402
from tests.conftest import reply_to  # noqa: E402


class CorrectedZDO(t.CommandsBase, subsystem=t.Subsystem.ZDO):
    """`ZDO.ExtNwkInfo` with the layout a real radio answers, because zigpy-znp's is wrong.

    ⚠️ **Measured, not chosen** (a Sonoff ZBDongle-P through tether). The
    firmware replies with 24 bytes:

        00 00 | 09 | 9e c4 | 00 00 | 76 17 d4 70 2d 8f 59 78 | 00×8 | 14
        short   dev  pan     parent  extended pan id           parent  channel

    which is exactly zigbee-herdsman's definition, and is **not** zigpy-znp's: theirs omits the
    device-state byte and makes the channel a four-byte `Channels` bitmask instead of a channel
    number, for 26 bytes. Nothing in zigpy-znp notices, because zigpy-znp never sends this
    command — it reads the network out of NVRAM instead. Replying with their schema would put
    two extra bytes on the wire and herdsman would read the fields shifted.

    Declaring a `CommandsBase` subclass here does not touch their tables: `COMMANDS_BY_ID` is
    built once at import from the module-level `ALL_COMMANDS` list, so this is a serialiser for
    our own use and nothing more. The request half stays theirs — it is empty in both.
    """

    ExtNwkInfo = t.CommandDef(
        t.CommandType.SREQ,
        0x50,
        req_schema=(),
        rsp_schema=(
            t.Param("Dst", t.NWK, "Short address of this device"),
            t.Param("DeviceState", t.DeviceState, "The byte zigpy-znp's schema is missing"),
            t.Param("PanId", t.PanId, "The PAN id"),
            t.Param("ParentNWK", t.NWK, "Short address of the parent"),
            t.Param("ExtendedPanId", t.ExtendedPanId, "64-bit extended PAN id"),
            t.Param("ParentIEEE", t.EUI64, "IEEE address of the parent"),
            t.Param("Channel", t.uint8_t, "Current channel, a number and not a bitmask"),
        ),
    )


class HerdsmanReplies:
    """The five MT commands zigbee-herdsman asks for that zigpy-znp's emulator does not answer.

    **This is the only behaviour in this file that is ours.** Everything else is zigpy-znp's
    emulator unmodified, which is the whole reason for borrowing it — so this mixin is opt-in
    (`--herdsman`), and the zigpy gate and `run_suite.py` never see it.

    **Why it is needed at all.** Their emulator answers the subset of Z-Stack MT that *their
    own driver* drives, and herdsman drives a different subset: it brings a Z-Stack 3
    coordinator up with `ZDO.StartupFromApp` where zigpy-znp uses BDB commissioning, and it
    reads the running network with `ZDO.ExtNwkInfo` where zigpy-znp reads NVRAM. Without these,
    herdsman's boot dies on an MT RPC error after a 40-second timeout.

    **Where the replies come from, and it is not us.** Each is copied from zigbee-herdsman's
    own `test/adapter/z-stack/adapter.test.ts` (v10.9.1, `ZnpRequestMockBuilder`, lines
    633-689), which is what their suite runs its Z-Stack adapter against — they `vi.mock` the
    whole `Znp` class away, so a hand-written reply table is upstream's standard here too. The
    values are then read off the NVRAM this emulator already holds, so nothing is invented and
    nothing can drift from the coordinator the client is talking to. Two of them were
    additionally checked byte-for-byte against a real Sonoff — see `CorrectedZDO`.

    ⚠️ The list grew by one while being written, and the one it grew by is worth knowing: their
    emulator *does* answer the active-endpoint question, but only down the pipe zigpy asks it
    on. Coverage of a protocol and coverage of a command are not the same thing.
    """

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._herdsman_groups = set()

    @reply_to(c.SYS.GetExtAddr.Req())
    def herdsman_get_ext_addr(self, request):
        """`.handle(Subsystem.SYS, "getExtAddr", () => ({payload: {extaddress: …}}))`

        Reached when the configured extended PAN id is the default `dd:dd:…`, which sends
        herdsman to the adapter's own IEEE address (`parseConfigNetworkOptions`). That is the
        state zigpy-znp's Z-Stack 1.2 image is captured in.
        """
        return c.SYS.GetExtAddr.Rsp(ExtAddr=self._ieee())

    @reply_to(c.ZDO.StartupFromApp.Req(partial=True))
    def startup_from_app(self, request):
        """`.handle(Subsystem.ZDO, "startupFromApp", () => ({}))`, plus the state change.

        The name shadows `BaseZStack1CC2531.startup_from_app` on purpose rather than by
        accident: the emulator registers handlers by walking `dir(self)`, so a second method
        under a new name would leave both replying to one request and put two responses on the
        wire. Shadowing it is safe because that class's other branch — commissioning a *blank*
        Z-Stack 1.2 coordinator — is only reached by a zigpy client, which never sees this
        mixin.

        `beginStartup` sends this and then waits up to 60 s for a `stateChangeInd` carrying
        state 9. Answering the SREQ without the AREQ is the one way to make this look like a
        hang rather than a failure.
        """
        if self.nib.nwkState != t.NwkState.NWK_ROUTER:
            # Not formed. herdsman would not have got here — a blank coordinator takes its
            # BDB commissioning path, which is theirs and untouched — so say so rather than
            # claim a network that does not exist.
            return c.ZDO.StartupFromApp.Rsp(State=c.zdo.StartupState.NotStarted)

        return [
            c.ZDO.StartupFromApp.Rsp(State=c.zdo.StartupState.RestoredNetworkState),
            self.update_device_state(t.DeviceState.StartedAsCoordinator),
        ]

    @reply_to(c.ZDO.ExtNwkInfo.Req())
    def herdsman_ext_nwk_info(self, request):
        """Their mock reads the NIB for this too, and so does this:

            .handle(Subsystem.ZDO, "extNwkInfo", (_, handler) => {
                const nib = Structs.nib(…NvItemsIds.NIB…);
                return {payload: {panid: nib.nwkPanId, extendedpanid: …, channel:
                        nib.nwkLogicalChannel}};
            })

        `getNetworkParameters()` *is* this command, so the pan, extended pan and channel a
        herdsman client reports come from here — which is what makes the gate's comparison
        across a reconnect a claim about the coordinator rather than about our bookkeeping.
        """
        nib = self.nib
        return CorrectedZDO.ExtNwkInfo.Rsp(
            Dst=nib.nwkDevAddress,
            DeviceState=self.device_state,
            PanId=nib.nwkPanId,
            ParentNWK=0x0000,
            ExtendedPanId=t.ExtendedPanId(nib.extendedPANID),
            ParentIEEE=t.EUI64([0x00] * 8),
            Channel=nib.nwkLogicalChannel,
        )

    @reply_to(c.ZDO.ActiveEpReq.Req(partial=True))
    def herdsman_active_ep_req(self, request):
        """The same question their emulator already answers, asked down a different pipe.

        Their `on_zdo_active_ep_req` is reachable only through `AF.DataRequestExt` with
        endpoint 0, which is how *zigpy* sends ZDO requests. herdsman sends the legacy MT ZDO
        command instead (`Znp.requestZdo`), so the handler exists and never fires. Hence this:
        the same list out of the same bookkeeping, in the envelope the other client uses.

        `registerEndpoints` asks this before registering anything, so without it herdsman
        registers no endpoints at all.
        """
        return [
            c.ZDO.ActiveEpReq.Rsp(Status=t.Status.SUCCESS),
            c.ZDO.ActiveEpRsp.Callback(
                Src=0x0000,
                Status=t.ZDOStatus.SUCCESS,
                NWK=0x0000,
                ActiveEndpoints=[ep.Endpoint for ep in self.active_endpoints],
            ),
        ]

    @reply_to(c.ZDO.SimpleDescReq.Req(partial=True))
    def herdsman_simple_desc_req(self, request):
        """The endpoint's own descriptor, for the same reason `ActiveEpReq` is here.

        ⚠️ **This one only fires on a first start**: `Controller.start` interrogates the
        coordinator — active endpoints, then a simple descriptor *per endpoint* — only when the
        database has no coordinator in it yet. A gate that resumes an existing install never
        asks. Without it herdsman waits
        6 s and dies with `SRSP - ZDO - simpleDescReq after 6000ms`.

        The descriptor is assembled from the `AF.Register` requests their emulator already
        records, so it says back exactly what the client registered — same source as
        `on_zdo_simple_desc_req`, different envelope.

        ⚠️ An unknown endpoint answers a refusing status rather than raising. Their handler
        calls `pytest.fail` there, which outside pytest is a confusing traceback from inside a
        reply; a status is what real Z-Stack sends and what the client is written to read.
        ⚠️ The request field is `Endpoint`, not `EndPoint` — their own `on_zdo_simple_desc_req`
        spells it the other way because it is reached through a different dispatch, and getting
        it wrong here raises *inside* the reply, so the client sees silence and blames a timeout
        rather than a typo.
        """
        for endpoint in self.active_endpoints:
            if endpoint.Endpoint == request.Endpoint:
                break
        else:
            # No callback here: the SRSP alone carries the refusal, and a descriptor for an
            # endpoint that does not exist has nothing to put in it.
            return c.ZDO.SimpleDescReq.Rsp(Status=t.Status.INVALID_PARAMETER)

        return [
            c.ZDO.SimpleDescReq.Rsp(Status=t.Status.SUCCESS),
            c.ZDO.SimpleDescRsp.Callback(
                Src=0x0000,
                Status=t.ZDOStatus.SUCCESS,
                NWK=0x0000,
                SimpleDescriptor=zdo_t.SizePrefixedSimpleDescriptor(
                    endpoint=endpoint.Endpoint,
                    profile=endpoint.ProfileId,
                    device_type=endpoint.DeviceId,
                    device_version=endpoint.DeviceVersion,
                    input_clusters=endpoint.InputClusters,
                    output_clusters=endpoint.OutputClusters,
                ),
            ),
        ]

    @reply_to(c.ZDO.ExtFindGroup.Req(partial=True))
    def herdsman_ext_find_group(self, request):
        """`.handle(Subsystem.ZDO, "extFindGroup", () => ({payload: {status: 0}}))`

        Laid out by hand because zigpy-znp's response schema for this is a single opaque
        `Bytes` field, so the bytes are ours to place: herdsman reads `status, groupid,
        namelen, groupname`. Measured against the Sonoff, which answers
        `00 84 0b 00` + fifteen zero bytes for a group it has.

        The group set is real state, and deliberately: `addToGroup` adds the green-power group
        on every start, so a coordinator that forgot it between two starts would make the
        gate's two runs differ for a reason that is the emulator's and not tether's.
        """
        found = (request.Endpoint, request.GroupId) in self._herdsman_groups
        status = 0x00 if found else 0x01
        return c.ZDO.ExtFindGroup.Rsp(
            Group=t.Bytes(bytes([status]) + request.GroupId.serialize() + bytes(16))
        )

    @reply_to(c.ZDO.ExtAddGroup.Req(partial=True))
    def herdsman_ext_add_group(self, request):
        """`.handle(Subsystem.ZDO, "extAddGroup", () => ({payload: {status: 0}}))`"""
        self._herdsman_groups.add((request.Endpoint, request.GroupId))
        return c.ZDO.ExtAddGroup.Rsp(Status=t.Status.SUCCESS)

    def _ieee(self):
        """The coordinator's IEEE address, read where their own `UTIL.GetDeviceInfo` reads it."""
        legacy = self._nvram[ExNvIds.LEGACY]
        return t.EUI64.deserialize(legacy[OsalNvIds.EXTADDR])[0]


def build_server(device_cls, path: str):
    """Construct the emulator and give it a pty to speak through."""
    config = conf.CONFIG_SCHEMA(
        {
            conf.CONF_DEVICE: {conf.CONF_DEVICE_PATH: path},
            zigpy.config.CONF_NWK_BACKUP_ENABLED: False,
        }
    )
    server = device_cls(config)
    server.port_path = path
    server._transports = []
    return server


def bind(server, wire) -> None:
    """Give the emulator a wire to speak through, and start reading what tether writes.

    Separate from construction because a coordinator outlives a *generation* of the device it
    is plugged into: staging a replug means binding the same server — the same NVRAM, the same
    network — to a new pty, which is exactly what a stick that was unplugged and put back is
    (see unplug_scenario.py).

    The wire is a `PtyLink` when tether runs here and a `rig.SerialWire` when it runs on a
    Windows machine. Nothing below knows which: both are a descriptor to read
    and a transport to answer into, which is all a coordinator ever needed.
    """
    server._uart = ZnpMtProtocol(server)
    server._uart.connection_made(wire.transport())

    loop = asyncio.get_running_loop()

    def readable() -> None:
        try:
            data = os.read(wire.fd, 4096)
        except OSError:
            # The wire is gone — the unplug, staged or real. Nothing useful to do but stop.
            loop.remove_reader(wire.fd)
            return
        if data:
            server._uart.data_received(data)

    loop.add_reader(wire.fd, readable)
    wire.loop = loop


async def serve(device_cls, wire) -> int:
    server = build_server(device_cls, wire.device)
    bind(server, wire)

    # One line, the same shape in both modes, and a caller waits for it rather than for a
    # filesystem artefact: run_gate.py has to know the coordinator is *attached* before it
    # starts tether, and on the serial route there is no symlink to watch for.
    print(f"{type(server).__name__} on {wire.device} — {wire}", flush=True)
    print("point tether's config `device` at that name; ctrl-c to stop", flush=True)

    try:
        await asyncio.get_running_loop().create_future()  # until cancelled
    finally:
        wire.close()
    return 0


def main() -> int:
    devices = {
        name: getattr(znp_fixtures, name)
        for name in dir(znp_fixtures)
        if name.startswith(("Formed", "Reset")) and isinstance(getattr(znp_fixtures, name), type)
    }

    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument(
        "--device",
        default="FormedLaunchpadCC26X2R1",
        choices=sorted(devices),
        help="which of zigpy-znp's emulated coordinators to be (default: a formed CC2652)",
    )
    parser.add_argument(
        "--link",
        type=pathlib.Path,
        help="a stable path to symlink at the pty, the way udev names a real adapter",
    )
    parser.add_argument(
        "--serial",
        type=int,
        metavar="PORT",
        help="speak to a serial port on another machine instead of making a pty: connect to "
             f"this TCP port on 127.0.0.1, where a hypervisor is bridging that machine's "
             f"{rig.DEVICE} (default port {rig.WIRE_PORT}; see rig.py and the Windows runbook)",
    )
    parser.add_argument(
        "--herdsman",
        action="store_true",
        help="also answer the five MT commands zigbee-herdsman needs and zigpy-znp's emulator "
             "does not implement (see HerdsmanReplies; off by default so the zigpy gate and "
             "their own suite meet their emulator unmodified)",
    )
    args = parser.parse_args()

    device_cls = devices[args.device]
    if args.herdsman:
        device_cls = type(f"Herdsman{args.device}", (HerdsmanReplies, device_cls), {})

    if args.serial is not None and args.link is not None:
        parser.error("--serial and --link are two different wires; pick one")
    wire = rig.SerialWire(args.serial) if args.serial is not None else PtyLink(args.link)

    try:
        return asyncio.run(serve(device_cls, wire))
    except KeyboardInterrupt:
        print("stopped", flush=True)
        return 0


if __name__ == "__main__":
    sys.exit(main())
