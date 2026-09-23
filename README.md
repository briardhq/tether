# briard-tether

**Bring a USB Zigbee coordinator to Home Assistant's ZHA or to zigbee2mqtt over the network** —
reliably, with no client-side plugin and no cloud.

Run it on the machine the dongle is plugged into. It serves the coordinator on a TCP port and
advertises it over mDNS, so ZHA discovers it and zigbee2mqtt reaches it with `mdns://` or `tcp://`.
The dongle can live where the RF is best, and the client wherever you want — including a VM, which
cannot own a USB device.

It is built by the [briard](https://briard.io) project, which uses it to reach every radio, and it
works standalone.

> **Status: beta.** Built and tested against real zigbee2mqtt, real Home Assistant and real
> hardware, but not yet on many machines we don't own. Please
> [tell us what broke](https://github.com/briardhq/tether/issues).

## Features

A generic serial-to-network bridge like ser2net can carry a Zigbee coordinator, but it leaves you
to assemble the setup by hand and knows nothing about what can go wrong. tether is built for
exactly this one job:

- **Zero configuration.** tether detects the adapter, recognises the model (using the same adapter
  table as zigbee2mqtt), and opens it with the right baud rate and flow control. The device path
  it uses survives reboots and replugging.
- **Automatic discovery.** It advertises the coordinator over mDNS, so Home Assistant offers it
  with no typing and zigbee2mqtt finds it with `mdns://`, even after the address changes.
- **The radio is never reset by a client.** The adapter stays open while clients come and go, and
  connecting or disconnecting never toggles the lines that reset it. Your Zigbee network keeps
  running while the client restarts.
- **One client at a time, and a new one wins.** A client that crashed without closing its
  connection cannot lock out its own restart: the new connection takes over and the stale one is
  closed.
- **Fails loudly, never silently.** If the dongle is unplugged, the client's connection is closed
  at once, so it recovers instead of hanging, and tether reopens the dongle when it comes back.
  TCP keepalives catch connections that died without a word.
- **Protected from USB power saving.** It stops the operating system from suspending the dongle,
  a common cause of setups that work for a while and then quietly die.
- **Tells you what is happening.** `briard-tether status` shows the adapter, the client and the
  Zigbee traffic: frames each way, radio resets and why, corrupted bytes. When something breaks,
  it can show whether the fault is the network or the dongle.
- **Tested against the real clients.** The test suite drives real zigbee2mqtt, real Home Assistant
  and zigpy through tether against an emulated coordinator. What only hardware can show, such as
  the reset lines and unplugging, is tested by hand on real dongles.
- **Installs itself.** One static binary for Linux (x86, 64-bit and 32-bit Raspberry Pi) and
  Windows. `install` sets it up as a service that starts at boot. No client-side plugin.

## Install

1. Plug in your Zigbee dongle.
2. Download the file for your machine from the
   [latest release](https://github.com/briardhq/tether/releases/latest):

   | machine | file |
   | --- | --- |
   | most Linux PCs and servers | `briard-tether-linux-amd64` |
   | Raspberry Pi, 64-bit OS | `briard-tether-linux-arm64` |
   | Raspberry Pi, 32-bit OS | `briard-tether-linux-armv7` |
   | Windows | `briard-tether-windows-amd64.exe` |

   Not sure which Linux one? `uname -m` says `x86_64`, `aarch64` or `armv7l`, in that order.

3. Run `install`:

   **Linux** (with your file name):

   ```sh
   chmod +x briard-tether-linux-amd64
   sudo ./briard-tether-linux-amd64 install
   ```

   **Windows** — in a terminal opened with *Run as administrator*:

   ```
   .\briard-tether-windows-amd64.exe install
   ```

That's it. tether is now a service that starts at boot, finds your dongle, and advertises it on
your network. There is nothing to configure.

To check on it, run the same file with `status` (`./briard-tether-linux-amd64 status`). To remove
it, use `uninstall` the same way you used `install`.

## Use it with zigbee2mqtt

### New installation

Set the serial port in `configuration.yaml`:

```yaml
serial:
  port: mdns://zigbee-coordinator
```

zigbee2mqtt finds tether on your network at every start, so it keeps working if tether's machine
changes address.

⚠️ **Running zigbee2mqtt in Docker?** On Docker's default bridge network it cannot see
`mdns://`. Either add `network_mode: host` to the container, or connect directly — which also needs
the adapter type, since a plain address cannot say what radio is behind it:

```yaml
serial:
  port: tcp://<tether-host>:6638
  adapter: zstack   # `status` shows the radio: znp → zstack, ezsp → ember, deconz → deconz
```

With `tcp://`, give tether's machine a static address or a DHCP reservation.

### Migrate your existing installation

Your devices stay paired. Move the dongle to tether's machine if it is not there already, then
change `serial.port` as above and nothing else — leave the network settings and any `adapter:` key
alone.

After restarting, the log must say `zigbee-herdsman started (resumed)`. If it says `reset` or
`restored`, the network was rebuilt: stop zigbee2mqtt and find out why before letting it run.

## Use it with ZHA (Home Assistant's built-in Zigbee integration)

### New installation

Once tether is running, Home Assistant shows a new Zigbee device under
**Settings → Devices & services**. Click **Add** and follow the steps.

Give tether's machine a static address or a DHCP reservation: ZHA remembers the address, and
reconnecting after it changes means walking through the migration steps below.

To set it up without discovery, choose the *socket* option and enter `socket://<tether-host>:6638`.

### Migrate your existing installation

Your devices stay paired. Move the dongle to tether's machine and install tether there. Home
Assistant offers a new Zigbee card; open it and choose:

| step | choose |
| --- | --- |
| *Migrate or change adapter settings* | **Change the current adapter's settings** |
| *Migrate to a new adapter* (it says that anyway) | **Advanced migration** |
| *Network formation* | **Keep adapter network settings** |

⚠️ **Do not choose "Migrate automatically"** — it tries to erase the old adapter's settings, and
on this move the old adapter is the dongle you are keeping. "Keep adapter network settings" writes
nothing to the radio. If the dongle stays on the same machine, the same choices are under
**Configure** on the ZHA integration instead of a new card.

As with a new installation, give tether's machine a static address or a DHCP reservation.

## More

- [ARCHITECTURE.md](ARCHITECTURE.md) — how it is built and why, and the guarantees it makes.
- [CONTRIBUTING.md](CONTRIBUTING.md) — building, testing, and what a good change looks like.
- [NOTICE](NOTICE) — the adapter table is derived from zigbee-herdsman's, with thanks.
