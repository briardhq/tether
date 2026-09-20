# Architecture

How briard-tether is built and why: enough to judge the foundations, understand the guarantees and
find your way around the code. The code's own comments are the subsystem reference.

## What it is, and what it is not

**briard-tether brings a USB serial Zigbee coordinator to a ZHA or zigbee2mqtt client over the
network, reliably.** It runs on the machine the dongle is plugged into, serves it on a TCP port,
and advertises it over mDNS so the client finds it with no typing. The target is one coordinator
and one client, on a wired LAN or across a VM boundary. **Reliability is the product** — anyone can
write the byte pump.

Two goals are not built and must not be designed out: **client failover** (the client moves
between machines and reconnects to the same coordinator) and a **WiFi uplink** (the dongle sits
where the RF is best, on a link with bad tails). The last section says what today's code owes them.

**Out of scope, permanently:** a Zigbee stack, coordinator backup, an MQTT broker, a device UI, or
anything else above the serial bytes — that belongs to the client. Also not to be scaffolded for: a
resumable dual-shim transport, failover orchestration, firmware flashing, multi-radio or
multi-client fan-out, HTTP or a web UI, a database, auth, non-serial USB, Bluetooth, Thread.

## Why a forwarder and not a bridge

The shipping network coordinators — SLZB-06 stock firmware and the community XZG — are a polling
loop with a **256-byte buffer that silently drops bytes on overflow**, up to five clients with
**no arbitration**, and no keepalive or flow control. They mostly work, because ZNP and EZSP/ASH
carry their own checksums and retransmission, and because in practice one client connects.

**So the data path is not where the risk is.** The risk is the seams users assemble by hand. Four
failure classes sink ser2net setups, and each gets one owned mechanism:

| Failure class | What closes it here |
| --- | --- |
| **1. Config assembly** — baud, flow control, telnet-vs-raw, `ttyUSB0` renumbering, adapter type | the family table (which also *finds* the adapter), by-id paths, and the TXT record as the single source of truth |
| **2. Lifecycle** — stale connection locks, boot-order races, silent stalls | one client with **kick-old takeover**, open-on-demand with retry, keepalive, **fail-loud close** |
| **3. Load-dependent data path** — flow-control mismatch, USB autosuspend | per-family flow control, `TCP_NODELAY`, autosuspend disabled, a back-pressured pipe |
| **4. Beneath the bridge** — firmware, RF, USB itself | not fixable, but **exonerable**: a liveness probe and link metrics that place the fault away from the transport |

The honest residual: an SLZB's radio UART is soldered chip-to-chip, while ours crosses USB. That
is mitigable, not eliminable — the difference is that it fails in our userspace with a metric,
rather than in the kernel with none.

## The shape

One binary, five parts, no plugin system and no transport abstraction.

```
   ┌─────────────────────────────────────────────────────────────┐
   │ the machine the dongle is plugged into                      │
   │                                                             │
   │   ┌──────────┐   ┌────────┐   ┌────────┐                    │
   │   │  device  │──▶│  pipe  │◀──│ server │◀── one client, TCP │
   │   │  (tty)   │◀──│        │──▶│ :6638  │                    │
   │   └────┬─────┘   └────────┘   └────────┘                    │
   │        │              ▲            ▲                        │
   │   ┌────▼─────┐   ┌────┴────────────┴────┐   ┌────────────┐  │
   │   │USB dongle│   │      management      │   │ discovery  │  │
   │   └──────────┘   │    probe · card      │   │ mDNS advert│  │
   │                  └──────────┬───────────┘   └─────┬──────┘  │
   └─────────────────────────────┼─────────────────────┼─────────┘
                                 ▼                     ▼
                        status socket (JSON)    your LAN — the client
                                                finds it by name
```

1. **device** — opens the port by a stable path, applies the family parameters, drains stale
   bytes, and notices the device vanishing. Owns every platform-specific line in the program.
2. **server** — one TCP listener, **one client at a time**, `TCP_NODELAY`, a keepalive matching
   the clients' 15 s, and takeover of a stale connection by a new one.
3. **pipe** — the copy between the two: **back-pressured, never lossy, byte-transparent**.
4. **discovery** — advertises `_zigbee-coordinator._tcp` with the TXT contract below.
5. **management** — what does not ride the byte stream: the liveness probe and the **report
   card**, the one place that reaches across components to answer "how is this going". The card is
   JSON on a unix socket, rendered by `briard-tether status`; fields may be added, never
   repurposed or removed.

The boundaries are ordinary Go packages, not interfaces.

**`main` sequences them and is not a sixth part.** It identifies the adapter and runs the **device
generation loop**: open the device, serve it, reopen it when it goes. A *generation* is one open
port's lifetime, and **the listener and the advert live and die with it**. Everything above — the
counters, the card, the status socket, the advertised identity — belongs to the process, so an
unplug and replug leave the same tether with the same numbers.

## Invariants

The guarantees. The code cites them by number; ★ marks the test-enforced ones.

1. ★ **The radio is never touched by client connect, disconnect, or takeover.** No reset, no
   DTR/RTS toggle, no line-state change as a side effect of the network. The coordinator keeps its
   network state across client restarts, which is why client recovery works at all.
2. ★ **Accept-then-silence.** The port is open, configured and drained *before* the listener
   accepts, so a client's first byte cannot race port setup. The converse is the reopen rule:
   **while there is no port there is no listener**, so a client arriving during an unplug is
   refused rather than accepted into silence. This is structural: `pipe.Serve` binds the device
   before it hands back what the generation waits on.
3. ★ **Fail loud, never stall.** On device error, disappearance or shutdown, close the TCP
   connection at once (within 50 ms), so the next client request *fails* rather than hangs. A
   silent stall is the worst input to a client's timers; a close triggers the recovery paths the
   clients already test. **A client cannot tell our close from a real unplug** — same errno, same
   line. Neither client reconnects by design (ZHA reloads the entry, zigbee2mqtt exits for its
   supervisor), so tether's job ends at closing loudly and reopening on its own.
4. ★ **Exactly one client.** A new connection takes over and the old one is closed. Never
   broadcast, never arbitrate two writers into one UART, and never weaken this into "refuse while
   busy". Refusing fails silently — a half-dead client keeps its socket and locks out the machine
   that is actually trying to work (failure class 2). Kick-old fails loudly: two clients fighting
   over one coordinator displace each other and log every round. tether cannot know which client
   deserves the radio, so it makes the fight visible instead — takeovers are counted apart from
   disconnects, and the card names the contending **hosts** (not addresses, which change on every
   reconnect). A successor inherits nothing: the pipe holds no buffer.
5. ★ **No lossy buffer.** Back-pressure the reader; never drop or overwrite a byte between a device
   and a client. **With no client attached there is no path**: the device is read and discarded, so
   a new client is never handed the tail of someone else's session, and the tty buffer never
   back-pressures the radio while nobody listens.
6. ★ **Byte-transparent.** Raw TCP, never telnet mode — `0xFF` is telnet's IAC and occurs inside
   ZNP and ASH frames. Nothing in the data path inspects, rewrites, or reframes.
7. **DTR and RTS are pinned, not toggled** — except RTS on the adapter rows that use hardware flow
   control, where the kernel drives it exactly as those adapters' own clients do. Otherwise the
   lines move only on an explicit verb. "Never asserted on open" is impossible on Linux (the tty
   layer raises both in `open()`), so the guarantee is the achievable one: **one assert per plug
   event, none across tether's own lifecycle**, bought by clearing `HUPCL`. That edge lands just
   after the plug has power-cycled the radio anyway. Windows needs none of this: the DCB sets line
   state atomically in `CreateFile`.
8. **The TXT record is the truth.** What tether advertises about adapter type and baud is what it
   used.

## The family table

Per-family knowledge is data, not code paths: rows keyed by USB vendor and product id **and the
descriptor strings**, with a config override.

**The rows are translated from zigbee-herdsman's `adapterDiscovery.ts`** (MIT; see
[`NOTICE`](NOTICE)). zigbee2mqtt has the hardware and the users; we cannot buy every stick.
Translated rather than vendored, because their regexes match a by-id string that does not always
exist and one uses a lookahead Go's RE2 cannot express. **Where we cannot test a stick, their code
— matching behaviour as well as data — is the authority**, and a difference is our bug until
measured otherwise. Two families are left out: **zboss** (not a zigpy `RadioType`, so nothing legal
to advertise) and **zigate** (a fourth family is a decision, not a row).

A **drift check** reads their compiled table from nixpkgs' zigbee2mqtt on every commit and reports
what they carry and we do not. It reports rather than fails, because adopting an adapter needs a
human to pick its radio type and flow control.

| Family | Protocol | Baud | Flow | Notes |
| --- | --- | --- | --- | --- |
| TI CC2652 / CC2652P7 (Sonoff, LaunchPad) | ZNP (`znp` / `zstack`) | 115200 | none | first-class target |
| Silabs EFR32MG21 / MG24 | EZSP/ASH (`ezsp` / `ember`) | 115200 | none | ASH timing and the ember driver's firmware coupling need care; some firmware wants 57600 via override |
| dresden ConBee II/III | deCONZ | **38400** | none | |

⚠️ **A vendor and product id alone identifies nothing.** `10c4:ea60` is the stock CP210x bridge id,
worn by ZNP sticks, EZSP sticks and unrelated hardware alike. An adapter the table cannot resolve is
**refused by name**, never guessed at.

Six rows need hardware RTS/CTS, which the serial library cannot enable. The device layer sets
`CRTSCTS` itself through a descriptor opened *before* the library and held for the port's life
(termios belongs to the tty, and the library's `TIOCEXCL` cannot lock out an earlier open) — no
fork, no patch. Flow control is applied **before the drain**.

## Discovery

**Advertise `_zigbee-coordinator._tcp.local.`**, the vendor-neutral type. ZHA's manifest already
matches it, and zigbee2mqtt reaches it with `mdns://zigbee-coordinator`. ★ **Never advertise
`_slzb-06._tcp`**: it triggers Home Assistant's SMLIGHT integration (an API we do not implement),
forces product-name impersonation, and collides with real SLZB hardware.

**Nothing probes the radio on this path, which is what makes INV 8 load-bearing.** Over USB, ZHA
tries each radio library until one answers. Over zeroconf it takes `radio_type` **verbatim** and
goes straight to the confirm step — so whatever tether advertises is what ZHA becomes.

**The TXT contract**, read out of both consumers:

- **`radio_type` — required**, exactly a zigpy `RadioType` name (`znp`, `ezsp`, `deconz`, …).
  zigbee-herdsman maps it (`znp`→`zstack`, `ezsp`→`ember`) and reads nothing else. It is what a
  bare `tcp://host:port` cannot supply, which is why zigbee2mqtt then needs `adapter:` by hand.
- **`serial_number` — required by ZHA 2025.1+, and its absence fails silently** (the flow aborts
  with `invalid_zeroconf_data` and no card appears). It must be **unique on the LAN** and **stable
  per adapter per host**, because a dismissed card is remembered by it.
- **`baud_rate` and `data_flow_control` are read by nobody.** They stay, as the one place an
  operator can read back what tether opened the port at, but **no behaviour may rest on them**.

**The instance name is `<adapter> at <hostname> (tether)`**, and `serial_number` is the same
string — one identity. It appears only on ZHA's discovery card and confirm dialog, so it answers
the two questions that reader has: **which stick** and **which machine**. The adapter half is the
family table's name, else the USB manufacturer and product strings, else the last path element. A
derived name is shortened to fit one 63-byte DNS label; a **configured** name that does not fit is
refused rather than silently altered.

**The advertised address**, in order: `advertise_address`; else the `listen` address if it is a
specific one; else each interface announces its own, which is what follows a NIC coming up later.
**Never loopback** — off Linux, tether picks the interfaces itself to ensure it.

**A clash is warned about, not prevented.** Before announcing, tether browses for other
coordinators and logs them, then advertises anyway: refusing would make the innocent install
invisible, the very outcome the refusal meant to avoid. **Two advertising coordinators on one LAN
is unsupported** — zigbee2mqtt takes the first responder — and the fix is `advertise: false` on
one plus a direct `tcp://`.

**The advert lives exactly as long as the device.** It goes up after the port opens and is
withdrawn with a real goodbye the moment it closes, so an unplugged dongle does not linger in ZHA's
list for the record TTL.

**`socket://host:port` (ZHA) and `tcp://host:port` (zigbee2mqtt) must always work without
discovery.** That covers the case mDNS cannot: a container on a bridge network, which link-local
multicast never reaches.

## Protocol awareness, and the two places it is allowed

Every vendor who started with a dumb bridge ended up speaking the protocol inside it, so the places
tether may parse frames are listed exhaustively. Context: **no client polls the radio on a timer**,
so with nobody attached a wedged-but-enumerated dongle gives no signal at all.

**1. The liveness probe.** A ZNP `SYS ping`, **only while no client is connected**. ZNP only: the
ping is five bytes each way, whereas an EZSP query needs ASH first — a protocol stack, not a probe.
EZSP and deCONZ health is whatever the card sees from outside the bytes. **The probe and a client
are mutually exclusive**: a probe is refused while a client is attached, and a client arriving
mid-probe waits for it, briefly, so it is never handed an answer it did not ask for.

**2. The frame census.** A **passive count** of ZNP frames in both directions, which turns "the
transport is probably fine" into evidence. Its fence:

- **Passive** — it sees a copy after forwarding and cannot alter, delay or drop anything (INV 6).
- **Client path only** — the probe's own traffic is excluded; the radio talking to an empty room is
  counted.
- **Stateless per frame** — no request/response matching, no transaction ids, nothing decoded past
  one leading enum byte. Once it needs one frame to understand another, it is a protocol stack.
- **An exhaustive list** — frames each way; unsolicited vs. answers vs. requests (the *imbalance*
  is the diagnostic); bytes that did not parse; **`SYS ResetInd`** with its reason; **`RPCError
  CommandNotRecognized`** with its code. Adding to it is a decision.

`ResetInd` with reason `External` means something pulled the reset line — what INV 1 and INV 7
forbid, made falsifiable on every machine. `CommandNotRecognized` means bytes arrived corrupted,
which is what a telnet-mode ser2net causes.

⚠️ **Unframed bytes alone are not corruption.** A healthy session produces them: zigpy's 256-byte
`0xEF` skip-bootloader burst, and one `0x00` from the radio per power-up. A count climbing with no
connects or resets behind it is the signal.

**Refused: a drain window on takeover.** The pipe holds no buffer, so there is nowhere for a stale
reply to wait; a local reply arrives in milliseconds while a client spends about a second booting;
and a drain long enough to matter would swallow device reports that only per-frame state could
tell apart.

## Configuration

**One JSON file, read once at startup, never created.** `~/.config/tether/config.json`, then
`/etc/tether/config.json` — the first that exists. Root skips the user location, so a system
service cannot be shadowed by a dotfile. No file is the normal install; tether logs which file it
read, or that every default applies.

Seven keys. Six are *deployment wiring*; `advertise` is the one behavioural key, because the second
coordinator on a LAN needs a way to stay quiet.

| key | absent means | set it to |
| --- | --- | --- |
| `device` | **find it** | pick between two adapters, or a stick the table does not carry |
| `listen` | `:6638`, ZHA's legacy zeroconf port | move the port, or bind one interface |
| `radio` | read it off the adapter | an unknown stick, or one reflashed into another family (radio type only; a known stick keeps its row's baud and flow control) |
| `baud` | the family table's | a bench experiment |
| `instance` | derive it | your own name for the coordinator |
| `advertise` | **on** | off, on the second coordinator of a LAN |
| `advertise_address` | derive it (see Discovery) | a host behind NAT, a container, odd interfaces |

The rules:

- **An absent key means "derive it", never "off"** — so zero config writes no file, and a program
  driving tether writes only what it has an opinion about.
- **An unknown key is refused**, so a typo cannot leave tether quietly on the default.
- **Everything is validated at startup**, so a bad family name fails once instead of looking like a
  dongle not yet plugged in forever. The port must be a number, not a `/etc/services` name.
- **Keys may be added, never renamed or repurposed** — another program writes this file.

**Serving is a verb: `briard-tether run`.** A bare `briard-tether` prints help to stderr and exits
1, so starting a radio forwarder — the one action with a side effect on somebody's house — is never
an accident, and a unit that lost its verb fails visibly. The help names the config paths and
socket directories of the machine it runs on.

**The status socket is `status.<pid>.sock`**, in `/run/tether` for a system service or
`$XDG_RUNTIME_DIR/tether` for a user one. Two dongles mean two tethers, and the pid keeps them from
colliding; `briard-tether status -pid` picks one and, with several running, it lists them rather
than choosing. Readers search both locations and skip — never delete — a socket nothing answers on.
The socket is mode `0666`: the card is read-only and everything on it is already in the log. A
user-scope card stays private anyway, because its directory is `0700`.

## Packaging, and the service

**The binary writes its own unit: `briard-tether install`.** A shipped unit file has to hardcode
the binary path, the tty group and a scope, and a package that ships the unit but forgets the udev
rule silently drops the autosuspend guard. Generating them keeps all of it consistent with the code:
the tty group is read off the host (`dialout` on Debian, Ubuntu, Fedora and NixOS, `uucp` on Arch,
or whatever owns an attached adapter), and the udev rule is derived from the family table.
`uninstall` removes the unit, the enable link, the running service and the udev rule.

**The scope is who is asking.** `sudo briard-tether install` is the machine's service; without
`sudo` it is a `systemd --user` service. The user install is lesser — it needs the tty group,
cannot arm the autosuspend guard, and starts at login unless lingering is enabled — so it says so
and asks before writing.

**The system unit runs as root for one thing: the USB autosuspend write** (`power/control` is owned
by root). Everything else needs only the tty group, and the test tree runs that way. **Nothing may
be built that works only as root**; what degrades without it logs its own fix.

**The udev rule is `99-briard-tether.rules`**, so it sorts after the tools it counters (TLP's
`85-tlp.rules`). It covers the windows tether's own write cannot — boot, a stopped tether, an
adapter tether is not using — but cannot stop a later `powertop --auto-tune`, since nothing in
udev can lock a sysfs attribute.

**The unit does not restart forever.** tether already retries everything a retry can fix, so an
exit means something else — a bad config, an address that will not bind. `Restart=on-failure` with
a limit of five a minute leaves the unit failed and saying why.

**The binary is static.** A plain `go build` on a machine with a C compiler links dynamically, so
both builds set `CGO_ENABLED=0`: the nix package, and the release workflow that cross-builds
`linux/amd64`, `linux/arm64`, `linux/armv7` and `windows/amd64` from a `v*` tag. CI and the
release both check the ELF. The costs, the pure-Go DNS resolver and `os/user`, are what this
binary wants anyway. The release stamps its version into the binary, and `briard-tether version`,
the first log line and the report card all say which build is running.

On Windows, `install` registers a service with the Service Control Manager, which is also the only
way to stop a tether there from outside its own console.

## Dependencies

Two, and a third needs an argument nobody has made yet.

**Serial: `go.bug.st/serial`** (BSD-3, maintained by Arduino, one transitive requirement:
`golang.org/x/sys`). Its `TIOCEXCL` on open keeps a second process off the dongle. The device layer
adds what it lacks: hardware flow control (see the family table) and **clearing `HUPCL`**, which
INV 7 rests on — tether opens the tty itself first, clears the flag, then hands the path to the
library configured to touch no modem bit. Its errors need translating: `PortError` does not
`Unwrap`, and reports "closed by us" and "unplugged" alike, so the device layer tracks which it was.

**mDNS: `github.com/brutella/dnssd`** (MIT), for two properties: it **re-resolves addresses per
interface at announce time**, so a tether started before the network is up is not stuck with a
stale address; and it **sends a goodbye**, which is what makes withdrawal immediate. Its caveats:

- ⚠️ **Every context handed to it must end by cancellation, never by deadline** — its socket readers
  exit only on `context.Canceled`, and an expired context leaves them spinning a CPU.
  `discovery.cancelOnly` is the one way in.
- On Windows and macOS its browser never reports a removal, and it announces loopback addresses —
  which is why tether picks the interfaces itself there.
- It costs four modules; two are Linux-only and behind build tags, so the Windows build stays clean.

**No client-side component, ever.** ZHA and zigbee2mqtt already speak TCP and mDNS; a plugin, fork
or upstream PR would tie tether to someone else's release cycle.

**A binary, not a library; the packages stay `internal/`.** A self-updating agent that ran tether
in-process would take the radio down every time it replaced itself, so **whatever restarts tether
is an OS service, outside the supervising program's process tree**. Separate processes also contain
a wedged serial read and keep the stable surface to a config file and a TCP port. If an in-process
need ever appears, the shape is fixed in advance: one exported
`func Run(ctx context.Context, cfg Config) error`, and nothing else.

## Testing

Three tiers; [CONTRIBUTING.md](CONTRIBUTING.md#testing) says how to run them. **The first two need
no hardware** — a pty stands in for the dongle, driven by real clients — so anyone can run
everything that decides whether a change is correct.

**Tier 1 — the pty rig.** A pty pair and a scripted responder: deterministic, no Zigbee semantics.
Covers lifecycle, takeover, fail-loud, accept ordering, TXT construction, family resolution. Most
tests live here.

**Tier 2 — the client gates.** zigpy-znp's test suite ships a Z-Stack emulator with a formed
network; bound to a pty it is a fake coordinator, and real clients driving it *through tether* are
the acceptance bar: zigpy, zigbee-herdsman (a second implementation), and above them real Home
Assistant and real zigbee2mqtt, which exercise config flows and discovery. ⚠️ The emulator answers
only what zigpy-znp sends, so herdsman needs a few extra replies, copied from its own test mocks.

**Tier 3 — hardware spot-checks, not in this repository.** USB and RF cannot be simulated, and a
pty has no `TIOCMGET`, so the control lines INV 7 is about are unreachable from tier 1. This tier
stages real plug events on a real bus and asserts no DTR/RTS edge by watching `usbmon`. Its
harnesses stay with the hardware, because a test nobody here can run goes stale unseen. The shared
half is here: **every scenario in `tests/` that stages a plug event takes `--device`** to run
against a real stick.

⚠️ **The multicast tests run only inside `scripts/wire-tests.sh`**, a network namespace with a veth
pair (no root needed). Run on a home LAN, they would offer your real Home Assistant a card for a
coordinator that lives four seconds. Loopback alone is not enough, since tether never advertises on
loopback.

**Where the risk is:** the **connection edges** — connect, disconnect, takeover, the dongle leaving
and returning, a client dying silently — not deep protocol behaviour. A new test earns its place by
covering an uncovered edge.

**Licensing in the test tree.** zigpy and zigpy-znp are GPL-3; tether only talks to them over a pty
or socket and ships none of them. Our files that import `zigpy_znp` may be derivative works, so
`tests/*.py` is GPL-3 throughout; the zigbee-herdsman harnesses are Apache-2.0. Each file carries
its licence.

## What today's code owes the two goals that are coming

**Client failover** needs nothing new, only that today's rules hold. INV 4's kick-old *is* the
failover primitive, so it must never become "refuse while busy". INV 1 means a client move never
resets the network. **tether holds no client state and must not start** — the device database and
network key live in the client. And **the advertised identity is independent of the client**, so a
moved client finds the same name and TXT. Deciding *where* the client runs is an orchestrator's job.

**A WiFi uplink.** Today's answer is INV 3 plus accept-then-silence: fail loud, let the client
recover. A resumable shim that hides session boundaries is fenced off, because hiding a boundary
removes the client's resync trigger and hiding an NCP reset misconfigures the stack — such a shim
must speak ASH and EZSP and becomes firmware-coupled. To keep it buildable later, one placement
rule: **serial timing assumptions live in `device` and `pipe` only.** Do not add an interface for
it.

## Code map

```
cmd/briard-tether/     main, the verbs, config parsing, the device generation loop
internal/device/       the serial port: open by stable path, family parameters, drain, vanish
internal/server/       one TCP listener, one client, kick-old takeover
internal/pipe/         the back-pressured copy, the frame census, probe arbitration
internal/discovery/    the mDNS advert, the TXT record, interface selection
internal/family/       the adapter table and its drift check against zigbee-herdsman
internal/management/   the liveness probe, the report card, the status socket
tests/                 the client gates, all runnable with no hardware
scripts/               the wire-test namespace, the pre-commit hook
```
