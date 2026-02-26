import { Sandbox } from "../sdks/typescript/src/index";

async function main() {
  console.log("Creating sandbox...");
  const sb = await Sandbox.create({
    template: "base",
    timeout: 3600,
    apiUrl: "https://app.opencomputer.dev",
  });
  console.log(`Sandbox created: ${sb.sandboxId}`);

  // Run commands
  console.log("\n--- Commands ---");
  const result = await sb.commands.run("echo hello from typescript sdk");
  console.log(`stdout: ${result.stdout.trim()}`);
  console.log(`exit code: ${result.exitCode}`);

  const uname = await sb.commands.run("uname -a");
  console.log(`uname: ${uname.stdout.trim()}`);

  // Filesystem
  console.log("\n--- Filesystem ---");
  await sb.files.write("/tmp/greeting.txt", "Hello from TypeScript SDK!");
  const content = await sb.files.read("/tmp/greeting.txt");
  console.log(`file content: ${content}`);

  const exists = await sb.files.exists("/tmp/greeting.txt");
  console.log(`file exists: ${exists}`);

  await sb.files.makeDir("/tmp/mydir");
  await sb.files.write("/tmp/mydir/test.py", 'print("hello from python")');

  const entries = await sb.files.list("/tmp");
  console.log("ls /tmp:");
  for (const entry of entries) {
    console.log(`  ${entry.isDir ? "d" : "-"} ${entry.name}`);
  }

  // Run a multi-line script
  console.log("\n--- Script execution ---");
  await sb.files.write(
    "/tmp/script.sh",
    [
      "#!/bin/bash",
      'echo "Current directory: $(pwd)"',
      'echo "User: $(whoami)"',
      'echo "Date: $(date)"',
      'echo "Files in /tmp:"',
      "ls /tmp",
    ].join("\n"),
  );

  const script = await sb.commands.run("bash /tmp/script.sh");
  console.log(script.stdout);

  // Check sandbox status
  console.log("--- Status ---");
  const running = await sb.isRunning();
  console.log(`running: ${running}`);

  // Clean up
  await sb.kill();
  console.log("\nSandbox killed. Done!");
}

main().catch(console.error);
