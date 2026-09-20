# SPDX-License-Identifier: GPL-3.0-or-later
#
# ⚠️ GPL-3: this file is a conftest for zigpy-znp's own test suite and imports it.
# It is never imported by anything we ship and is linked into nothing.
"""Re-point zigpy-znp's application suite at a real transport.

This file is not run from where it sits. `run_suite.py` copies zigpy-znp's test tree to a
temporary directory and drops this in as `tests/application/conftest.py`, where pytest's
ordinary rule — a nearer conftest overrides a farther one — replaces their `make_znp_server`
with the one below. Their own conftest is never edited.

Their fixture patches `zigpy_znp.uart.create_serial_connection` with an in-process pair of
forwarding transports: the client's writes become a direct call to the server protocol's
`data_received`. Nothing is ever serialised through anything. This replaces that one function
so the same emulator instead sits on a pty, with a real tether serving it and the real client
connecting over TCP — the same three pieces `run_gate.py` arranges, once per test.

Everything else is left exactly as they wrote it. Both ends still live in this process, so
every assertion their tests make about client *and* server internals still holds; the only
thing that changed is that the bytes are now real.
"""

from __future__ import annotations

import asyncio
import json
import os
import pathlib
import socket
import subprocess
import sys
import tempfile

import pytest
from zigpy.serial import create_serial_connection as real_create_serial_connection
from zigpy_znp.uart import ZnpMtProtocol

from .. import conftest as theirs

import rig

# The tether binary under test. run_suite.py resolves it and passes it down; there is no
# default, because silently testing some other tether would be worse than not running.
TETHER = pathlib.Path(os.environ["TETHER_BINARY"])


class TetherRig:
    """One emulator on a wire, with one tether in front of it, for the length of one test.

    The wire and the tether both come from `rig`, so the same 155 tests run against a tether
    here and against one on a Windows machine — the only difference being a serial port
    bridged from there instead of a pty here.
    """

    def __init__(self, server, workdir: pathlib.Path) -> None:
        self.server = server
        self.workdir = workdir
        workdir.mkdir(parents=True, exist_ok=True)
        self.wire = rig.wire()

        server._uart = ZnpMtProtocol(server)
        server._uart.connection_made(self.wire.transport())

        self.loop = asyncio.get_running_loop()
        self.loop.add_reader(self.wire.fd, self._readable)
        self.wire.loop = self.loop

        with socket.socket() as s:
            s.bind(("127.0.0.1", 0))
            self.port = s.getsockname()[1]

        self.tether = rig.Tether(
            TETHER,
            {
                "device": self.wire.device,
                "radio": "znp",  # a pty carries no USB descriptors, and neither does a COM port
                "listen": rig.listen(self.port),
                "advertise": False,
            },
            workdir,
        )
        # A rig that cannot come up must take its own tether down with it. Without this the
        # fixture never records it — construction raised — so nothing closes it, and over the
        # seam one failure cascades: the leaked tether keeps the coordinator's port open and
        # every test after it meets "Serial port busy" (measured: eight of them).
        try:
            self.tether.serving(timeout=20.0)
        except BaseException:
            self.close()
            raise

    def _readable(self) -> None:
        try:
            data = os.read(self.wire.fd, 4096)
        except OSError:
            self.loop.remove_reader(self.wire.fd)
            return
        if data:
            self.server._uart.data_received(data)

    async def connect_client(self, loop, protocol_factory, url, **kwargs):
        """What `zigpy_znp.uart.create_serial_connection` becomes for the client.

        The URL it was going to open is discarded — that is the whole point, the client is
        reaching a coordinator over the network now — and every other argument is handed
        untouched to the real zigpy serial layer, whose `socket://` platform is a raw TCP
        stream. Baud rate and flow control travel with it and are ignored there, which is
        exactly what happens to a real ZHA pointed at a networked coordinator.
        """
        host, port = self.tether.endpoint
        return await real_create_serial_connection(
            loop=loop,
            protocol_factory=protocol_factory,
            url=f"socket://{host}:{port}",
            **kwargs,
        )

    def close(self) -> None:
        self.tether.stop()
        self.wire.close()


@pytest.fixture
def make_znp_server(mocker):
    """Their fixture, with the wire made real.

    The body mirrors theirs deliberately — same config defaults, same shortened delays, same
    attributes hung on the server — so that a divergence between the two runs is the transport
    and not a fixture we rewrote from memory.
    """
    rigs: list[TetherRig] = []
    tmp = tempfile.TemporaryDirectory(prefix="tether-suite-")

    def inner(server_cls, config=None, shorten_delays=True):
        if config is None:
            config = theirs.config_for_port_path(theirs.FAKE_SERIAL_PORT)

        if shorten_delays:
            mocker.patch("zigpy_znp.api.AFTER_BOOTLOADER_SKIP_BYTE_DELAY", 0.001)
            mocker.patch("zigpy_znp.api.BOOTLOADER_PIN_TOGGLE_DELAY", 0.001)

        server = server_cls(config)
        server._transports = []
        server.port_path = theirs.FAKE_SERIAL_PORT
        server.serial_port = theirs.FAKE_SERIAL_PORT

        harness = TetherRig(server, pathlib.Path(tmp.name) / f"rig{len(rigs)}")
        rigs.append(harness)
        mocker.spy(server._uart, "data_received")

        mocker.patch("zigpy_znp.uart.create_serial_connection", new=harness.connect_client)
        return server

    yield inner

    # A tether that died mid-test must not be allowed to look like a client bug: without this
    # it surfaces as whatever timeout the client hits next, and the suite would report a zigpy
    # failure for a tether fault — the most misleading result this exercise could produce.
    died = [r for r in rigs if r.tether.proc.poll() not in (None, 0, -15)]
    for r in rigs:
        r.close()
    tmp_said = [r.tether.output() for r in died]
    tmp.cleanup()
    if died:
        pytest.fail("tether exited during the test:\n" + "\n".join(tmp_said))
