/**
 * OpenSandbox Quick Start Example
 *
 * A minimal example showing basic usage of the SDK.
 *
 * Usage:
 *   npx ts-node examples/quickstart.ts [server-url]
 */

import { OpenSandbox } from '../src/index';

const SERVER_URL = process.argv[2] || 'http://localhost:8080';

async function main() {
  // Create client
  const client = new OpenSandbox(SERVER_URL);

  // Create a sandbox session
  console.log('Creating sandbox...');
  const sandbox = await client.create();
  console.log(`Session ID: ${sandbox.sessionId}`);
  if (sandbox.previewUrl) {
    console.log(`Preview URL: ${sandbox.previewUrl}`);
  }

  try {
    // Run commands
    console.log('\n--- Running Commands ---');

    const result = await sandbox.run('echo "Hello from OpenSandbox!" && date');
    console.log('Output:', result.stdout.trim());

    const lsResult = await sandbox.run('ls -la /');
    console.log('\nRoot directory listing:');
    console.log(lsResult.stdout);

    // Test environment variables
    console.log('--- Environment Variables ---');
    await sandbox.setEnv({ MY_VAR: 'test-value' });
    const envResult = await sandbox.run('echo "MY_VAR=$MY_VAR"');
    console.log(envResult.stdout.trim());

    // Try file operations (may not be available on older servers)
    console.log('\n--- File Operations ---');
    try {
      // Write a file using echo command as fallback
      await sandbox.run('echo "Hello, World!" > /tmp/hello.txt');
      console.log('Wrote /tmp/hello.txt via command');

      // Try the HTTP file API
      await sandbox.writeFile('/home/test.txt', 'Hello from SDK!');
      console.log('Wrote /home/test.txt via HTTP API');

      const content = await sandbox.readFileText('/home/test.txt');
      console.log('Read back:', content);

      const files = await sandbox.listFiles('/home');
      console.log('Files in /home:');
      for (const f of files) {
        console.log(`  - ${f.name} (${f.size} bytes)`);
      }
    } catch (err) {
      console.log('File API not available (server may need update)');
      console.log('Using command-based file operations instead...');

      // Fallback: use commands for file operations
      await sandbox.run('echo "Hello from SDK!" > /tmp/test.txt');
      const catResult = await sandbox.run('cat /tmp/test.txt');
      console.log('File content via cat:', catResult.stdout.trim());

      const lsHome = await sandbox.run('ls -la /tmp/');
      console.log('Files in /tmp:');
      console.log(lsHome.stdout);
    }

  } finally {
    // Cleanup
    console.log('\n--- Cleanup ---');
    await sandbox.destroy();
    await client.close();
    console.log('Done!');
  }
}

main().catch(console.error);
