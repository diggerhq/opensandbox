/**
 * List all VMs and snapshots
 * 
 * Usage: npx tsx examples/list-vms.ts
 */

import { Client } from "../src/index.js"

const API_KEY = process.env.OPENSANDBOX_API_KEY ?? "ws_test_key";

async function main() {
    console.log("📋 OpenSandbox SDK - List VMs & Snapshots\n");
    
    const client = new Client(API_KEY);

    // List VMs
    console.log("VMs:");
    console.log("─".repeat(50));
    const vmsResult = await client.list();
    if (vmsResult.success) {
        if (vmsResult.vms.length === 0) {
            console.log("  (no VMs found)");
        } else {
            for (const vm of vmsResult.vms) {
                console.log(`  ${vm.name}`);
                console.log(`    Status: ${vm.status}`);
                console.log(`    SSH Port: ${vm.ssh_port}`);
                console.log(`    Created: ${vm.created_at}`);
                console.log();
            }
        }
    } else {
        console.error("❌ Failed to list VMs:", vmsResult.error);
    }

    // List all snapshots
    console.log("\nAll Snapshots:");
    console.log("─".repeat(50));
    const snapsResult = await client.snapshots();
    if (snapsResult.success) {
        if (snapsResult.snapshots.length === 0) {
            console.log("  (no snapshots found)");
        } else {
            for (const snap of snapsResult.snapshots) {
                console.log(`  ${snap.snapshot_name}`);
                console.log(`    VM ID: ${snap.vm_id}`);
                console.log(`    Command: ${snap.command ?? "(initial)"}`);
                console.log(`    S3: ${snap.s3_key ?? "(local only)"}`);
                console.log(`    Created: ${snap.created_at}`);
                console.log();
            }
        }
    } else {
        console.error("❌ Failed to list snapshots:", snapsResult.error);
    }
}

main().catch(console.error);

