# SPDX-License-Identifier: GPL-3.0-or-later
#
# ⚠️ GPL-3 with the rest of this directory: it exists to bind zigpy-znp's GPL-3 emulator to a
# pty. Nothing here is linked into the briard-tether binary.
"""A pty for an emulated coordinator to speak through, and a stable name over it.

Extracted when the third caller appeared — `fake_coordinator.py`, the suite's
`application_conftest.py` and `unplug_scenario.py` — which is the three real call sites an
abstraction has to earn here. Two copies of twenty lines is the right answer below that line.
"""

from __future__ import annotations

import os
import pathlib
import termios
import tty


class PtyTransport:
    """The asyncio transport an emulated coordinator writes its replies into.

    It is the counterpart of zigpy-znp's own `ForwardingSerialTransport` and just as small:
    theirs calls the other protocol's `data_received` directly, and this one writes to a pty
    master. Everything a protocol asks of a transport is here.
    """

    def __init__(self, fd: int) -> None:
        self._fd = fd

    def write(self, data: bytes) -> None:
        # os.write on a tty can be short. A frame arriving in two pieces is legal on a real
        # UART and tether must not care, but a frame silently truncated would look like the
        # emulator being wrong, so the loop is not optional.
        while data:
            data = data[os.write(self._fd, data) :]

    def close(self) -> None:
        pass  # PtyLink closes the pty's descriptors; see PtyLink.close.

    async def set_modem_pins(self, *, dtr=None, rts=None, **kwargs) -> None:
        # A pty has none — measured: TIOCMGET is ENOTTY on both ends — and over a socket
        # neither does the client, which does skip-bootloader in band instead.
        # Accepting and ignoring is what the real path does too.
        pass


class PtyLink:
    """One pty pair, optionally under a stable name, for one generation of a device.

    "Generation" is the word tether's own supervision loop uses and it is the right one here:
    a replug is a *new node under the same name*, so staging one means closing this and opening
    another with the same link. That is what `/dev/serial/by-id/…` is and why tether resolves
    the name afresh on every open.
    """

    def __init__(self, link: pathlib.Path | None = None) -> None:
        self.master_fd, self.slave_fd = os.openpty()
        # Raw, and this is not a detail: a fresh slave has ECHO set, and an emulator's replies
        # are *input* to the slave, so they would be echoed back to the master and parsed as
        # though the client had sent them. tether sets the port raw when it opens, but that is
        # later and only while it is attached.
        tty.setraw(self.slave_fd, termios.TCSANOW)
        self.path = os.ttyname(self.slave_fd)

        # What tether's config must call this coordinator, and what a harness therefore
        # writes into it: the stable name when there is one, the pty itself when there is not.
        # `rig.SerialWire` answers the same question with `COM1`, which is the whole of the
        # difference between running tether here and running it on the far machine.
        self.device = str(link) if link is not None else self.path
        # What a reader watches. Named for the job rather than for the mechanism so that a
        # socket-backed wire can stand in for a pty here (rig.py); `master_fd` stays because
        # the pty's two descriptors are the thing this class exists to own.
        self.fd = self.master_fd

        self.link = link
        # Set by whoever starts reading this pty, so that close() can stop the read first.
        # It is not optional bookkeeping: closing a descriptor that the event loop is still
        # watching leaves a stale selector registration, and `openpty` hands out the *lowest
        # free* descriptors — so the next pty gets the same numbers and inherits the wreckage.
        # That bug looks exactly like a coordinator that went quiet after a replug.
        self.loop = None
        if link is not None:
            if link.is_symlink() or link.exists():
                link.unlink()
            link.symlink_to(self.path)

    def __str__(self) -> str:
        return self.path

    def transport(self) -> PtyTransport:
        return PtyTransport(self.master_fd)

    def close(self, *, unlink: bool = True) -> None:
        """Close the pair. To tether this is the adapter disappearing.

        Both descriptors go: while any slave is open the pty survives, and a device that is
        still there is not an unplug.
        """
        if self.loop is not None:
            # See rig.SerialWire.close: a loop pytest-asyncio has already closed must not turn
            # tidying up into a test error.
            if not self.loop.is_closed():
                self.loop.remove_reader(self.master_fd)
            self.loop = None
        for fd in (self.slave_fd, self.master_fd):
            try:
                os.close(fd)
            except OSError:
                pass
        if unlink and self.link is not None and self.link.is_symlink():
            self.link.unlink()
