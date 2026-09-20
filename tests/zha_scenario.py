# SPDX-License-Identifier: GPL-3.0-or-later
#
# ⚠️ GPL-3 by association rather than by import: Home Assistant is Apache-2.0 and is
# driven as a subprocess over HTTP, never linked, but this file places the emulator beside it and
# shares `run_gate`'s helpers with the files that do import zigpy-znp. Nothing here reaches the
# briard-tether binary or any release artifact.
"""A real ZHA through tether: the config flow, the zeroconf card, and losing the radio.

What `client_gate.py` proves is `ControllerApplication` — ZHA's engine, driven by a harness of
ours. *A real ZHA* is Home Assistant with the `zha` integration, and everything this file
measures lives **above** that object: the config flow that decides what to do with a radio's
existing network, the zeroconf card tether's TXT record exists to raise, and the reload that a
lost radio triggers, which INV 3 assumes rather than proves.

Five cases, in the order they run:

  (1) **the manual path**       `socket://host:port` typed in by hand, which is what a ZHA user
                                does with any networked coordinator today. The entry must come
                                up `loaded` **on the network the radio already carries** — INV
                                8's parity claim, on the real consumer rather than a harness.
  (2) **the zeroconf card**     `--discover` only. HA browses, finds tether's advert, and the
                                flow carries our `radio_type` and `serial_number` through
                                verbatim. Nobody types an address.
  (3) **the restart**           `homeassistant/restart` over the API. HA exits 100 for its
                                supervisor — this harness is that supervisor — and the entry
                                comes back on the same network.
  (4) **the client kill**       `SIGKILL` on `hass`, and a fresh one arrives while tether still
                                holds the corpse's socket. INV 4 with a real ZHA on it.
  (5) **the unplug**            the radio goes while ZHA holds it. The entry must fall over and
                                then **come back by itself** once the radio returns: no restart,
                                no hand on it. That is the half of INV 3 that is otherwise
                                assumed.

Every case after (1) asserts the same thing in the end — `unique_id` is still `epid=…`, the same
extended PAN id — because "the network survived" is the only question any of them is really
about.

Usage, no dongle needed. ⚠️ **The `.#zha` shell, not the default one:** Home Assistant is 1.3 GiB
unpacked and is not a tax the Go loop should pay (flake.nix says why).

    nix develop --command go build -o briard-tether ./cmd/briard-tether
    nix develop .#zha --command python3 tests/zha_scenario.py
    nix develop .#zha --command python3 tests/zha_scenario.py --discover

And against a real stick, as yourself, with the tty group and nothing else:

    nix develop .#zha --command python3 tests/zha_scenario.py --discover \\
        --stick /dev/serial/by-id/usb-ITead_Sonoff_…-port0 \\
        --unplug "sudo sh -c 'echo 0 > /sys/bus/usb/devices/<node>/authorized'" \\
        --replug "sudo sh -c 'echo 1 > /sys/bus/usb/devices/<node>/authorized'"

⚠️ **The radio must already carry a network**, which is why `--device` defaults to the *formed*
fixture. This harness never forms one: the flow is always walked to `reuse_settings`, because
resumption is the branch every start after day one takes, and a run that formed a new network
would have overwritten the very thing cases (3) to (5) check is still there. Forming through
tether is measured on the Z2M side instead, where the dongle is disposable by policy.

`--discover` re-execs the whole scenario into a network namespace with nothing but loopback in
it, for the reasons `run_gate.into_a_private_network` sets out. It is what case (2) needs, and it
is also *courtesy*: a coordinator advert has no business on somebody's LAN for the length of a
test.

**Over the seam** (`TETHER_WINDOWS_SSH`) only tether moves: the emulator and Home
Assistant stay here, and tether opens the far machine's COM port instead of a pty. Cases (1),
(3) and (4) run unchanged. Case (5) is skipped and says so — the emulator's plug event is its
end of the wire closing, and the hypervisor keeps the guest's COM port up regardless, so staging
it there would measure the rig. Case (2) is refused for a different reason: the card needs Home
Assistant on a segment with the far machine — `run_gate.py`'s `OnTheGuestsSegment` is what
builds one, and this harness does not use it yet.
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import pathlib
import sqlite3
import secrets
import shutil
import socket
import struct
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

import rig  # noqa: E402
from run_gate import (  # noqa: E402
    Coordinator,
    free_port,
    into_a_private_network,
    wait_for,
)

# What `hass` exits with when something asks it to restart — its own `--help` says so. It is the
# ZHA-side twin of zigbee2mqtt's exit 2: the process does not restart itself, it tells whoever
# started it to do it, and case (3) is this harness being that whoever.
RESTART_EXIT_CODE = 100

# The account the harness onboards as. It exists for the length of a temp directory and reaches
# nothing outside it; the password is here rather than generated so that a failed run can be
# poked at by hand, with the config dir the report names.
USER = "gate"
PASSWORD = "a password for a directory that will not survive this run"

# The extended PAN id in zigpy-znp's `FormedLaunchpadCC26X2R1` NVRAM image — the same value
# the zigpy gate reads back through tether, and the emulator's channel is 15 where the test
# Sonoff is 20, so the two coordinators cannot be mistaken for each other. ZHA turns it into the
# config entry's unique id, which is the one field that proves ZHA *read the radio* rather than
# taking anything this harness told it.
EMULATOR_UNIQUE_ID = "epid=a2:ba:38:a8:b5:e6:83:a0"

# The flow's own words. Written out rather than imported from `homeassistant.components.zha`,
# and deliberately: these are the steps a *user* is walked through, so a test that borrowed the
# constants would follow HA through a rename that had broken every user's muscle memory — the
# same reason the wire test writes out the mDNS service type instead of importing ours.
ADVANCED = "setup_strategy_advanced"
# The same menu under a different name. `async_step_verify_radio` routes to
# `choose_setup_strategy` when no ZHA entry exists and to `choose_migration_strategy` when one
# does, and the advanced branch of either arrives at the same `choose_formation_strategy` — so
# this is one id, not one more decision.
MIGRATION_ADVANCED = "migration_strategy_advanced"
KEEP_THE_NETWORK = "reuse_settings"
FORM_NEW = "form_new_network"
# The options flow's own first menu: reconfigure this radio, or migrate to a different one. The
# difference is `_async_reset_old_radio`, which only `intent_migrate` reaches.
RECONFIGURE = "intent_reconfigure"

# What `--wrong-radio` makes tether claim in front of the Z-Stack emulator (case (c)).
# `ezsp` and not `deconz` on purpose: ZHA takes the TXT's `radio_type` verbatim on the zeroconf
# path and never probes, so this is a supported driver doing real reads over the wrong protocol —
# the case worth staging, and the one a family-table mistake actually produces.
WRONG_RADIO = "ezsp"

# zigpy's own enums, as `zigbee.db` stores them: a device with its endpoints read, an endpoint
# past ZDO init, and the logical types. Written out rather than imported because this harness
# talks to Home Assistant over HTTP and imports none of its stack.
DEVICE_ENDPOINTS_INIT = 2
ENDPOINT_ZDO_INIT = 1
LOGICAL_ROUTER, LOGICAL_END_DEVICE = 1, 2


def ha_config(port: int, discover: bool) -> str:
    """As close to an empty `configuration.yaml` as this can be, and that is the point.

    A ZHA user's file says nothing about ZHA — the integration is a config *entry*, made by the
    flow this harness walks, and YAML has had nothing to do with it for years. So the only line
    here that is not the http port is `zeroconf:`, and only when case (2) needs HA to browse.

    What HA sets up regardless is its own default set, which is where `frontend` — and with it
    `api`, `auth`, `config`, `onboarding` and `websocket_api` — comes from. ⚠️ Those are not
    optional extras: a Home Assistant whose frontend fails to load drops into **recovery mode**,
    and the config-entries API this file drives does not exist there at all. flake.nix carries
    the other half of that.
    """
    lines = [f"http:\n  server_port: {port}\n"]
    if discover:
        # Only when the card is the thing under test. HA's zeroconf both browses and advertises,
        # and neither belongs on anybody's network for the length of a test — which the private
        # namespace this scenario re-execs into is what actually guarantees.
        lines.append("zeroconf:\n")
    return "".join(lines)


class WebSocket:
    """Enough of RFC 6455 to ask Home Assistant one question.

    **Why this exists at all:** the list of *discovered* config flows — the cards a user sees on
    the integrations page — is reachable only over the websocket API. `GET` on the REST flow
    endpoint is explicitly `HTTPMethodNotAllowed` (config/config_entries.py), so case (2) cannot
    see the card any other way.

    It is written out rather than depended on because of what it replaces: one command, one
    connection, no reconnection, no extensions, no fragmentation to send. A websocket client
    library would be a third of a megabyte of somebody else's code to make one call.
    Server frames *are* handled fragmented, because a flow list is easily past 126 bytes and the
    length encodings are three lines each.
    """

    def __init__(self, host: str, port: int) -> None:
        self.sock = socket.create_connection((host, port), timeout=30)
        key = base64.b64encode(secrets.token_bytes(16)).decode()
        self.sock.sendall(
            f"GET /api/websocket HTTP/1.1\r\n"
            f"Host: {host}:{port}\r\n"
            f"Upgrade: websocket\r\nConnection: Upgrade\r\n"
            f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n".encode())
        self.buf = b""
        while b"\r\n\r\n" not in self.buf:
            self.buf += self._recv()
        head, _, self.buf = self.buf.partition(b"\r\n\r\n")
        status = head.split(b"\r\n")[0]
        if b"101" not in status:
            raise RuntimeError(f"no websocket upgrade from Home Assistant: {status!r}")

    def _recv(self) -> bytes:
        chunk = self.sock.recv(65536)
        if not chunk:
            raise RuntimeError("Home Assistant closed the websocket")
        return chunk

    def _take(self, n: int) -> bytes:
        while len(self.buf) < n:
            self.buf += self._recv()
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def recv(self) -> dict:
        while True:
            first, second = self._take(2)
            opcode, length = first & 0x0F, second & 0x7F
            if length == 126:
                length = struct.unpack(">H", self._take(2))[0]
            elif length == 127:
                length = struct.unpack(">Q", self._take(8))[0]
            payload = self._take(length)
            if opcode == 0x9:  # ping: answer it, or HA hangs up mid-question
                self.sock.sendall(self._frame(payload, 0xA))
                continue
            if opcode == 0x8:
                raise RuntimeError("Home Assistant closed the websocket")
            return json.loads(payload)

    @staticmethod
    def _frame(payload: bytes, opcode: int = 0x1) -> bytes:
        # Client frames must be masked — a server that sees an unmasked one is required to close
        # the connection, which is exactly how this presents when the mask is forgotten.
        mask = secrets.token_bytes(4)
        masked = bytes(byte ^ mask[i % 4] for i, byte in enumerate(payload))
        head = bytes([0x80 | opcode])
        if len(payload) < 126:
            head += bytes([0x80 | len(payload)])
        elif len(payload) < 1 << 16:
            head += bytes([0x80 | 126]) + struct.pack(">H", len(payload))
        else:
            head += bytes([0x80 | 127]) + struct.pack(">Q", len(payload))
        return head + mask + masked

    def command(self, command: str, token: str) -> list | dict:
        """Authenticate, ask, and return the result. One command per connection is enough here
        and keeps the id bookkeeping to a single number."""
        if self.recv()["type"] == "auth_required":
            self.sock.sendall(self._frame(
                json.dumps({"type": "auth", "access_token": token}).encode()))
            if self.recv()["type"] != "auth_ok":
                raise RuntimeError("Home Assistant refused the token on the websocket")
        self.sock.sendall(self._frame(json.dumps({"id": 1, "type": command}).encode()))
        while True:
            message = self.recv()
            if message.get("id") == 1 and message["type"] == "result":
                if not message["success"]:
                    raise RuntimeError(f"{command}: {message['error']}")
                return message["result"]

    def close(self) -> None:
        self.sock.close()


class HomeAssistant:
    """One `hass` on one config directory, and the API this harness talks to it through.

    The config directory outlives any one process on purpose: cases (3), (4) and (5) all end with
    a Home Assistant that is *not the one that started*, and the whole question is whether the
    ZHA entry it finds on disk comes back up on the same network.
    """

    def __init__(self, config: pathlib.Path, workdir: pathlib.Path, name: str, port: int) -> None:
        self.config, self.name, self.port = config, name, port
        self.base = f"http://127.0.0.1:{port}"
        # HA's own client-id rules are indieauth's: a URL, and the redirect must share its host.
        # Its own address satisfies both and is never fetched, 127.0.0.1 being local.
        self.client_id = f"{self.base}/"
        self.log_path = workdir / f"{name}.log"
        self.log = self.log_path.open("wb")
        # `--skip-pip` is already baked into the wrapper by nixpkgs, and it matters: every
        # requirement comes from flake.lock, so a Home Assistant that reached for pip here would
        # either fail or — worse — quietly test a version nothing pinned.
        self.proc = subprocess.Popen(
            [os.environ["HASS"], "-c", str(config), "--log-no-color"],
            stdout=self.log, stderr=subprocess.STDOUT,
        )

    # ── the process ──────────────────────────────────────────────────────────────────────────
    def died(self) -> bool:
        return self.proc.poll() is not None

    def output(self) -> str:
        if not self.log.closed:
            self.log.flush()
        return self.log_path.read_text(errors="replace").strip()

    def kill(self) -> None:
        self.proc.kill()
        self.proc.wait(timeout=30)

    def stop(self) -> None:
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=60)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=30)
        self.log.close()

    def serving(self, timeout: float = 300.0) -> None:
        """Wait until the API answers, or say what happened instead.

        ⚠️ **Any HTTP answer counts, and that is the whole subtlety.** `/api/` without a token
        is a 401 and `/api/onboarding` on an already-onboarded instance is a **404** — the
        onboarding component only loads while there is onboarding left to do, so its endpoint
        vanishes for every start after the first. A check that waited for a *successful*
        response would therefore hang for the full timeout against a Home Assistant that was
        serving perfectly, and report it as one that never came up.

        ⚠️ **Generous on purpose.** A first start builds a registry, a store and an auth
        provider from nothing; a later one replays them and sets up ZHA on the way. Both are
        slower than anything else in this tree, and a tight timeout here would report "Home
        Assistant is broken" for a busy machine.
        """
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                self.api("/api/")
                return
            except urllib.error.HTTPError:
                return  # it answered; being refused by it is not being unable to reach it
            except OSError:
                pass
            if self.died():
                raise RuntimeError(
                    f"{self.name} exited on the way up (code {self.proc.returncode})")
            time.sleep(1.0)
        raise TimeoutError(f"{self.name} never answered on {self.base}")

    # ── the API ──────────────────────────────────────────────────────────────────────────────
    def api(self, path: str, data: dict | None = None, token: str | None = None,
            form: bool = False) -> dict | list | None:
        """One request. A body makes it a POST, as every call here but the reads is."""
        body: bytes | None = None
        headers = {}
        if data is not None:
            if form:
                body = urllib.parse.urlencode(data).encode()
                headers["Content-Type"] = "application/x-www-form-urlencoded"
            else:
                body = json.dumps(data).encode()
                headers["Content-Type"] = "application/json"
        if token is not None:
            headers["Authorization"] = f"Bearer {token}"
        request = urllib.request.Request(self.base + path, data=body, headers=headers)
        with urllib.request.urlopen(request, timeout=120) as response:
            raw = response.read().decode()
        return json.loads(raw) if raw.strip() else None

    def onboard(self) -> str:
        """Create the first account and finish onboarding, returning a token for everything else.

        ⚠️ **Finishing it is not a formality, it is a precondition of what case (1) measures.**
        ZHA asks `onboarding.async_is_onboarded` twice in the flow, and while it is False it
        takes the fast paths a new user gets: a discovery is not even confirmed, and a blank
        radio is *formed automatically* with no menu shown. An unonboarded HA would walk a
        different flow from every real user's, and could form a network on a radio this harness
        promised not to touch.
        """
        code = self.api("/api/onboarding/users", {
            "client_id": self.client_id, "name": USER, "username": USER,
            "password": PASSWORD, "language": "en",
        })["auth_code"]
        token = self.api("/auth/token", {
            "grant_type": "authorization_code", "code": code, "client_id": self.client_id,
        }, form=True)["access_token"]
        for step, payload in (
            ("core_config", {}),
            ("analytics", {}),
            ("integration", {"client_id": self.client_id,
                             "redirect_uri": f"{self.client_id}?auth_callback=1"}),
        ):
            self.api(f"/api/onboarding/{step}", payload, token=token)
        remaining = [s["step"] for s in self.api("/api/onboarding") if not s["done"]]
        if remaining:
            raise RuntimeError(f"onboarding did not finish: {remaining} left")
        self.forget_the_onboarding_extras(token)
        return token

    def forget_the_onboarding_extras(self, token: str) -> None:
        """Remove the four config entries onboarding makes for a *person*, which this is not.

        Finishing core config kicks off flows for `google_translate`, `met`, `radio_browser` and
        `shopping_list` — a text-to-speech, a weather service, a radio directory and a list. None
        of them has anything to do with a Zigbee coordinator, and two of them cannot work here at
        all: nixpkgs installs the requirements of the components it is *asked* for, and this
        shell asks for `zha`, `frontend` and `zeroconf`.

        ⚠️ **Leaving them costs the run everything, and the way it fails is a trap.** A
        `google_translate` entry whose `gtts` is absent throws inside the import executor while
        HA is still bootstrapping, and the *next* start — case (3)'s — hangs there before it ever
        opens its HTTP port, so the failure reads as "Home Assistant never came up" three cases
        away from its cause. The alternative was to install all four, which would have put a
        weather fetch and a DNS lookup into a test that is otherwise sealed inside a loopback
        namespace.

        They are created as background tasks, so this sweeps for a few seconds rather than
        reading once, and does not insist on finding all four.
        """
        theirs = {"google_translate", "met", "radio_browser", "shopping_list"}
        gone: set[str] = set()
        deadline = time.monotonic() + 20.0
        while time.monotonic() < deadline:
            for entry in self.api("/api/config/config_entries/entry", token=token):
                if entry["domain"] in theirs:
                    self.api_delete(f"/api/config/config_entries/entry/{entry['entry_id']}",
                                    token)
                    gone.add(entry["domain"])
            if gone == theirs:
                break
            time.sleep(0.5)
        if gone:
            print(f"  dropped onboarding's own integrations: {', '.join(sorted(gone))}",
                  flush=True)

    def api_delete(self, path: str, token: str) -> None:
        request = urllib.request.Request(self.base + path, method="DELETE",
                                         headers={"Authorization": f"Bearer {token}"})
        urllib.request.urlopen(request, timeout=60).close()

    def sign_in(self) -> str:
        """A token on a Home Assistant that is already onboarded — every start after the first.

        The password grant is what the login form uses, and it is the only way back in once
        onboarding is done: `/api/onboarding/users` refuses, and the first process's token died
        with it.
        """
        code = self.api("/auth/login_flow", {
            "client_id": self.client_id, "handler": ["homeassistant", None],
            "redirect_uri": f"{self.client_id}?auth_callback=1",
        })
        result = self.api(f"/auth/login_flow/{code['flow_id']}", {
            "username": USER, "password": PASSWORD, "client_id": self.client_id,
        })
        return self.api("/auth/token", {
            "grant_type": "authorization_code", "code": result["result"],
            "client_id": self.client_id,
        }, form=True)["access_token"]

    def zha_entry(self, token: str) -> dict | None:
        """The ZHA config entry as HA reports it, or None while there is not one."""
        entries = self.api("/api/config/config_entries/entry?domain=zha", token=token)
        return entries[0] if entries else None

    def discovered_zha_flows(self, token: str) -> list[dict]:
        """The cards on the integrations page, which only the websocket API lists."""
        socket_ = WebSocket("127.0.0.1", self.port)
        try:
            flows = socket_.command("config_entries/flow/progress", token)
        finally:
            socket_.close()
        return [flow for flow in flows if flow["handler"] == "zha"]

    def stored_unique_id(self) -> str | None:
        """The entry's unique id, read out of HA's own storage.

        Not available over REST — `entry_json` does not carry it — and it is the field that
        matters most here: ZHA builds it as `epid=<extended pan id>` from the network it *read
        off the radio*, so it is the one value in the entry that this harness could not have
        told it.

        ⚠️ HA writes that store lazily, so this returns None for a second or so after a flow
        finishes and callers wait rather than read once.
        """
        path = self.config / ".storage" / "core.config_entries"
        if not path.is_file():
            return None
        try:
            stored = json.loads(path.read_text())
        except json.JSONDecodeError:
            return None  # caught it mid-write; the caller is polling
        for entry in stored["data"]["entries"]:
            if entry["domain"] == "zha":
                return entry.get("unique_id")
        return None


def loaded(ha: HomeAssistant, token: str, timeout: float = 300.0) -> bool:
    """Wait for the ZHA entry to be up, which is ZHA holding the radio through tether."""
    try:
        wait_for(lambda: (entry := ha.zha_entry(token)) is not None and entry["state"] == "loaded",
                 "the ZHA config entry to load", timeout=timeout)
        return True
    except TimeoutError:
        return False


def walk_to_reuse(ha: HomeAssistant, token: str, flow: dict, findings: list[str],
                  endpoint: str = "/api/config/config_entries/flow") -> dict:
    """Drive the flow from wherever it is to an entry, keeping the radio's own network.

    **The one choice this harness makes, and the reason it makes it:** every menu is walked to
    `reuse_settings`. `setup_strategy_recommended` — the button a real user is nudged towards —
    *forms a brand-new network*, which on a coordinator carrying one is the destructive answer,
    and it would erase the very thing cases (3) to (5) then check is still there. The advanced
    branch is where a user who wants to keep their network goes, and it is the branch every
    migration onto tether takes.
    """
    while flow["type"] == "menu":
        options = flow["menu_options"]
        if ADVANCED in options:
            choice = ADVANCED
        elif MIGRATION_ADVANCED in options:
            choice = MIGRATION_ADVANCED
        elif KEEP_THE_NETWORK in options:
            choice = KEEP_THE_NETWORK
        else:
            # Reached only when the radio carried no network to reuse, which for this harness is
            # a broken premise rather than a step: `form_new_network` is the only way on, and
            # taking it would commission the coordinator. Say so and stop.
            findings.append(
                f"FAULT: the flow offered no way to keep the radio's network ({options}) — the "
                f"coordinator is not carrying one, so there is nothing for ZHA to resume and "
                f"{'a new network would be formed' if FORM_NEW in options else 'the flow is not the one this harness knows'}")
            return flow
        flow = ha.api(f"{endpoint}/{flow['flow_id']}", {"next_step_id": choice}, token=token)
        print(f"  menu {options} → {choice}", flush=True)
    return flow


def configure_by_hand(ha: HomeAssistant, token: str, address: str, findings: list[str]) -> dict:
    """Case (1): the flow a user starts from the Add Integration button.

    ⚠️ **The slow step is ZHA probing, and it is not tether being slow.** Handed a path, ZHA
    autoprobes its recommended radios in order — ezsp, then znp — and the ezsp attempt has to
    time out first, which costs the better part of a minute and shows up in tether's log as two
    short-lived clients before the one that stays. The zeroconf path in case (2) skips all of it,
    because our TXT record says `radio_type` outright: the advert's value is not a convenience,
    it is the difference between a card that connects at once and a minute of probing.
    """
    flow = ha.api("/api/config/config_entries/flow",
                  {"handler": "zha", "show_advanced_options": True}, token=token)
    if flow.get("step_id") != "choose_serial_port":
        findings.append(f"FAULT: the ZHA flow opened on `{flow.get('step_id')}`, not the serial "
                        f"port question this harness knows how to answer")
        return flow
    flow = ha.api(f"/api/config/config_entries/flow/{flow['flow_id']}",
                  {"path": address}, token=token)
    return walk_to_reuse(ha, token, flow, findings)


def adopt_the_card(ha: HomeAssistant, token: str, advertised: str,
                   findings: list[str]) -> dict | None:
    """Case (2): the flow *HA itself* raised, from tether's advert. Nobody typed an address.

    The two fields the TXT record carries are both load-bearing here and are checked
    before the flow is touched: `serial_number` becomes the discovery's unique id, which is what
    stops the same coordinator raising a second card, and `radio_type` is taken verbatim as the
    radio family — ZHA never probes on this path.
    """
    def zeroconf_cards() -> list[dict]:
        """⚠️ **The zeroconf ones, and only those.** Home Assistant raises a ZHA card for every
        way it can see a coordinator, and `usb` is one of them — so a real dongle in the machine
        running this harness raises its own card, which sorts first and would be adopted instead
        of tether's. The namespace `--discover` re-execs into isolates the *network*; it does not
        unplug the machine's hardware. Taking the wrong one migrates a populated ZHA onto somebody
        else's Sonoff and reports success, because nothing downstream would notice: this filter
        and the path assertion in `run_migration` are the two things that stop it."""
        return [f for f in ha.discovered_zha_flows(token)
                if f["context"].get("source") == "zeroconf"]

    try:
        wait_for(lambda: bool(zeroconf_cards()),
                 "Home Assistant to discover the advert", timeout=180.0)
    except TimeoutError:
        seen = [f"{f['context'].get('source')}:{f['context'].get('unique_id')}"
                for f in ha.discovered_zha_flows(token)]
        findings.append("FAULT: Home Assistant never raised a *zeroconf* ZHA card for tether's "
                        f"advert `{advertised}` — either nothing was advertised, or nothing in "
                        f"this namespace saw it"
                        f"{f'. What it did raise: {seen}' if seen else '. It raised nothing at all'}")
        return None
    flow = zeroconf_cards()[0]
    context = flow["context"]
    findings.append(
        f"Home Assistant discovered tether by itself and raised a ZHA card for it, "
        f"source `{context['source']}`, named `{context['unique_id']}`")
    findings.append(
        "the card's identity is the `serial_number` from our TXT record, which is what keeps one "
        "coordinator to one card"
        if context["unique_id"] == advertised else
        f"FAULT: the card's identity is `{context['unique_id']}` and we advertised "
        f"`{advertised}` — a mismatch here means rediscovery raises a second card each time")
    # `confirm` is a dialog with no fields: posting nothing is the user pressing the button.
    flow = ha.api(f"/api/config/config_entries/flow/{flow['flow_id']}", {}, token=token)
    return walk_to_reuse(ha, token, flow, findings)


def check_entry(ha: HomeAssistant, token: str, flow: dict, address: str, expect_id: str | None,
                findings: list[str], what: str) -> str | None:
    """What the finished flow wrote: the entry, its radio type, its path, its unique id."""
    if flow.get("type") != "create_entry":
        findings.append(f"FAULT: {what}: the flow ended as `{flow.get('type')}` "
                        f"(step `{flow.get('step_id')}`, errors {flow.get('errors')}) rather "
                        f"than writing a config entry")
        return None
    state = flow["result"]["state"]
    findings.append(
        f"{what}: the ZHA config entry came up `{state}` — a real Home Assistant holding a real "
        f"radio through tether"
        if state == "loaded" else
        f"FAULT: {what}: the entry was created but is `{state}` ({flow['result'].get('reason')})")

    # The store rather than the REST view, which carries neither the device settings nor the
    # unique id — and it is written lazily, so this waits for it rather than reading once.
    wait_for(lambda: ha.stored_unique_id() is not None,
             "Home Assistant to write the config entry to its store", timeout=60.0)
    stored = json.loads((ha.config / ".storage" / "core.config_entries").read_text())
    written = next(e for e in stored["data"]["entries"] if e["domain"] == "zha")
    radio, path = written["data"]["radio_type"], written["data"]["device"]["path"]
    findings.append(
        f"the entry names the radio `{radio}` at `{path}`"
        if radio == "znp" and path == address else
        f"FAULT: the entry says radio `{radio}` at `{path}`, and we served `znp` at `{address}`")

    unique_id = written.get("unique_id")
    findings.append(
        f"its unique id is `{unique_id}` — the extended PAN id ZHA read off the radio through "
        f"tether, which is the one field here nothing in this harness could have told it"
        if unique_id and unique_id.startswith("epid=") else
        f"FAULT: the entry's unique id is `{unique_id}`, not the `epid=…` ZHA builds from a "
        f"network it has read")
    if expect_id is not None and unique_id != expect_id:
        findings.append(f"FAULT: the network ZHA read is `{unique_id}` and the coordinator's is "
                        f"`{expect_id}` — the two disagree, so something between them is not "
                        f"carrying the bytes it was given")
    return unique_id


def still_the_same_network(ha: HomeAssistant, token: str, before: str | None,
                           findings: list[str], what: str) -> None:
    """The claim cases (3), (4) and (5) all end on."""
    if not loaded(ha, token):
        entry = ha.zha_entry(token)
        findings.append(f"FAULT: {what}: the ZHA entry did not come back "
                        f"({entry['state'] if entry else 'no entry at all'}"
                        f"{', ' + str(entry.get('reason')) if entry and entry.get('reason') else ''})")
        return
    now = ha.stored_unique_id()
    findings.append(
        f"{what}: the entry loaded again on the same network, `{now}`"
        if now == before else
        f"FAULT: {what}: the entry came back on `{now}` and was on `{before}`")


# ─── The migration, the ZHA half: the database that has to survive a Reconfigure ─────────

# Two devices, written into `zigbee.db` by hand, for the reason the Z2M arm writes two into
# `database.db`: the emulator is the coordinator and only the coordinator, and a device whose
# every answer we wrote would test our reply table rather than the migration. What is
# under test is that a populated database survives a change of radio path, and a row is what a
# populated database is made of.
#
# The column values are zigpy's own, read off a real coordinator row rather than guessed:
# `ieee` is colon-separated lowercase, `status` is the enum, and a node descriptor is required
# for ZHA to treat the device as interviewed rather than re-interviewing it on every start.
SEEDED_DEVICES = (
    # ieee, nwk, logical type, endpoint profile, endpoint device type
    ("00:12:4b:00:01:02:03:04", 0x1234, LOGICAL_ROUTER, 260, 256),
    ("00:12:4b:00:05:06:07:08", 0x5678, LOGICAL_END_DEVICE, 260, 770),
)


def seed_devices(config: pathlib.Path) -> list[str]:
    """Put the two devices into ZHA's database, with Home Assistant stopped.

    ⚠️ **Stopped is not optional.** zigpy holds `zigbee.db` open and writes through its own
    session; rows inserted underneath a running ZHA are as likely to be overwritten as read.
    """
    db = config / "zigbee.db"
    con = sqlite3.connect(db)
    try:
        for ieee, nwk, logical, profile, device_type in SEEDED_DEVICES:
            con.execute("insert or replace into devices_v15 (ieee, nwk, status, last_seen) "
                        "values (?, ?, ?, ?)", (ieee, nwk, DEVICE_ENDPOINTS_INIT, time.time()))
            con.execute("insert or replace into endpoints_v15 "
                        "(ieee, endpoint_id, profile_id, device_type, status) values (?,?,?,?,?)",
                        (ieee, 1, profile, device_type, ENDPOINT_ZDO_INIT))
            # The shape of a real one, with the logical type changed. Without a node descriptor
            # ZHA has an uninterviewed device and goes looking for it on the air.
            con.execute(
                "insert or replace into node_descriptors_v15 (ieee, logical_type, "
                "complex_descriptor_available, user_descriptor_available, reserved, aps_flags, "
                "frequency_band, mac_capability_flags, manufacturer_code, maximum_buffer_size, "
                "maximum_incoming_transfer_size, server_mask, maximum_outgoing_transfer_size, "
                "descriptor_capability_field) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (ieee, logical, 0, 0, 0, 0, 8, 143, 4476, 80, 160, 11265, 160, 0))
        con.commit()
    finally:
        con.close()
    return [ieee for ieee, *_ in SEEDED_DEVICES]


def devices_in(config: pathlib.Path) -> dict[str, tuple]:
    """Every non-coordinator device in `zigbee.db`, with its endpoints, by IEEE address.

    `last_seen` is left out on purpose: ZHA writes it whenever it hears from a device or simply
    at shutdown, so comparing it would make the assertion fail for the passage of time.
    """
    con = sqlite3.connect(f"file:{config / 'zigbee.db'}?mode=ro", uri=True)
    try:
        rows = {}
        for ieee, nwk, status in con.execute(
                "select ieee, nwk, status from devices_v15 where nwk != 0"):
            endpoints = tuple(con.execute(
                "select endpoint_id, profile_id, device_type, status from endpoints_v15 "
                "where ieee = ? order by endpoint_id", (ieee,)))
            descriptor = con.execute(
                "select logical_type, manufacturer_code from node_descriptors_v15 where ieee = ?",
                (ieee,)).fetchone()
            rows[ieee] = (nwk, status, endpoints, descriptor)
        return rows
    finally:
        con.close()


def database_intact(before: dict, config: pathlib.Path, findings: list[str], what: str) -> bool:
    """The assertion the ZHA half exists for, made the same way every time it is made."""
    after = devices_in(config)
    if after == before:
        findings.append(f"{what}: `zigbee.db` still carries both devices, unchanged — "
                        f"{', '.join(sorted(before))}")
        return True
    gone = sorted(set(before) - set(after))
    arrived = sorted(set(after) - set(before))
    changed = sorted(k for k in set(before) & set(after) if before[k] != after[k])
    findings.append(
        f"FAULT: {what}: the device database did not survive"
        + (f" — lost {', '.join(gone)}" if gone else "")
        + (f" — gained {', '.join(arrived)}" if arrived else "")
        + ("".join(f" — rewrote {k}: {before[k]} → {after[k]}" for k in changed)))
    return False


def zha_devices_known_to(ha: HomeAssistant, token: str, entry_id: str) -> set[str]:
    """The devices Home Assistant's own registry has against the ZHA entry.

    The file is one claim and the running integration is another: a row ZHA never loaded is a row
    that did not survive in any sense a user would recognise. Only the websocket API lists the
    device registry, which is why this harness already carries a websocket client.
    """
    socket_ = WebSocket("127.0.0.1", ha.port)
    try:
        devices = socket_.command("config/device_registry/list", token)
    finally:
        socket_.close()
    found = set()
    for device in devices:
        if entry_id not in (device.get("config_entries") or []):
            continue
        for kind, value in device.get("connections", []) + device.get("identifiers", []):
            found.add(str(value).lower())
    return found

def run(tether_binary: pathlib.Path, args: argparse.Namespace, workdir: pathlib.Path) -> int:
    findings: list[str] = []
    config = workdir / "ha"
    config.mkdir()
    port, ha_port = free_port(), free_port()
    link = workdir / "coordinator"

    tether = coordinator = None
    runs: list[HomeAssistant] = []
    try:
        # ─── the coordinator, and tether in front of it ──────────────────────────────────────
        print("── the coordinator ──", flush=True)
        # What tether is told to open, which is the one line that changes when tether is the
        # thing on the far machine: over the seam the emulator reaches it down a serial line the
        # hypervisor bridges to the guest's COM port, so the name is that port's and not a path
        # on this filesystem (rig.py). Getting this wrong is not subtle — tether sits there
        # retrying a Linux path Windows cannot open — but it is invisible until something runs
        # it over there, which is why this comment is longer than the line.
        settings = {"device": rig.DEVICE if rig.WINDOWS else (args.stick or str(link)),
                    "listen": rig.listen(port),
                    # On only for the case that browses for it, which is also what keeps the
                    # advert inside the namespace this scenario re-execed into.
                    "advertise": args.discover}
        if args.stick:
            # No `radio` key: a dongle carries USB descriptors, so the family table names it.
            print(f"  a real one, on {args.stick}", flush=True)
            if args.unplug:
                coordinator = Coordinator(None, link, workdir, "zigpy", stick=args.stick,
                                          unplug=args.unplug, replug=args.replug)
        else:
            coordinator = Coordinator(args.device, link, workdir, "zigpy")
            print(f"  {args.device} is on {link}", flush=True)
            # A pty carries no USB identity, so the emulator's family has to be named.
            settings["radio"] = "znp"

        tether = rig.Tether(tether_binary, settings, workdir)
        tether.serving()
        address = f"socket://{tether.endpoint[0]}:{tether.endpoint[1]}"
        print(f"  {rig.where()}, serving {address}", flush=True)

        # ─── Home Assistant, on a config directory that did not exist ────────────────────────
        print("\n── Home Assistant ──", flush=True)
        (config / "configuration.yaml").write_text(ha_config(ha_port, args.discover))
        started = time.monotonic()
        ha = HomeAssistant(config, workdir, "hass-first", ha_port)
        runs.append(ha)
        ha.serving()
        token = ha.onboard()
        print(f"  up and onboarded in {time.monotonic() - started:.0f}s, on {ha.base}", flush=True)

        # ─── (1) or (2): how ZHA is told where the radio is ──────────────────────────────────
        expect_id = None if args.stick else EMULATOR_UNIQUE_ID
        if args.discover:
            print("\n── the zeroconf card ──", flush=True)
            # Read back off tether's own log rather than rebuilt here: what the card has
            # to match is the `serial_number` that actually went into the TXT record, and
            # that line prints the record it published.
            advertised = tether.output().partition("serial_number:")[2].partition("]")[0]
            flow = adopt_the_card(ha, token, advertised, findings)
            what = "the discovered card"
        else:
            print("\n── the manual path: socket://, typed in by hand ──", flush=True)
            flow = configure_by_hand(ha, token, address, findings)
            what = "the manual path"
        if flow is None:
            raise RuntimeError("there is no flow to finish; the fault above says why")
        unique_id = check_entry(ha, token, flow, address, expect_id, findings, what)
        if unique_id is None:
            raise RuntimeError("ZHA never took the radio; everything after this would be a "
                               "question about an integration that is not there")

        # ─── (3) the restart ─────────────────────────────────────────────────────────────────
        print("\n── homeassistant/restart, and this harness is the supervisor ──", flush=True)
        opened = tether.output().count("open at ")
        try:
            ha.api("/api/services/homeassistant/restart", {}, token=token)
        except (OSError, urllib.error.HTTPError):
            # Expected as often as not: HA stops serving while the answer is in flight, so the
            # call is a request rather than a transaction. Whether it took is the next line's
            # question, not this one's.
            pass
        wait_for(ha.died, "Home Assistant to exit for its supervisor", timeout=180.0)
        code = ha.proc.returncode
        findings.append(
            f"a restart asked for over the API left `hass` exited {code} for whoever started it, "
            f"which is the contract systemd and the supervisor both rely on"
            if code == RESTART_EXIT_CODE else
            f"FAULT: `hass` exited {code} on a requested restart, and its own --help says {RESTART_EXIT_CODE}")
        ha.stop()
        ha = HomeAssistant(config, workdir, "hass-restarted", ha_port)
        runs.append(ha)
        ha.serving()
        token = ha.sign_in()
        still_the_same_network(ha, token, unique_id, findings, "after a restart")

        # ─── (4) the client is killed ────────────────────────────────────────────────────────
        print("\n── SIGKILL, and a fresh Home Assistant arrives ──", flush=True)
        ha.kill()
        findings.append("the attached Home Assistant was killed outright, with no chance to "
                        "close its socket or tell the radio anything")
        ha.stop()
        ha = HomeAssistant(config, workdir, "hass-after-kill", ha_port)
        runs.append(ha)
        ha.serving()
        token = ha.sign_in()
        still_the_same_network(ha, token, unique_id, findings, "after a SIGKILL")

        said = tether.output()
        findings.append(
            "the device was opened once and never reopened across the restart and the kill: two "
            "whole client lifecycles never reached the radio (INV 1)"
            if said.count("open at ") == opened else
            f"FAULT: the device was opened {said.count('open at ')} times, having been opened "
            f"{opened} before the restart — a client lifecycle reached the radio, which is INV 1 "
            f"broken")

        # ─── (5) the radio is pulled out from under it ───────────────────────────────────────
        if rig.WINDOWS:
            # Measured, and it fails the way
            # `unplug_scenario.py` predicts rather than the way a bug does: the emulator's
            # unplug is its end of the wire closing, and over the seam that wire is a socket the
            # *hypervisor* holds — qemu keeps the guest's COM1 whether or not anything is on the
            # other end of the chardev. So tether's device never goes, the listener never
            # withdraws, and what the run measures is the rig rather than the product. Over
            # there the plug event has to be a real one, hot-removed by the hypervisor, which is
            # `unplug_scenario.py --device`'s ground.
            findings.append(
                "the unplug case did not run: tether is on the far machine, where the "
                "emulator's plug event — its end of the wire closing — leaves the guest's COM "
                "port standing. A real removal over there is unplug_scenario.py --device")
        elif coordinator is None:
            findings.append(
                "the unplug case did not run: this run's coordinator is a real dongle and no "
                "--unplug/--replug was given, so nothing here can stage a plug event")
        else:
            print("\n── the radio goes while ZHA holds it ──", flush=True)
            coordinator.unplug()
            wait_for(lambda: refused(tether.endpoint), "the listener to go", timeout=15.0)
            findings.append("tether withdrew its listener while the device was absent "
                            "(INV 2 backwards)")
            # ZHA's own reaction, and the whole point of the case: zigpy raises
            # `connection_lost`, ZHA turns it into a `ConnectionLostEvent`, and Home Assistant
            # reloads the config entry — which cannot succeed while the radio is away, so the
            # entry has to *leave* `loaded` before coming back means anything.
            try:
                wait_for(lambda: (entry := ha.zha_entry(token)) is not None
                         and entry["state"] != "loaded",
                         "ZHA to notice the radio had gone", timeout=180.0)
                entry = ha.zha_entry(token)
                findings.append(
                    f"ZHA noticed the radio go and put the entry in `{entry['state']}` — the "
                    f"reload INV 3 assumes, happening")
            except TimeoutError:
                findings.append("FAULT: ZHA never noticed the radio had gone; the entry stayed "
                                "`loaded` with nothing on the other end of it")

            print("  plugging it back in — same coordinator, new wire, same name", flush=True)
            coordinator.plug_in()
            wait_for(lambda: not refused(tether.endpoint),
                     "tether to reopen the device and serve", timeout=60.0)
            findings.append("tether reopened the stable name on its own and served again")
            # Nothing restarts Home Assistant here, and that is the measurement: a client that
            # needs a hand after an unplug is a client whose users are down until somebody comes
            # home. HA retries a failed entry on its own schedule, so this waits on *its* clock.
            still_the_same_network(ha, token, unique_id, findings,
                                   "after the radio came back, with nothing restarted")
    except (TimeoutError, RuntimeError, OSError, urllib.error.HTTPError) as exc:
        findings.append(f"FAULT: {exc or 'a step failed with no message'}")
    finally:
        for ha_ in reversed(runs):
            ha_.stop()
        if tether is not None:
            tether.stop()
        if coordinator is not None:
            coordinator.stop()

    said = tether.output() if tether is not None else "(no tether was started)"
    return report(findings, said, runs, config)


def fell_over(ha: HomeAssistant, token: str, timeout: float = 180.0) -> str | None:
    """Wait for the ZHA entry to stop being `loaded`, and say what it became.

    The mirror of `loaded`, and case (d) needs it: an address that has gone is measured by the
    entry failing, not by anything succeeding.
    """
    deadline = time.monotonic() + timeout
    state = None
    while time.monotonic() < deadline:
        entry = ha.zha_entry(token)
        state = entry and entry.get("state")
        if state != "loaded":
            return state
        time.sleep(1.0)
    return None


def served_at(port: int) -> str:
    """The `socket://` a client uses for a tether this run started on `port`.

    Not `127.0.0.1` any more, and that is the veth segment showing through: inside the private
    namespace tether binds every interface and announces `wire0`'s address, so loopback is one of
    the places it can be reached and not the one a discovered client picks. `rig.reachable_host`
    is the single answer to "where is it", and these assertions ask it rather than assuming.
    """
    return f"socket://{rig.reachable_host(None)}:{port}"


def settled(ha: HomeAssistant, token: str, timeout: float = 180.0) -> str | None:
    """Wait until a failed entry has stopped moving, and say where it stopped.

    ⚠️ **Not politeness — ZHA's options flow crashes on an entry that is still tearing down.**
    `async_step_init` asks `get_zha_gateway`, which raises only when the gateway *proxy* is gone;
    in the window after the radio dies but before the proxy is cleared it returns a gateway whose
    `application_controller` is `None`, and the next line reaches for `.backups` on it. The REST
    call answers 500 (`AttributeError: 'NoneType' object has no attribute 'backups'`,
    measured). That window is exactly when a user whose coordinator moved would open
    Configure → Reconfigure, which is why it is worth reporting rather than merely waiting out.
    """
    deadline = time.monotonic() + timeout
    state = None
    while time.monotonic() < deadline:
        entry = ha.zha_entry(token)
        state = entry and entry.get("state")
        if state in ("setup_retry", "setup_error", "not_loaded", "failed_unload"):
            return state
        time.sleep(2.0)
    return state


def migrate_by_reconfigure(ha: HomeAssistant, token: str, tether_binary: pathlib.Path,
                           link: pathlib.Path, port: int, workdir: pathlib.Path,
                           entry_id: str, findings: list[str],
                           tether: "rig.Tether | None" = None) -> tuple[dict, "rig.Tether"]:
    """**Configure → Reconfigure → Advanced → keep the radio's network settings.**

    The route for a coordinator that is not going anywhere — a dongle already on the machine
    tether will run on, or one reached over a network by something else. Its first step is what
    frees the port: *"A backup will be performed and ZHA will be stopped."*

    ⚠️ **That step also has a trap that costs two minutes to find.** It runs
    `create_backup(load_devices=True)`, and `zigpy.backups` has no lock while `_backup_loop`
    runs one of its own from startup. Open this flow within a few tens of seconds of ZHA coming
    up and the two cross: the POST hangs, and the log shows a failed backup and then a watchdog
    failure, which is the link gone rather than one request lost. Let the entry settle and the
    same POST returns in a second (measured). This arm settles it twice over, because
    the control run sits in between.
    """
    print("\n── the options flow ──", flush=True)
    options = "/api/config/config_entries/options/flow"
    step = ha.api(options, {"handler": entry_id}, token=token)
    if step.get("step_id") != "init":
        findings.append(f"FAULT: the options flow opened on `{step.get('step_id')}`, not the "
                        f"`init` step this harness knows how to answer")
        raise RuntimeError("the options flow is not the one this harness knows")
    step = ha.api(f"{options}/{step['flow_id']}", {}, token=token)
    print(f"  init → {step.get('step_id')} {step.get('menu_options') or ''}", flush=True)

    # The entry is unloaded from here on, so the radio is free and tether can have it — unless
    # the caller already has one, which case (d) does: there the address moved while tether kept
    # the radio, so nothing has to change hands.
    if tether is None:
        tether = serve_it(tether_binary, link, port, workdir, advertise=False)

    if step.get("type") != "menu" or RECONFIGURE not in (step.get("menu_options") or []):
        findings.append(f"FAULT: the options flow did not offer `{RECONFIGURE}` "
                        f"({step.get('menu_options')}) — `intent_migrate` is the branch that "
                        f"resets the old radio, and this arm must not take it")
        raise RuntimeError("the options flow did not offer a reconfigure")
    findings.append(f"the options flow offered {step['menu_options']}, and `{RECONFIGURE}` is the "
                    f"one that never reaches `_async_reset_old_radio`")
    step = ha.api(f"{options}/{step['flow_id']}", {"next_step_id": RECONFIGURE}, token=token)

    if step.get("step_id") != "choose_serial_port":
        findings.append(f"FAULT: reconfigure went to `{step.get('step_id')}`, not the serial port "
                        f"question")
        raise RuntimeError("the reconfigure flow is not the one this harness knows")
    address = served_at(port)
    print(f"  choose_serial_port ← {address}", flush=True)
    try:
        step = ha.api(f"{options}/{step['flow_id']}", {"path": address}, token=token)
        return walk_to_reuse(ha, token, step, findings, endpoint=options), tether
    except BaseException:
        # The same handover rule as `migrate_by_card`: this tether leaves with the return or
        # dies here, never both and never neither.
        tether.stop()
        raise


def migrate_by_card(ha: HomeAssistant, token: str, coordinator: Coordinator,
                    tether_binary: pathlib.Path, link: pathlib.Path, port: int,
                    workdir: pathlib.Path, findings: list[str],
                    radio: str = "znp") -> tuple[dict, "rig.Tether"]:
    """**The dongle moves machines, and Home Assistant offers the card by itself.**

    This is the ordinary route rather than the exotic one: in all practical cases the dongle
    has to be moved to a new machine, or tether is pointless. Which changes two things the
    Reconfigure route has to worry about and this one does not.

    **Nothing has to free the port**, because the user already did, with their hand. ZHA loses
    the radio the way INV 3 says it loses one, and the entry falls over on its own — no
    backup step, no "ZHA will be stopped" dialog, no ordering to get right.

    **And nobody types an address.** The card carries `radio_type` from our TXT record, so ZHA
    never probes — the thirty seconds the manual path spends autoprobing ezsp before znp is not
    spent here — and the port is the one tether actually advertised rather than one a user read
    off a screen and retyped.

    ⚠️ **The fork still matters, and it is the same fork.** From the card, *Migrate automatically*
    reaches `_async_reset_old_radio`, which opens the entry's old `/dev/tty…` to erase it. With
    the dongle physically gone that fails into an "Old adapter not found" prompt naming a path
    that no longer exists — harmless, but alarming, and it then restores a backup onto the radio
    for no reason. *Advanced migration* → *Keep adapter network settings* reaches neither.
    `walk_to_reuse` is what takes that branch, in this flow as in the other.
    """
    print("\n── the dongle leaves the machine ZHA was using ──", flush=True)
    coordinator.unplug()
    # ZHA is holding a radio that has gone. It does not have to have noticed yet — what matters
    # is that the port is free, which it is the moment the emulator's end of the pty closes.
    findings.append("the dongle was unplugged from the machine ZHA had it on, which is how a "
                    "migration actually starts and is also what frees the port")

    print("\n── and arrives on the machine tether runs on ──", flush=True)
    coordinator.plug_in()
    tether = serve_it(tether_binary, link, port, workdir, advertise=True, radio=radio)
    advertised = tether.output().partition("serial_number:")[2].partition("]")[0]

    print("\n── the card Home Assistant raises for it ──", flush=True)
    try:
        step = adopt_the_card(ha, token, advertised, findings)
    except BaseException as exc:
        # ⚠️ **A tether started inside this function has to leave with it or die with it.** Twice
        # now a run that raised after `serve_it` left a tether holding the pty, which the *next*
        # run then shared the serial line with — and the symptom is not a leak, it is the next
        # run failing somewhere unrelated (ZHA autoprobing a port it cannot get a straight answer
        # from). The caller's `finally` cannot help: it never received the object.
        if not isinstance(exc, urllib.error.HTTPError):
            tether.stop()
            raise
        # ⚠️ **A flow that raises is a result, not this harness's error.** ZHA lets
        # `HomeAssistantError: Failed to connect to Zigbee adapter` out of the step rather than
        # turning it into a form error, so the REST call answers 500 — which is what a wrong
        # `radio_type` looks like from outside (case (c), measured: bellows times out
        # resetting a Z-Stack coordinator). Catching it here keeps `tether` in the caller's hands
        # for the cleanup, and lets the assertion be about what the flow did.
        body = ""
        try:
            body = exc.read().decode(errors="replace")[:200]
        except Exception:  # noqa: BLE001 — the body is a nicety, not the finding
            pass
        step = {"type": "error", "reason": f"HTTP {exc.code} out of the flow step"
                                           f"{f': {body}' if body.strip() else ''}"}
        print(f"  the flow raised: {step['reason']}", flush=True)
    if step is None:
        raise RuntimeError("there is no card to adopt; the fault above says why")
    return step, tether


def serve_it(tether_binary: pathlib.Path, link: pathlib.Path, port: int, workdir: pathlib.Path,
             *, advertise: bool, radio: str = "znp", name: str = "tether") -> "rig.Tether":
    """One tether in front of the coordinator, which both routes want and word differently.

    A pty carries no USB identity, so the family has to be named — which is also the knob case (c)
    turns, by naming the wrong one.
    """
    # ⚠️ `name` is not decoration: `rig.Tether` writes `<name>.log` in the workdir and opens it
    # for truncation, so a second tether in one run silently erases the first one's log — which
    # is the evidence you want most when the second one is the thing that went wrong. Case (d)
    # starts two.
    tether = rig.Tether(tether_binary, {"device": str(link), "listen": rig.listen(port),
                                        "advertise": advertise, "radio": radio}, workdir,
                        name=name)
    tether.serving()
    print(f"  tether is serving it at {tether.endpoint}, claiming `{radio}`"
          f"{', and advertising' if advertise else ''}", flush=True)
    return tether


def run_migration(tether_binary: pathlib.Path, args: argparse.Namespace,
                  workdir: pathlib.Path) -> int:
    """The migration, case (a): a populated ZHA moved from a serial path onto tether.

    The Z2M arm's shape, in the client that has no config file to edit. What a Z2M user does by
    changing one line, a ZHA user does through **Configure → Reconfigure → Advanced → keep the
    radio's network settings**, and all three parts of that matter: the *recommended*
    branch of the same menu restores a backup, and in the config flow resets the old radio first.

      1. the coordinator on a pty with **nothing in front of it** — a ZHA that predates tether;
      2. ZHA configured against that pty, by hand, and `loaded`;
      3. two devices written into `zigbee.db`, with Home Assistant stopped;
      4. Home Assistant started again, **still on the pty** — the control, for the reason the Z2M
         arm has one: a row lost here is ZHA forgetting on any restart, not on this one;
      5. **the migration**, by one of two routes — `--discover` stages the one a user actually
         performs (the dongle is unplugged, arrives on the machine tether runs on, and Home
         Assistant offers a card), and without it the Reconfigure route for a coordinator that
         is not moving. `migrate_by_card` and `migrate_by_reconfigure` carry the difference;
      6. the same entry, the same `epid=` unique id, `loaded`, and both devices still there.

    ⚠️ **Step 4 is also what makes the Reconfigure route reliable.** ZHA's options flow opens
    with `create_backup(load_devices=True)`, and `zigpy.backups` has no lock
    while `_backup_loop` runs one of its own from startup. Open the flow too soon after ZHA comes
    up and the two cross: the POST hangs for two minutes, and the log shows a failed backup and
    then a watchdog failure, which is the link gone rather than one request lost. Waiting for the
    entry to settle — which this arm does anyway, twice — is what stops it. Measured.

    ⚠️ **The pty takes one holder at a time.** On the Reconfigure route ZHA has it until the
    flow's first step unloads the entry, and tether takes it after; on the card route the unplug
    is what hands it over, which is the same thing a user's hand does. Starting tether earlier
    puts two readers on one serial line and the emulator's answers go to whichever reads first.
    """
    findings: list[str] = []
    config = workdir / "ha"
    config.mkdir()
    port, ha_port = free_port(), free_port()
    link = workdir / "coordinator"

    tether = coordinator = None
    runs: list[HomeAssistant] = []
    try:
        # ─── the coordinator, and no tether anywhere ─────────────────────────────────────
        print("── the coordinator, on a serial port and nothing else ──", flush=True)
        coordinator = Coordinator(args.device, link, workdir, "zigpy")
        print(f"  {args.device} is on {link}", flush=True)

        # ─── ZHA as it was before anybody had heard of tether ────────────────────────────
        print("\n── Home Assistant ──", flush=True)
        (config / "configuration.yaml").write_text(ha_config(ha_port, args.discover))
        ha = HomeAssistant(config, workdir, "hass-on-usb", ha_port)
        runs.append(ha)
        ha.serving()
        token = ha.onboard()
        print(f"  up and onboarded, on {ha.base}", flush=True)

        print("\n── ZHA, pointed straight at the serial port ──", flush=True)
        flow = configure_by_hand(ha, token, str(link), findings)
        entry_unique_id = check_entry(ha, token, flow, str(link), EMULATOR_UNIQUE_ID, findings,
                                      "the install before tether")
        if entry_unique_id is None:
            raise RuntimeError("ZHA never took the radio; everything after this would be a "
                               "question about an integration that is not there")
        entry = ha.zha_entry(token)
        entry_id = entry["entry_id"]
        print(f"  entry {entry_id}, unique id {entry_unique_id}", flush=True)

        # ─── a populated install, which the emulator cannot populate for us ──────────────
        print("\n── two devices, into zigbee.db by hand ──", flush=True)
        ha.stop()
        seeded = seed_devices(config)
        before = devices_in(config)
        print(f"  {', '.join(seeded)}", flush=True)

        # ─── the control: the same install, restarted, still on the serial port ──────────
        print("\n── Home Assistant again, still on the serial port — the control ──", flush=True)
        ha = HomeAssistant(config, workdir, "hass-control", ha_port)
        runs.append(ha)
        ha.serving()
        token = ha.sign_in()
        if not loaded(ha, token):
            findings.append("FAULT: the control: the ZHA entry did not come back on the serial "
                            "port, so nothing after this could say anything about tether")
            raise RuntimeError("the control run never loaded ZHA")
        findings.append("the control: ZHA came back on the serial port it was configured with")
        known = zha_devices_known_to(ha, token, entry_id)
        missing = [ieee for ieee in seeded if ieee not in known]
        findings.append(
            "the control: Home Assistant's device registry carries both seeded devices, so ZHA "
            "read them rather than merely leaving the rows alone"
            if not missing else
            f"FAULT: the control: ZHA did not load {', '.join(missing)} — a row it never reads is "
            f"not a populated install, and the migration below would prove nothing")
        if not database_intact(before, config, findings, "the control"):
            raise RuntimeError("the seeded devices did not survive a plain restart on the serial "
                               "port, so nothing after this could have told you about tether")

        # ─── the migration, by whichever route this run is about ────────────────────────
        if args.discover:
            step, tether = migrate_by_card(ha, token, coordinator, tether_binary, link, port,
                                           workdir, findings,
                                           radio=WRONG_RADIO if args.wrong_radio else "znp")
        else:
            step, tether = migrate_by_reconfigure(ha, token, tether_binary, link, port, workdir,
                                                  entry_id, findings)

        if args.wrong_radio:
            # Case (c). The refusal *is* the pass, and the entry surviving it is the claim: a
            # family-table mistake must stop ZHA before it writes to a database it cannot read.
            landed = step.get("reason") or step.get("errors") or step.get("step_id")
            findings.append(
                f"a wrong `radio_type` stopped the flow rather than repointing the entry: it "
                f"ended `{step.get('type')}` ({landed})"
                if step.get("reason") != "reconfigure_successful" else
                "FAULT: tether advertised `ezsp` in front of a Z-Stack coordinator and ZHA "
                "repointed the entry at it anyway — a family-table mistake a client does not "
                "notice is one that reaches the radio")
            stored = json.loads((ha.config / ".storage" / "core.config_entries").read_text())
            written = next(e for e in stored["data"]["entries"] if e["domain"] == "zha")
            path = written["data"]["device"]["path"]
            findings.append(
                f"the entry still names the radio it had, `{path}`"
                if path == str(link) else
                f"FAULT: the entry was moved to `{path}` by a flow that did not finish")
            database_intact(before, config, findings, "the wrongly-typed flow")
            return report(findings, tether.output() if tether else "(no tether)", runs, config)

        if step.get("type") == "abort" and step.get("reason") == "reconfigure_successful":
            findings.append("the migration finished on `reconfigure_successful`, keeping the "
                            "radio's own network: no reset of the old radio, and no backup "
                            "restored onto it")
        else:
            findings.append(f"FAULT: the migration ended `{step.get('type')}` "
                            f"({step.get('reason') or step.get('step_id')}), not an abort on "
                            f"`reconfigure_successful`")

        # ─── and the same install comes up through it ────────────────────────────────────
        print("\n── what ZHA has afterwards ──", flush=True)
        if not loaded(ha, token):
            findings.append("FAULT: the migrated install: the ZHA entry never came back")
        else:
            after = ha.zha_entry(token)
            findings.append(
                f"the migrated install: the same config entry ({entry_id}) came back `loaded`"
                if after and after["entry_id"] == entry_id else
                f"FAULT: the migrated install: the entry is {after and after.get('entry_id')}, "
                f"not {entry_id} — a migration that replaces the entry loses everything keyed on "
                f"it")
            still_the_same_network(ha, token, entry_unique_id, findings, "the migrated install")
            print(f"  unique id {ha.stored_unique_id()}", flush=True)
            # ⚠️ **Where the entry points.** The unique id and
            # the device rows both survive a migration onto the *wrong radio* — `unique_id` is
            # not rewritten by `async_update_reload_and_abort`, and the database is ZHA's rather
            # than the coordinator's — so without this the arm would pass while pointing a
            # populated ZHA at a stranger's dongle.
            stored = json.loads((ha.config / ".storage" / "core.config_entries").read_text())
            written = next(e for e in stored["data"]["entries"] if e["domain"] == "zha")
            path = written["data"]["device"]["path"]
            findings.append(
                f"the migrated install: the entry now names `{path}`, which is the tether this "
                f"run started"
                if path == served_at(port) else
                f"FAULT: the migrated install: the entry names `{path}` and this run's tether is "
                f"at `{served_at(port)}` — the migration landed on a different radio")
            database_intact(before, config, findings, "the migrated install")
            known = zha_devices_known_to(ha, token, entry_id)
            missing = [ieee for ieee in seeded if ieee not in known]
            findings.append(
                "the migrated install: both devices are still in Home Assistant's device "
                "registry, so they are the running ZHA's and not only the file's"
                if not missing else
                f"FAULT: the migrated ZHA does not know {', '.join(missing)}, whatever the "
                f"database says")
            if args.ip_change:
                # ─── (d) the address goes away under a configured ZHA ────────────────────
                print("\n── the address changes under it ──", flush=True)
                gone = served_at(port)
                tether.stop()
                state = fell_over(ha, token)
                findings.append(
                    f"with its address gone the entry went `{state}` — ZHA does not follow an "
                    f"address it was not told about, which is why the README asks for a "
                    f"reservation"
                    if state is not None else
                    "FAULT: the entry stayed `loaded` with nothing on the other end of its "
                    "address, so whatever it is talking to is not this run's tether")

                port = free_port()
                tether = serve_it(tether_binary, link, port, workdir, advertise=True,
                                  name="tether-moved")
                advertised = tether.output().partition("serial_number:")[2].partition("]")[0]
                print(f"  it is now at {port}, and {gone} answers nothing", flush=True)

                # ⚠️ **This block can only ask its question because the namespace has a real
                # segment in it.** A loopback-only one cannot: tether refuses to advertise on
                # loopback (`advertisable`), so what reaches Home Assistant goes through the
                # library's own fallback and arrives once — and "no card appeared" then says
                # nothing about Home Assistant. `into_a_private_network` builds a veth segment,
                # and the advert behaves the way it does on a real LAN.
                #
                # It is a *port* that moves rather than an address, and that is the same staleness
                # from ZHA's side: the entry stores `socket://host:port` whole, so either half
                # going out of date leaves it dialling something that is not there.
                print("\n── what Home Assistant offers for the new address ──", flush=True)
                appeared = None
                deadline = time.monotonic() + 180.0
                while time.monotonic() < deadline and appeared is None:
                    for card in ha.discovered_zha_flows(token):
                        if card["context"].get("source") == "zeroconf":
                            appeared = card
                            break
                    time.sleep(3.0)
                findings.append(
                    f"Home Assistant raised a fresh card for the moved coordinator "
                    f"(`{appeared['context'].get('unique_id')}`), so the discovery path does "
                    f"offer a recovery for an address that moved"
                    if appeared is not None else
                    "no card was raised for the moved address within 180 s, on a segment where "
                    "the first advert was seen — so the discovery path does **not** offer a "
                    "recovery here")

                # ─── and what adopting that card actually does ───────────────────────────
                # The realistic recovery, now that the card is known to appear. Reconfigure is
                # measured by this arm's own `--migrate` run, so doing it again here would cost
                # five minutes to learn nothing; what nothing has measured is the card's *own*
                # path for an address that moved, which is the question a user asks.
                print("\n── adopting the card for the moved address ──", flush=True)
                resting = settled(ha, token)
                findings.append(
                    f"the failed entry settled at `{resting}` before the card was adopted — "
                    f"taking it while ZHA is still tearing down answers 500 instead, which is "
                    f"upstream's rather than this harness's"
                    if resting is not None else
                    "FAULT: the failed entry never settled, so what follows was done against an "
                    "integration still in motion")
                step = adopt_the_card(ha, token, advertised, findings)
                if step is None:
                    raise RuntimeError("the card went away before it could be adopted")
                findings.append(
                    "the card for a moved address walks the **same menus as a migration** — ZHA "
                    "does not recognise it as the adapter it already has, so `Advanced` and "
                    "`Keep adapter network settings` are needed again"
                    if step.get("reason") == "reconfigure_successful" else
                    f"FAULT: adopting the card for the moved address ended `{step.get('type')}` "
                    f"({step.get('reason') or step.get('step_id')})")
                if loaded(ha, token):
                    stored = json.loads(
                        (ha.config / ".storage" / "core.config_entries").read_text())
                    written = next(e for e in stored["data"]["entries"] if e["domain"] == "zha")
                    now = written["data"]["device"]["path"]
                    findings.append(
                        f"the card recovered it in place: the entry names `{now}`, is `loaded`, "
                        f"and is the same entry ({entry_id})"
                        if now == served_at(port) and written["entry_id"] == entry_id else
                        f"FAULT: after the card the entry names `{now}` (id "
                        f"{written['entry_id']}), and tether moved to `{served_at(port)}`")
                    still_the_same_network(ha, token, entry_unique_id, findings,
                                           "the recovered install")
                    database_intact(before, config, findings, "the recovered install")
                else:
                    findings.append("FAULT: the entry never came back after the card was adopted")
    except (TimeoutError, RuntimeError, OSError, urllib.error.HTTPError) as exc:
        findings.append(f"FAULT: {exc or 'a step failed with no message'}")
    finally:
        for run_ in reversed(runs):
            run_.stop()
        if tether is not None:
            tether.stop()
        if coordinator is not None:
            coordinator.stop()

    said = tether.output() if tether is not None else "(the run never got as far as a tether)"
    return report(findings, said, runs, config)


def refused(endpoint: tuple[str, int]) -> bool:
    with socket.socket() as s:
        s.settimeout(0.2)
        return s.connect_ex(endpoint) != 0


def report(findings: list[str], tether_said: str, runs: list[HomeAssistant],
           config: pathlib.Path) -> int:
    print("\n--- what was measured ---", flush=True)
    for finding in findings:
        print(("  ✗ " if finding.startswith("FAULT") else "  · ") + finding)
    print("\n--- what tether said ---")
    print(tether_said)
    faults = [f for f in findings if f.startswith("FAULT")]
    if faults:
        for run_ in runs:
            # The tail only: a Home Assistant start is thousands of lines, and the ones that
            # matter are always the last ones before it stopped doing what was asked.
            print(f"\n--- the last of what {run_.name} said ---", file=sys.stderr)
            print("\n".join(run_.output().splitlines()[-80:]), file=sys.stderr)
        print(f"\nits config directory was {config}", file=sys.stderr)
        print(f"{len(faults)} fault(s)", file=sys.stderr)
        return 1
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--tether", type=pathlib.Path, default=pathlib.Path("./briard-tether"))
    parser.add_argument("--device", default="FormedLaunchpadCC26X2R1",
                        help="the emulator fixture; the default carries a network, which is "
                             "what every case here needs (see the module docstring)")
    parser.add_argument("--stick",
                        help="a real coordinator for this harness to put a tether in front of, "
                             "instead of the emulator: a /dev/serial/by-id/… path. ⚠️ It must "
                             "already carry a network, and this harness will not form one")
    parser.add_argument("--unplug",
                        help="with --stick, a shell command that removes the dongle — a USB "
                             "deauthorize, a hypervisor's monitor, a smart hub's port off, or "
                             "`read -p 'pull it, then Enter'`. Without it case (5) is skipped "
                             "and says so")
    parser.add_argument("--replug", help="with --unplug, the command that puts it back")
    parser.add_argument("--migrate", action="store_true",
                        help="case (a): stage a ZHA that predates tether — configured "
                             "against the serial port, with two devices written into zigbee.db — "
                             "then Configure → Reconfigure → Advanced → keep the radio's network "
                             "settings, and assert the entry, its network and its devices all "
                             "survived")
    parser.add_argument("--ip-change", action="store_true",
                        help="case (d): after the migration, move tether's address "
                             "out from under the configured entry and measure what Home "
                             "Assistant offers to recover it")
    parser.add_argument("--wrong-radio", action="store_true",
                        help="case (c): have tether advertise the wrong radio family "
                             "and assert ZHA refuses rather than writing to a database it cannot "
                             "read")
    parser.add_argument("--discover", action="store_true",
                        help="let Home Assistant find tether over mDNS instead of being told "
                             "where it is, which is case (2) and re-execs this into a private "
                             "network namespace")
    args = parser.parse_args()

    if bool(args.unplug) != bool(args.replug):
        parser.error("--unplug and --replug come as a pair: a case that pulls the dongle and "
                     "never puts it back measures half of what it claims")
    if args.unplug and not args.stick:
        parser.error("--unplug/--replug are for --stick: the emulator's plug event is a pty "
                     "closing, which this harness stages itself")
    if args.stick and args.device != parser.get_default("device"):
        parser.error("--stick and --device are two answers to the same question: one is a real "
                     "coordinator, the other picks an emulated one")
    if args.stick and rig.WINDOWS:
        parser.error("--stick names a coordinator on *this* machine, and over the seam the "
                     "radio hangs off the far one: point TETHER_WINDOWS_DEV at its COM port "
                     "instead (rig.py), which is the same knob the emulator's wire uses")
    if args.discover and rig.WINDOWS:
        parser.error("the discovered card needs Home Assistant to share a segment with tether, "
                     "and over the seam that needs a namespace with a leg on the far machine's "
                     "segment — run_gate.py has one for the herdsman client; this harness does "
                     "not yet")
    if args.ip_change and not (args.migrate and args.discover):
        parser.error("--ip-change needs --migrate --discover: it measures what the *card* offers "
                     "for an address that has moved, so there has to be a migrated entry first "
                     "and an advert for the new address after.")
    if args.ip_change and args.wrong_radio:
        parser.error("--ip-change and --wrong-radio are two different cases: one needs a "
                     "migration that succeeded and the other needs one that refused.")
    if args.wrong_radio and not (args.migrate and args.discover):
        parser.error("--wrong-radio needs --migrate --discover: the wrong family has to reach the "
                     "client, and on the zeroconf path the TXT record is what carries it. A path "
                     "typed by hand makes ZHA probe the radio instead, and it finds the truth.")
    if args.migrate and (args.stick or args.unplug):
        parser.error("--migrate stages a ZHA that predates tether and then puts one in front of "
                     "the same coordinator, which is the emulator's pty. --stick is a dongle this "
                     "harness only ever serves through tether, and --unplug belongs to case (5): "
                     "this arm stages its own plug event, because that is what a migration is.")
    if "HASS" not in os.environ:
        print("no $HASS — run this inside `nix develop .#zha`, not the default shell",
              file=sys.stderr)
        return 2

    if args.discover:
        # Before anything is started, because it re-execs this whole process.
        into_a_private_network()

    tether = rig.binary(args.tether)
    # Kept when something fails, and that is not laziness: the report names the config directory,
    # and a Home Assistant that would not start is answered by its own log and its own `.storage`
    # — both of which a temp dir cleaned up on the way out would have taken with it.
    workdir = pathlib.Path(tempfile.mkdtemp(prefix="tether-zha-"))
    code = (run_migration if args.migrate else run)(tether, args, workdir)
    if code == 0:
        shutil.rmtree(workdir, ignore_errors=True)
    else:
        print(f"the run's whole directory is kept at {workdir}", file=sys.stderr)
    return code


if __name__ == "__main__":
    sys.exit(main())
