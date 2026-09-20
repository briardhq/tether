// A real zigbee-herdsman client, held open so a Python scenario can do something to it.
// Apache-2.0 — zigbee-herdsman is MIT; see herdsman_gate.js.
//
// It exists because the client is TypeScript and the staging is Python: whoever owns the pty
// pulls the plug, and whoever owns the process sends the signal. The event has to be **the same
// event** the zigpy runs stage, or the two measurements cannot be compared, so the staging stays
// in one place and this watches — or dies — from the client's side.
//
// It reports rather than asserts. What tether owes is asserted on the Python side; what
// *herdsman* does is somebody else's behaviour, written down exactly as zigpy's is.
//
// Output is one JSON object per line on stdout, so the Python side can read it without parsing
// prose. herdsman's own logging goes to stderr and is ignored.
//
//     node tests/herdsman_client.js tcp://127.0.0.1:6638           # attach, report, leave
//     node tests/herdsman_client.js tcp://127.0.0.1:6638 --watch   # ... then watch for the close
//     node tests/herdsman_client.js tcp://127.0.0.1:6638 --hold    # ... then keep the radio busy

const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const { attach, describe, logToStderr } = require("./herdsman_gate.js");

/** How long to wait for the disconnect to reach the client before calling it a stall. */
const NOTICE_TIMEOUT_MS = 15000;

/** How long a request issued after the close gets before it counts as hung. */
const REQUEST_TIMEOUT_MS = 15000;

function emit(object) {
  process.stdout.write(`${JSON.stringify(object)}\n`);
}

function disconnected(adapter) {
  /* The event zigbee2mqtt hangs its restart on. herdsman never reconnects by itself — the
   * controller above it emits `disconnected` and Z2M exits so its supervisor restarts it
   * — so this is the whole of the client's recovery machinery, and whether it fires
   * is the question. */
  return new Promise((resolve) => {
    const started = Date.now();
    const timer = setTimeout(() => resolve({ fired: false, after_ms: null }), NOTICE_TIMEOUT_MS);
    adapter.once("disconnected", () => {
      clearTimeout(timer);
      resolve({ fired: true, after_ms: Date.now() - started });
    });
  });
}

async function requestAfterClose(adapter) {
  /* INV 3's substance from the only side that can judge it. A stall here is the real failure:
   * a request that neither completes nor fails is what a bridge going quiet does to a client's
   * timers, and it is what INV 3 says must never happen. */
  const hung = Symbol("hung");
  const timer = new Promise((resolve) => setTimeout(() => resolve(hung), REQUEST_TIMEOUT_MS));
  try {
    const result = await Promise.race([adapter.getNetworkParameters(), timer]);
    if (result === hung) {
      return { outcome: "hung", detail: `no answer in ${REQUEST_TIMEOUT_MS}ms` };
    }
    return { outcome: "answered", detail: JSON.stringify(result) };
  } catch (error) {
    return { outcome: "failed", detail: `${error.constructor.name}: ${error.message}` };
  }
}

async function hold(adapter) {
  /* Keep the radio genuinely busy, so that a signal arriving from outside lands *mid-operation*
   * rather than at idle. That is the difference between this and a disconnect: a client killed
   * between two frames leaves requests in flight and a radio part-way through answering them,
   * and what the successor then finds is the thing worth checking.
   *
   * `backup` is the loop body because it is the heaviest thing herdsman will do — roughly 900
   * frames of NV reads per pass — so a kill at a random moment is almost certain to land inside
   * a multi-frame sequence. It is read-only, and it never returns here: this process is meant to
   * be killed, and it exits no other way. */
  for (let pass = 1; ; pass += 1) {
    await adapter.backup([]);
    emit({ busy: pass });
  }
}

async function main() {
  // Before anything talks: stdout is this file's report and nothing else may write to it.
  logToStderr();

  const [address, mode] = process.argv.slice(2);
  if (!address) {
    console.error("usage: herdsman_client.js tcp://host:port [--watch|--hold]");
    return 2;
  }

  const backupDir = fs.mkdtempSync(path.join(os.tmpdir(), "herdsman-client-"));
  let attached;
  try {
    attached = await attach(address, "zstack", backupDir);
    emit({ facts: await describe(attached) });

    if (mode !== "--watch" && mode !== "--hold") {
      await attached.adapter.stop();
      return 0;
    }

    // The line the Python side waits on before it does whatever it came to do. Flushed by
    // `emit`, which writes synchronously to a pipe.
    emit({ ready: true });

    if (mode === "--hold") {
      await hold(attached.adapter);
      return 0; // unreachable: `hold` only ends when this process is killed
    }

    const notice = await disconnected(attached.adapter);
    emit({ disconnected: notice });
    emit({ request_after_close: await requestAfterClose(attached.adapter) });
    return 0;
  } finally {
    // Best-effort: the adapter's port is already gone in the watched case, and `stop` on a dead
    // socket is allowed to throw. Failing the teardown would turn a measurement into a fault.
    try {
      if (attached && mode !== undefined) await attached.adapter.stop();
    } catch {
      /* the dongle is unplugged; there is nothing to close cleanly */
    }
    fs.rmSync(backupDir, { recursive: true, force: true });
  }
}

main().then(
  (code) => process.exit(code),
  (error) => {
    emit({ error: `${error.constructor.name}: ${error.message}` });
    process.exit(1);
  },
);
