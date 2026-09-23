# Contributing

Thanks for looking. Issues, pull requests and forks are all welcome. This is what is worth knowing
first: the guarantees the tests enforce, and the defaults a review will apply.

## Before you write a patch

briard-tether is written by a very small team and it is early; the design still moves. **Open
source, not open governance:** direction is ours, features start as issues rather than pull
requests, and pull requests are looked at weekly, small ones first. So for anything bigger than a
bug fix, open an issue first — it is a shame to write something that was never going to land. If
we decline a patch, we will say so directly rather than let it rot.

**Licence: Apache-2.0** ([LICENSE](LICENSE); [NOTICE](NOTICE) for the adapter table). The
exception is the test harnesses that import zigpy-znp, which are GPL-3
([text](LICENSES/GPL-3.0-or-later.txt), [why](ARCHITECTURE.md#testing)) and labelled per file.

**No CLA — the [Developer Certificate of Origin](https://developercertificate.org/) instead.** Sign
off your commits with `git commit -s`. Without a CLA nobody can relicense your code later, which
makes the no-rug-pull promise structural.

**No AI attribution trailers** (`Co-Authored-By: Claude` and the like) — CI rejects them. Use
whatever tools you like; the sign-off says the commit is yours.

## Build and test

You need [Nix](https://nixos.org/download/) with flakes; the dev shell brings everything else.

```sh
nix develop                        # go, python, node, the client stacks; installs the gofmt hook
go test -race ./...                # unit tests and the pty rig — what CI runs
./scripts/wire-tests.sh            # the real-multicast tests, in a network namespace
go build -o briard-tether ./cmd/briard-tether
python3 tests/run_gate.py          # a real zigpy client through a real tether
python3 tests/run_suite.py         # zigpy-znp's own application suite through tether
nix build .#default                # a source build: one static binary
```

**`-race` is not optional.** The product is a concurrent pipe — a device reader, a client reader,
and a takeover that closes one session while another arrives — and a race there is exactly the
"works until it doesn't" bug this project exists to stop.

⚠️ **Never run the multicast tests outside `scripts/wire-tests.sh`.** On a home network they offer
your real Home Assistant a discovery card for a coordinator that lives four seconds.

The other harnesses in `tests/` drive zigbee-herdsman, zigbee2mqtt and Home Assistant against an
emulated coordinator; the Home Assistant one needs `nix develop .#zha` (a separate shell because it
is 1.3 GiB).

**Everything here runs without a Zigbee stick**, so you and a reviewer can both run everything that
decides whether a change is correct. Hardware checks (a real dongle, `usbmon` across a plug event)
live elsewhere; if a change seems to need one, raise it in an issue and we will run it before
merging. Scenarios that stage a plug event take `--device` to run against a real stick.

To install a build: `sudo ./result/bin/briard-tether install` for a system service, or without
`sudo` for a `systemd --user` one. `uninstall` undoes either.

## The guarantees

[ARCHITECTURE.md](ARCHITECTURE.md#invariants) states eight invariants with their reasoning, and
most are test-enforced. If a change seems to need breaking one, it is almost certainly mis-scoped —
open an issue first. The short form: the radio is never touched by client activity; no listener
without an open port; fail loud, never stall; exactly one client, with kick-old takeover; no lossy
buffer; byte-transparent; DTR and RTS pinned; the TXT record is the truth.

Two more rules no test can express:

- **Protocol awareness has exactly two homes** — the liveness probe and the frame census.
  Everything else that wants to parse a frame is scope creep.
- **No client-side component, ever.** No plugin, fork or upstream PR to ZHA or zigbee2mqtt.

## What a good change looks like

**The smallest change that satisfies the requirement.** This project fails by expansion, not
omission. A new abstraction, dependency, module, config option, or second way to do something needs
an issue first.

- **New dependency: default no.** Weigh complexity removed against surface added. Two are spent
  (serial, mDNS); a third needs a real argument.
- **New abstraction: not until three real call sites need it** — including "so we can swap the
  transport later".
- **New config key: default no.** Config is deployment wiring, never behaviour. An absent key means
  "derive it", never "off". Keys are never renamed or repurposed.
- **One way per concern.** Standard library throughout: `log`, `fmt.Errorf` with `%w`,
  `encoding/json`. No second language in the shipped binary; test helpers may be Python or
  JavaScript where the upstream client is.
- **Every log line is actionable.** This binary has to be diagnosable at 3am by somebody whose
  lights stopped working.
- **Name things for the role, not the mechanism**, using the standard term.
- **Linux first, Windows stays possible.** No udev or systemd assumptions below packaging, no
  POSIX-only syscalls outside `internal/device`. CI builds for Windows, linux/arm64 and
  linux/armv7 on every commit, and runs the tests natively on arm64.
- **Comments describe current state** — what the code does and why — never "was", "until" or
  "now". History belongs in commit messages.

New tests:

- **Decision logic gets exhaustive unit tests** — enumerate the states, do not sample them.
- **Mechanisms get the pty rig.** Do not mock the thing you are verifying.
- **The client gates are the acceptance bar** for anything touching the data path or discovery.
- **The assertion must be able to fail.** A test that passes against a broken implementation is
  worse than none.
- **Cover an uncovered edge** — connect, disconnect, takeover, unplug, a silent client death —
  before moving more bytes down a proven path.

If a comment only makes sense with a document you cannot open, that is a bug — please report it.

## Issues, discussions and security

- **Issues** are for bugs — behaviour that differs from the docs. The output of
  `briard-tether status` and the adapter you use is usually all we need.
- **Discussions** are for help, questions and "should this work like X?".
- **New adapters:** the table is imported from zigbee-herdsman, so the fastest route is often a row
  in their `adapterDiscovery.ts`. Open an issue either way — the `radio` config key may already
  cover your stick.
- **Security bugs:** email **security@briard.io** instead of opening an issue. You will get a human
  reply, and credit unless you prefer not.
