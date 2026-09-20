# SPDX-License-Identifier: GPL-3.0-or-later
#
# ⚠️ GPL-3, and it could carry no other licence: this file imports zigpy and zigpy-znp, both
# GPL-3, and a module that imports a GPL-3 library is plausibly a derivative work.
# The Apache-2.0 tree around it is unaffected — mere aggregation — and nothing here is linked
# into the briard-tether binary or appears in any release artifact. tether meets this code the
# way it meets any client: over a TCP socket, at arm's length, as a separate process.
"""The client gate: a real zigpy client driving a real radio through tether.

This is the acceptance bar for anything touching the data path — ARCHITECTURE.md "Testing" —
and the shortest honest form of it. `ControllerApplication` is the class Home Assistant's ZHA
instantiates and `socket://host:port` is the transport it uses for a networked coordinator;
Home Assistant adds a config flow and entity plumbing above this and touches none of it. So a
client here is not *like* ZHA — below the config flow it is the same code.

It reads and never writes. `load_network_info` interrogates the coordinator's NVRAM; nothing
forms, resets, or re-keys a network, so it is safe against a stick that is carrying one. The
one write anywhere near it — `auto_form` — is not reachable from this file on purpose.

What it proves, in the order it proves it:

  1. a real Zigbee stack completes its handshake through tether, which no scripted responder
     can stand in for: the byte stream has to satisfy a real MT framer, not a table;
  2. the coordinator's own identity comes back intact — IEEE address, PAN, channel;
  3. INV 1 with a real client on the end of it. The client disconnects and reconnects against
     a live tether, and the radio answers the second client with exactly what it told the
     first. A transport that reset the radio on client lifecycle — the failure this project
     exists to stop being — would answer the second one differently, or not at all.

Usage:

    briard-tether run -config <device: /dev/serial/by-id/usb-…, listen: 127.0.0.1:6638>
    python3 tests/client_gate.py 127.0.0.1:6638

Neither command wants root. The tty group is all tether needs for a real stick, and this side
needs nothing at all — it is a TCP client.
"""

from __future__ import annotations

import argparse
import asyncio
import sys

import zigpy.config
from zigpy.exceptions import NetworkNotFormed

import zigpy_znp.config as conf
from zigpy_znp.zigbee.application import ControllerApplication


def describe(app: ControllerApplication) -> dict[str, str]:
    """The facts worth comparing across a reconnect.

    The network key is deliberately not among them. It is a secret on somebody's real network,
    this output is meant to be pasted into a runbook or an issue, and "a key is present" is the
    whole of what this test needs to know about it.
    """
    node = app.state.node_info
    net = app.state.network_info
    return {
        "ieee": str(node.ieee),
        "nwk": f"0x{node.nwk:04X}",
        "logical_type": str(node.logical_type),
        "pan_id": f"0x{net.pan_id:04X}",
        "extended_pan_id": str(net.extended_pan_id),
        "channel": str(net.channel),
        "nwk_update_id": str(net.nwk_update_id),
        "network_key": "present" if net.network_key else "absent",
    }


async def interrogate(address: str) -> dict[str, str]:
    """Connect as ZHA would, read what the coordinator says about itself, and leave.

    A coordinator with no network formed is not an error here. zigpy reads the NIB first and
    raises `NetworkNotFormed` before it ever reaches the IEEE address, so a blank stick can
    report nothing about itself — but "I read your NVRAM and there is no network in it" is
    still a real answer from a real radio, arrived at through tether, and it is still the same
    answer twice if the radio was not disturbed. So it is reported as a fact and compared like
    any other.
    """
    # Handed over unvalidated, which is what zigpy-znp's own conftest does for its client
    # config (`apply_schema=False`). ControllerApplication validates it in __init__, and
    # zigpy 1.4.1's schema is not idempotent: validating first turns the OTA providers into
    # objects, and the second pass calls .get() on them and raises AttributeError.
    config = {
        conf.CONF_DEVICE: {conf.CONF_DEVICE_PATH: f"socket://{address}"},
        # Nothing here should write a backup file into the repo, and this run is not
        # anybody's backup.
        zigpy.config.CONF_NWK_BACKUP_ENABLED: False,
    }

    app = ControllerApplication(config)
    try:
        # `startup` is ZHA's boot, not a lighter stand-in for it: connect, read the network out
        # of NVRAM, and bring the coordinator up on it. `auto_form` is False and is never
        # anything else here — this file may start a network that exists and must never create
        # one. Forming a network is a separate program that lives nowhere near this one.
        await app.startup(auto_form=False)
        return describe(app)
    except NetworkNotFormed:
        return {"network": "not formed"}
    finally:
        await app.shutdown(db=False)


async def run(address: str) -> int:
    print(f"connecting as a zigpy client to socket://{address}")
    first = await interrogate(address)
    for key, value in first.items():
        print(f"  {key:16} {value}")

    # INV 1 with a real stack on the end: the client goes away and comes back, and the radio
    # must not have noticed. A transport that reset the coordinator would either fail the
    # second connection or answer it with different facts.
    print("disconnecting, then reconnecting against the same live tether")
    second = await interrogate(address)

    if first != second:
        print("\nFAIL: the radio answered the second client differently", file=sys.stderr)
        for key in sorted(set(first) | set(second)):
            if first.get(key) != second.get(key):
                print(f"  {key}: {first.get(key)!r} → {second.get(key)!r}", file=sys.stderr)
        print(
            "\n  A client reconnecting changed what the coordinator says about itself, which\n"
            "  means the client lifecycle reached the radio. That is INV 1 broken, and it is\n"
            "  the failure this project exists to stop being.",
            file=sys.stderr,
        )
        return 1

    print("the radio answered the second client exactly as it answered the first (INV 1)")

    if "network" in first:
        # Everything above is real and none of it is nothing: a genuine MT handshake through
        # tether, twice, and NVRAM read through it, twice. But a blank coordinator can say
        # nothing about itself, so the strongest half of this test — that the *identity and
        # the network* survive a client restart — is not what just ran.
        print(
            "\n  NOTE: this coordinator has no network formed, so the comparison above is\n"
            "  between two 'not formed' answers. The full gate wants a formed network: form\n"
            "  one on a channel of its own and never a household one, then re-run."
        )
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument(
        "address",
        help="where tether is listening, host:port — the address a client would be given",
    )
    args = parser.parse_args()
    try:
        return asyncio.run(run(args.address))
    except (OSError, asyncio.TimeoutError) as exc:
        print(f"\nFAIL: could not reach a coordinator through {args.address}: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
