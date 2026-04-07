/**
 * Test runtime secret injection into a running sandbox.
 *
 * Flow:
 *   1. Create a sandbox with an initial secret (INITIAL_SECRET)
 *   2. Verify the initial secret is available via the secrets proxy
 *   3. Inject a new secret (INJECTED_SECRET) into the running sandbox
 *   4. Verify the new secret is also available
 *   5. Verify the initial secret still works
 *
 * Usage:
 *   OPENCOMPUTER_API_KEY=... npx tsx examples/test-inject-secrets.ts
 *
 * Requires a secret store with at least one secret, or uses direct env injection.
 */

const API_URL = process.env.OPENCOMPUTER_API_URL || "https://app.opencomputer.dev";
const API_KEY = process.env.OPENCOMPUTER_API_KEY;

if (!API_KEY) {
  console.error("Set OPENCOMPUTER_API_KEY");
  process.exit(1);
}

const headers: Record<string, string> = {
  "Content-Type": "application/json",
  "X-API-Key": API_KEY,
};

function resolveApiUrl(url: string): string {
  const base = url.replace(/\/+$/, "");
  return base.endsWith("/api") ? base : `${base}/api`;
}

const apiUrl = resolveApiUrl(API_URL);

async function exec(sandboxId: string, cmd: string): Promise<{ exitCode: number; stdout: string; stderr: string }> {
  const resp = await fetch(`${apiUrl}/sandboxes/${sandboxId}/exec/run`, {
    method: "POST",
    headers,
    body: JSON.stringify({ cmd: "sh", args: ["-c", cmd], timeout: 10 }),
  });
  if (!resp.ok) throw new Error(`exec failed: ${resp.status} ${await resp.text()}`);
  return resp.json();
}

async function main() {
  console.log("=== Test: Runtime Secret Injection ===\n");

  // Step 1: Create sandbox with an initial secret via envs
  console.log("1. Creating sandbox with initial secret...");
  const createResp = await fetch(`${apiUrl}/sandboxes`, {
    method: "POST",
    headers,
    body: JSON.stringify({
      timeout: 120,
      envs: { INITIAL_SECRET: "hello-from-creation" },
    }),
  });
  if (!createResp.ok) throw new Error(`create failed: ${createResp.status} ${await createResp.text()}`);
  const { sandboxID } = await createResp.json() as { sandboxID: string };
  console.log(`   Sandbox: ${sandboxID}\n`);

  await new Promise((r) => setTimeout(r, 3000));

  // Step 2: Verify initial secret is set
  console.log("2. Verifying initial secret...");
  const check1 = await exec(sandboxID, "echo $INITIAL_SECRET");
  const initialValue = check1.stdout.trim();
  if (initialValue.startsWith("osb_sealed_")) {
    console.log(`   ✓ INITIAL_SECRET is sealed: ${initialValue.slice(0, 30)}...`);
  } else if (initialValue === "hello-from-creation") {
    console.log(`   ⚠ INITIAL_SECRET is plaintext (secrets proxy not active)`);
  } else {
    console.log(`   ✗ INITIAL_SECRET unexpected value: "${initialValue}"`);
  }

  // Step 3: Verify INJECTED_SECRET is NOT set yet
  console.log("\n3. Verifying INJECTED_SECRET is not set...");
  const check2 = await exec(sandboxID, "echo \"val=$INJECTED_SECRET\"");
  const preInject = check2.stdout.trim();
  if (preInject === "val=") {
    console.log("   ✓ INJECTED_SECRET is empty (as expected)");
  } else {
    console.log(`   ✗ INJECTED_SECRET unexpectedly set: "${preInject}"`);
  }

  // Step 4: Inject a new secret
  console.log("\n4. Injecting new secret...");
  const injectResp = await fetch(`${apiUrl}/sandboxes/${sandboxID}/inject-secrets`, {
    method: "POST",
    headers,
    body: JSON.stringify({
      envs: {
        INJECTED_SECRET: "hello-from-injection",
        ANOTHER_SECRET: "second-value",
      },
    }),
  });
  if (!injectResp.ok) {
    const text = await injectResp.text();
    console.log(`   ✗ Inject failed: ${injectResp.status} ${text}`);
    await cleanup(sandboxID);
    process.exit(1);
  }
  const injectResult = await injectResp.json() as { injectedCount: number };
  console.log(`   ✓ Injected ${injectResult.injectedCount} secrets`);

  // Step 5: Verify injected secret is now available
  console.log("\n5. Verifying injected secrets...");
  const check3 = await exec(sandboxID, "echo $INJECTED_SECRET");
  const injectedValue = check3.stdout.trim();
  if (injectedValue.startsWith("osb_sealed_")) {
    console.log(`   ✓ INJECTED_SECRET is sealed: ${injectedValue.slice(0, 30)}...`);
  } else if (injectedValue === "hello-from-injection") {
    console.log(`   ⚠ INJECTED_SECRET is plaintext (secrets proxy not active)`);
  } else if (injectedValue === "") {
    console.log("   ✗ INJECTED_SECRET is empty (injection may have failed)");
  } else {
    console.log(`   ? INJECTED_SECRET value: "${injectedValue}"`);
  }

  const check4 = await exec(sandboxID, "echo $ANOTHER_SECRET");
  const anotherValue = check4.stdout.trim();
  if (anotherValue.startsWith("osb_sealed_") || anotherValue === "second-value") {
    console.log(`   ✓ ANOTHER_SECRET is set`);
  } else {
    console.log(`   ✗ ANOTHER_SECRET not set: "${anotherValue}"`);
  }

  // Step 6: Verify initial secret still works
  console.log("\n6. Verifying initial secret still works...");
  const check5 = await exec(sandboxID, "echo $INITIAL_SECRET");
  const stillWorks = check5.stdout.trim();
  if (stillWorks === initialValue) {
    console.log(`   ✓ INITIAL_SECRET unchanged`);
  } else {
    console.log(`   ✗ INITIAL_SECRET changed: was "${initialValue}", now "${stillWorks}"`);
  }

  // Step 7: Test that injected secrets work through HTTPS proxy
  // (the sealed token gets substituted when making outbound HTTPS requests)
  console.log("\n7. Testing secret substitution via HTTPS proxy...");
  const check6 = await exec(
    sandboxID,
    `curl -s -H "Authorization: Bearer $INJECTED_SECRET" https://httpbin.org/headers 2>/dev/null | head -20`
  );
  if (check6.stdout.includes("hello-from-injection")) {
    console.log("   ✓ Secret was substituted in outbound HTTPS request");
  } else if (check6.stdout.includes("osb_sealed_")) {
    console.log("   ✗ Sealed token was NOT substituted (proxy may not be active)");
  } else {
    console.log("   ? Could not verify (httpbin may be unreachable)");
    console.log(`     stdout: ${check6.stdout.slice(0, 200)}`);
  }

  await cleanup(sandboxID);

  console.log("\n=== Test Complete ===");
}

async function cleanup(sandboxID: string) {
  console.log(`\nCleaning up ${sandboxID}...`);
  await fetch(`${apiUrl}/sandboxes/${sandboxID}`, { method: "DELETE", headers });
}

main().catch((err) => {
  console.error("Fatal:", err.message);
  process.exit(1);
});
