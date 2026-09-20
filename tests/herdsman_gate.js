// The second client gate: zigbee-herdsman driving a coordinator through tether.
//
// Licence, and it differs from the Python beside it: zigbee-herdsman is MIT, so importing it
// carries no copyleft obligation and this file stays Apache-2.0 with the rest of the tree. The
// zigpy harnesses are GPL-3 because zigpy is; nothing about that reaches here.
//
// **Why a second client at all.** zigpy is Python and herdsman is TypeScript, and they share no
// code: nothing proved about one proves anything about the other. This is the stack behind
// zigbee2mqtt, below the MQTT and the device converters — Z2M adds those above `Adapter` and
// touches none of what follows.
//
// What it proves, in the order it proves it:
//
//   1. **the address**, when it was given a service type rather than a host: `discoverAdapter`
//      is the function `Adapter.create` calls with whatever is in Z2M's `port:` line, so
//      `mdns://zigbee-coordinator` here is a real line in a real Z2M config. It must come back
//      with the port tether is really listening on and with `zstack`, mapped from our TXT's
//      `radio_type: znp`. Over this path nothing asks the radio anything, so that
//      mapping is the whole of the machine-readable contract;
//   2. **`resumed`** — herdsman read the NIB and the key material through tether, compared them
//      against its configuration itself, and concluded the network was already there. Not
//      `restored`, which would mean it rebuilt from a backup, and not `reset`, which would mean
//      it formed a new network over the top of one that existed;
//   3. **the coordinator's own account of itself** — identity, network parameters, and a full
//      `backup`, which is where the heavy NVRAM traffic is: the boot reads a handful of NV
//      items, the backup walks the extended tables and the device list;
//   4. **INV 1 with a second implementation on the end of it.** The client goes away and comes
//      back against a live tether, and the coordinator answers the second one with exactly what
//      it told the first — down to the device count in the backup. A transport that reset the
//      radio on client lifecycle — the failure this project exists to stop being — would
//      answer differently, or not at all.
//
// **It cannot re-form a network, and that is by construction rather than by care.** herdsman
// commissions when its configuration disagrees with the adapter, so the network it is handed is
// read off the coordinator first and handed straight back: configuration and adapter agree by
// definition, and `beginCommissioning` is unreachable. The reading is Z-Stack 3 only — Z-Stack
// 1.2 keeps its key somewhere else — which is no loss, since the CC2531 images exist for zigpy.
//
// Everything it then does, a zigbee2mqtt start does too: registering endpoints and adding the
// green-power group are writes, they are idempotent, and they are not a network.
//
// Usage:
//
//     node tests/herdsman_gate.js tcp://127.0.0.1:6638
//     node tests/herdsman_gate.js mdns://zigbee-coordinator 6638
//
// Run it through tests/run_gate.py, which arranges a coordinator and a tether in front of it;
// see that file on why the mDNS form needs a network namespace of its own.

const assert = require("node:assert");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const { Adapter } = require("zigbee-herdsman/dist/adapter");
const { discoverAdapter } = require("zigbee-herdsman/dist/adapter/adapterDiscovery");
const { AdapterNvMemory } = require("zigbee-herdsman/dist/adapter/z-stack/adapter/adapter-nv-memory");
const { NvItemsIds } = require("zigbee-herdsman/dist/adapter/z-stack/constants/common");
const Structs = require("zigbee-herdsman/dist/adapter/z-stack/structs");
const { unpackChannelList } = require("zigbee-herdsman/dist/adapter/z-stack/utils");
const { Znp } = require("zigbee-herdsman/dist/adapter/z-stack/znp");
const { setLogger } = require("zigbee-herdsman/dist/utils/logger");

function logToStderr() {
  /* herdsman's default logger writes debug and info to stdout. That is fine for a human and
   * fatal for a harness that reports on stdout itself (herdsman_client.js), so both files put
   * it where diagnostics belong. It stays on by default and is not silenced: when a gate fails,
   * the last few frames before it are the whole story. */
  const write = (level) => (messageOrLambda, namespace) => {
    const message = typeof messageOrLambda === "function" ? messageOrLambda() : messageOrLambda;
    process.stderr.write(`[${new Date().toISOString()}] ${namespace}: ${message}\n`);
  };
  setLogger({ debug: write(), info: write(), warning: write(), error: write() });
}

// What tether opens the port at, and what ZHA hardcodes for a networked coordinator. Neither
// end of a TCP socket has a baud rate; herdsman wants the argument anyway.
const BAUD = 115200;

async function resolve(address, expectedPort) {
  // `adapter` is undefined on purpose. Over mDNS herdsman ignores any configured adapter type
  // and takes ours out of the TXT record, so passing one would let a wrong `radio_type` pass
  // unnoticed. For a `tcp://` address there is nothing to discover and it has to be named.
  const isMdns = address.startsWith("mdns://");
  const found = await discoverAdapter(isMdns ? undefined : "zstack", address);
  console.log(`  adapter  ${found.adapter}`);
  console.log(`  path     ${found.path}`);

  if (isMdns) {
    // A URL rather than a string compare: herdsman builds the path from `service.addresses[0]
    // ?? service.host`, so the host half is whatever the advert resolved to and is not ours to
    // predict. The port and the radio are.
    const url = new URL(found.path);
    assert.strictEqual(url.protocol, "tcp:", "herdsman must be pointed at a raw TCP stream");
    if (expectedPort !== undefined) {
      assert.strictEqual(
        url.port,
        String(expectedPort),
        "the advert sent this client to a port tether is not listening on",
      );
    }
    assert.strictEqual(
      found.adapter,
      "zstack",
      "`radio_type: znp` must map to the zstack driver; over mDNS nothing asks the radio, so " +
        "a wrong value here is a client driving the wrong library at our port",
    );
  }

  return found;
}

async function readNetwork(tcpPath) {
  /* The network as the coordinator has it, which is what the client is then configured with —
   * see the header on why that is a safety property and not a shortcut. Every read here is one
   * herdsman's own `determineStrategy` makes a moment later. */
  const znp = new Znp(tcpPath, BAUD, false);
  await znp.open();
  try {
    const nv = new AdapterNvMemory(znp);
    // Wrapped, because a corrupted stream fails here first and fails obscurely: herdsman's MT
    // framer drops what it cannot parse, the NV read comes back empty, and the struct builder
    // reports a null it was handed rather than the byte that was wrong.
    let nib;
    let active;
    try {
      await nv.init();
      nib = await nv.readItem(NvItemsIds.NIB, 0, Structs.nib);
      active = await nv.readItem(NvItemsIds.NWK_ACTIVE_KEY_INFO, 0, Structs.nwkKeyDescriptor);
    } catch (error) {
      throw new Error(
        `could not read the coordinator's NVRAM through ${tcpPath} (${error.message}). ` +
          "A real MT framer is on this end, so a stream that is not byte-for-byte what the " +
          "radio sent fails exactly here — INV 6.",
      );
    }
    assert.ok(nib, "the coordinator has no NIB: it carries no network to resume");
    assert.ok(active?.key, "the coordinator has no active network key");
    return {
      panID: nib.nwkPanId,
      extendedPanID: [...nib.extendedPANID],
      channelList: unpackChannelList(nib.channelList),
      networkKey: [...active.key],
      networkKeyDistribute: false,
    };
  } finally {
    await znp.close();
  }
}

async function attach(tcpPath, adapterType, backupDir) {
  /* Bring an adapter up the way a zigbee2mqtt start does, and hand it back running. */
  const network = await readNetwork(tcpPath);
  const adapter = await Adapter.create(
    network,
    { path: tcpPath, adapter: adapterType, baudRate: BAUD, rtscts: false },
    // Nowhere near the repo, and nowhere that survives the run: a stored backup is an input to
    // herdsman's startup decision, and a leftover one would make this gate depend on the
    // previous gate.
    path.join(backupDir, "coordinator-backup.json"),
    { disableLED: false },
  );
  const start = await adapter.start();
  return { adapter, network, start };
}

async function describe({ adapter, network, start }) {
  /* What the coordinator says about itself. Split from `attach` so the unplug scenario can hold
   * an adapter open across an event rather than taking it straight down again. */
  const version = await adapter.getCoordinatorVersion();
  const parameters = await adapter.getNetworkParameters();

  // `backup` is where the heavy NVRAM traffic is, and it is the reason it is here: the boot
  // above reads a handful of NV items, while this walks the extended tables, the device list
  // and the key material. It is read-only — `createBackup` writes a file and never the radio
  // — and it roughly doubles what crosses the wire, which is the point.
  const supportsBackup = await adapter.supportsBackup();
  const backup = supportsBackup ? await adapter.backup([]) : undefined;

  // ⚠️ Zero seconds, always, and never anything else: this *closes* the join window. A gate
  // that opened somebody's network to joins for even a minute would be a worse thing than
  // anything it could catch. What it exercises is `sendZdo` on the broadcast path — a second
  // ZDO command shape, and the only one here that is not a read.
  await adapter.permitJoin(0);

  return {
    start,
    ieee: await adapter.getCoordinatorIEEE(),
    type: version.type,
    pan_id: `0x${parameters.panID.toString(16).toUpperCase().padStart(4, "0")}`,
    extended_pan_id: parameters.extendedPanID,
    channel: String(parameters.channel),
    nwk_update_id: String(parameters.nwkUpdateID),
    // Deliberately not the key itself. It is a secret on somebody's real network, this output
    // is meant to be pasted into a runbook or an issue, and "a key is present" is the whole of
    // what this test needs to know about it.
    network_key: network.networkKey.some((b) => b !== 0) ? "present" : "absent",
    // Compared across the reconnect like everything else, so a coordinator that forgot a
    // device — or grew one — between two clients is a failure and not a footnote.
    backup: backup ? `${backup.devices.length} devices` : "unsupported",
  };
}

async function interrogate(tcpPath, adapterType, backupDir) {
  /* Connect as zigbee2mqtt does, read what the coordinator says about itself, and leave. */
  const attached = await attach(tcpPath, adapterType, backupDir);
  try {
    return await describe(attached);
  } finally {
    await attached.adapter.stop();
  }
}

async function main() {
  const [address, expectedPort] = process.argv.slice(2);
  if (!address) {
    console.error("usage: herdsman_gate.js tcp://host:port | mdns://<service-type> [port]");
    return 2;
  }

  console.log(`resolving ${address} the way zigbee2mqtt's \`port:\` line does`);
  const found = await resolve(address, expectedPort);

  const backupDir = fs.mkdtempSync(path.join(os.tmpdir(), "herdsman-gate-"));
  try {
    console.log(`connecting as a zigbee-herdsman client to ${found.path}`);
    const first = await interrogate(found.path, found.adapter, backupDir);
    for (const [key, value] of Object.entries(first)) {
      console.log(`  ${key.padEnd(16)} ${value}`);
    }

    assert.strictEqual(
      first.start,
      "resumed",
      "`restored` means herdsman rebuilt the network from a backup and `reset` means it formed " +
        "a new one; only `resumed` means the network it found was the network it kept",
    );

    // INV 1 with a second implementation: the client goes away and comes back, and the radio
    // must not have noticed.
    console.log("disconnecting, then reconnecting against the same live tether");
    const second = await interrogate(found.path, found.adapter, backupDir);

    assert.deepStrictEqual(
      second,
      first,
      "the coordinator answered the second client differently, which means the client " +
        "lifecycle reached the radio — INV 1 broken, and the failure this project exists to " +
        "stop being",
    );
    console.log("the radio answered the second client exactly as it answered the first (INV 1)");
    return 0;
  } finally {
    fs.rmSync(backupDir, { recursive: true, force: true });
  }
}

// The three pieces the unplug scenario borrows, so that it meets the coordinator through the
// same code this gate does rather than through a second copy of it.
module.exports = { BAUD, attach, describe, logToStderr, resolve };

if (require.main === module) {
  logToStderr();
  main().then(
    (code) => process.exit(code),
    (error) => {
      // The message, not the stack: every failure here is either "nothing answered" or an
      // assertion naming what went wrong, and both say more than a trace through somebody
      // else's adapter code.
      console.error(`\nFAIL: ${error.message}`);
      process.exit(1);
    },
  );
}
