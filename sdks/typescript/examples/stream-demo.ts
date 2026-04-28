/**
 * stream-demo.ts — simulates apt-install-style output over ~6 seconds via
 * shell.run(), printing each chunk with its arrival timestamp so you can
 * see exactly how streaming feels in practice.
 *
 * Usage:
 *   cd sdks/typescript
 *   npx tsx examples/stream-demo.ts
 */

import { Sandbox } from "../src/index.js";

const API_URL = process.env.OPENCOMPUTER_API_URL || "https://app.opencomputer.dev";
const API_KEY = process.env.OPENCOMPUTER_API_KEY || "opensandbox-dev";

// Pseudo-apt output. Uses /bin/echo (external, flushes on exit) rather
// than bash builtins so glibc block-buffering doesn't hide the streaming.
const APT_SIM = String.raw`
/bin/echo "Reading package lists..."
sleep 0.3
/bin/echo "Building dependency tree..."
sleep 0.2
/bin/echo "Reading state information..."
sleep 0.4
/bin/echo "The following NEW packages will be installed:"
/bin/echo "  libfoo1 libfoo-dev pkg-a pkg-b pkg-c"
sleep 0.3
/bin/echo "0 upgraded, 5 newly installed, 0 to remove and 0 not upgraded."
/bin/echo "Need to get 4,321 kB of archives."
/bin/echo "After this operation, 12.3 MB of additional disk space will be used."
sleep 0.4
for i in 1 2 3 4 5; do
  /bin/echo "Get:$i http://deb.example.com/debian bookworm/main amd64 pkg-$i"
  sleep 0.2
done
/bin/echo "Fetched 4,321 kB in 1s (4,321 kB/s)"
sleep 0.3
for i in 1 2 3 4 5; do
  /bin/echo "Selecting previously unselected package pkg-$i."
  /bin/echo "Unpacking pkg-$i (1.0-$i) ..."
  sleep 0.15
  /bin/echo "Setting up pkg-$i (1.0-$i) ..."
  sleep 0.15
done
/bin/echo "W: Could not fetch http://example.com/deprecated — ignoring" >&2
/bin/echo "Processing triggers for libc-bin (2.36-9+deb12u3) ..."
sleep 0.4
/bin/echo "Processing triggers for man-db (2.11.2-2) ..."
sleep 0.5
/bin/echo "done."
`;

async function main() {
  console.log(`API: ${API_URL}`);
  const sandbox = await Sandbox.create({
    apiUrl: API_URL,
    apiKey: API_KEY,
    template: "base",
  });
  console.log(`sandbox: ${sandbox.sandboxId}`);

  const decoder = new TextDecoder();
  const t0 = Date.now();
  let outCount = 0;
  let errCount = 0;
  let lastT = t0;

  const fmt = (n: number) => (n / 1000).toFixed(2).padStart(5, " ");

  try {
    const sh = await sandbox.exec.shell();

    const onStdout = (b: Uint8Array) => {
      outCount++;
      const now = Date.now();
      const gap = now - lastT;
      lastT = now;
      const text = decoder.decode(b).replace(/\n+$/, "");
      console.log(`[+${fmt(now - t0)}s Δ${fmt(gap)}s OUT] ${text}`);
    };

    const onStderr = (b: Uint8Array) => {
      errCount++;
      const now = Date.now();
      const gap = now - lastT;
      lastT = now;
      const text = decoder.decode(b).replace(/\n+$/, "");
      console.log(`[+${fmt(now - t0)}s Δ${fmt(gap)}s ERR] ${text}`);
    };

    console.log("--- starting simulated apt-install ---");
    const r = await sh.run(APT_SIM, { onStdout, onStderr });
    const total = (Date.now() - t0) / 1000;
    console.log("--- done ---");
    console.log(
      `exit=${r.exitCode}  total_wall=${total.toFixed(2)}s  ` +
        `stdout_chunks=${outCount} stderr_chunks=${errCount}`,
    );
    console.log(`stdout_bytes=${r.stdout.length} stderr_bytes=${r.stderr.length}`);
    await sh.close();
  } finally {
    await sandbox.kill();
  }
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
