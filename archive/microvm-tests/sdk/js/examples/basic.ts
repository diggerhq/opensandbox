/**
 * Basic SDK test - create sandbox, run commands, destroy
 * 
 * Usage: npx tsx examples/basic.ts
 */

import { Client } from "../src/index.js"

const API_KEY = process.env.OPENSANDBOX_API_KEY ?? "ws_test_key";

async function main() {
    console.log("🌍 OpenSandbox SDK - Basic Test\n");
    
    const client = new Client(API_KEY);

    // Create a sandbox
    console.log("Creating sandbox...");
    const createResult = await client.create("test-sandbox");
    if (!createResult.success) {
        console.error("❌ Failed to create sandbox:", createResult.error);
        process.exit(1);
    }
    console.log("✅ Sandbox created\n");

    const sandbox = createResult.sandbox;

    // Run a few commands
    console.log("Running commands...");
    
    const cmd1 = await sandbox.run("echo 'Hello from sandbox!'");
    if (cmd1.success) {
        console.log(`  $ echo 'Hello from sandbox!'`);
        console.log(`    stdout: ${cmd1.stdout.trim()}`);
        console.log(`    snapshot: ${cmd1.snapshot}\n`);
    } else {
        console.error("❌ Command failed:", cmd1.error);
    }

    const cmd2 = await sandbox.run("pwd");
    if (cmd2.success) {
        console.log(`  $ pwd`);
        console.log(`    stdout: ${cmd2.stdout.trim()}\n`);
    }

    const cmd3 = await sandbox.run("ls -la");
    if (cmd3.success) {
        console.log(`  $ ls -la`);
        console.log(`    stdout:\n${cmd3.stdout}`);
    }

    // List snapshots
    console.log("Listing snapshots...");
    const snapsResult = await sandbox.snapshots();
    if (snapsResult.success) {
        console.log(`  Found ${snapsResult.snapshots.length} snapshots:`);
        for (const snap of snapsResult.snapshots) {
            console.log(`    - ${snap.snapshot_name} (${snap.command ?? "initial"})`);
        }
    }
    console.log();

    // Cleanup
    console.log("Destroying sandbox...");
    const destroyResult = await sandbox.destroy();
    if (destroyResult.success) {
        console.log("✅ Sandbox destroyed\n");
    } else {
        console.error("❌ Failed to destroy:", destroyResult.error);
    }

    console.log("🎉 Test complete!");
}

main().catch(console.error);

