/**
 * Time travel test - create files, snapshot, modify, restore
 * 
 * Usage: npx tsx examples/time-travel.ts
 */

import { Client } from "../src/index.js"

const API_KEY = process.env.OPENSANDBOX_API_KEY ?? "ws_test_key";

async function main() {
    console.log("⏰ OpenSandbox SDK - Time Travel Test\n");
    
    const client = new Client(API_KEY);

    // Create sandbox
    console.log("Creating sandbox...");
    const createResult = await client.create("time-travel-test");
    if (!createResult.success) {
        console.error("❌ Failed:", createResult.error);
        process.exit(1);
    }
    const sandbox = createResult.sandbox;
    console.log("✅ Sandbox created\n");

    // Step 1: Create a file
    console.log("Step 1: Creating file with 'hello'...");
    await sandbox.run("echo 'hello' > test.txt");
    const read1 = await sandbox.run("cat test.txt");
    if (read1.success) {
        console.log(`  Contents: ${read1.stdout.trim()}`);
        console.log(`  Snapshot: ${read1.snapshot}\n`);
    }

    // Save this snapshot name
    const snapsAfterStep1 = await sandbox.snapshots();
    const step1Snapshot = snapsAfterStep1.success 
        ? snapsAfterStep1.snapshots[snapsAfterStep1.snapshots.length - 1]?.snapshot_name 
        : null;

    // Step 2: Modify the file
    console.log("Step 2: Appending 'world' to file...");
    await sandbox.run("echo 'world' >> test.txt");
    const read2 = await sandbox.run("cat test.txt");
    if (read2.success) {
        console.log(`  Contents: ${read2.stdout.trim().replace(/\n/g, ', ')}\n`);
    }

    // Step 3: Time travel back!
    if (step1Snapshot) {
        console.log(`Step 3: Restoring to snapshot '${step1Snapshot}'...`);
        const restoreResult = await sandbox.restore(step1Snapshot);
        if (restoreResult.success) {
            console.log("✅ Restored!\n");
        }

        // Verify
        console.log("Step 4: Verifying file is back to 'hello' only...");
        const read3 = await sandbox.run("cat test.txt");
        if (read3.success) {
            console.log(`  Contents: ${read3.stdout.trim()}`);
            if (read3.stdout.trim() === "hello") {
                console.log("  ✅ Time travel successful!\n");
            } else {
                console.log("  ❌ Unexpected content\n");
            }
        }
    }

    // Cleanup
    console.log("Cleaning up...");
    await sandbox.destroy();
    console.log("✅ Done!\n");

    console.log("🎉 Time travel test complete!");
}

main().catch(console.error);

