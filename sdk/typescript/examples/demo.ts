/**
 * OpenSandbox TypeScript SDK Demo
 *
 * This demo showcases all the features of the SDK:
 * - Creating sandbox sessions
 * - Running commands
 * - File operations (write, read, list)
 * - Environment and working directory management
 * - Preview URLs
 *
 * Usage:
 *   npx ts-node examples/demo.ts [server-url]
 *
 * Example:
 *   npx ts-node examples/demo.ts http://localhost:8080
 */

import { OpenSandbox, Sandbox, CommandResult } from '../src/index';

// Configuration
const SERVER_URL = process.argv[2] || 'http://localhost:8080';

// Helper to print section headers
function section(title: string): void {
  console.log('\n' + '='.repeat(60));
  console.log(`  ${title}`);
  console.log('='.repeat(60) + '\n');
}

// Helper to print command results
function printResult(label: string, result: CommandResult): void {
  console.log(`${label}:`);
  console.log(`  Exit Code: ${result.exitCode}`);
  if (result.stdout.trim()) {
    console.log(`  Stdout: ${result.stdout.trim()}`);
  }
  if (result.stderr.trim()) {
    console.log(`  Stderr: ${result.stderr.trim()}`);
  }
}

async function main(): Promise<void> {
  console.log('OpenSandbox TypeScript SDK Demo');
  console.log(`Connecting to: ${SERVER_URL}`);

  const client = new OpenSandbox(SERVER_URL);
  let sandbox: Sandbox | null = null;

  try {
    // =========================================
    section('1. Creating a Sandbox Session');
    // =========================================

    sandbox = await client.create({
      env: { DEMO_VAR: 'hello-from-sdk' },
    });

    console.log(`Session ID: ${sandbox.sessionId}`);
    console.log(`Preview URL: ${sandbox.previewUrl || '(not configured)'}`);

    // =========================================
    section('2. Running Basic Commands');
    // =========================================

    // Simple echo
    const echoResult = await sandbox.run('echo "Hello from OpenSandbox!"');
    printResult('Echo command', echoResult);

    // Check environment variable
    const envResult = await sandbox.run('echo $DEMO_VAR');
    printResult('Environment variable', envResult);

    // List root directory
    const lsResult = await sandbox.run('ls -la /');
    printResult('List root directory', lsResult);

    // Check current user and working directory
    const whoamiResult = await sandbox.run('whoami && pwd');
    printResult('User and directory', whoamiResult);

    // =========================================
    section('3. File Operations');
    // =========================================

    // Write a text file
    console.log('Writing /home/test.txt...');
    await sandbox.writeFile('/home/test.txt', 'Hello, World!\nThis is a test file.');
    console.log('  Done!');

    // Write a JSON file
    const jsonData = {
      name: 'OpenSandbox Demo',
      version: '1.0.0',
      features: ['commands', 'files', 'isolation'],
    };
    console.log('Writing /home/config.json...');
    await sandbox.writeFile('/home/config.json', JSON.stringify(jsonData, null, 2));
    console.log('  Done!');

    // Read the text file back
    console.log('\nReading /home/test.txt...');
    const textContent = await sandbox.readFileText('/home/test.txt');
    console.log(`  Content: "${textContent}"`);

    // Read the JSON file back
    console.log('\nReading /home/config.json...');
    const jsonContent = await sandbox.readFileText('/home/config.json');
    console.log(`  Content: ${jsonContent}`);

    // List directory contents
    console.log('\nListing /home directory...');
    const files = await sandbox.listFiles('/home');
    console.log('  Files:');
    for (const file of files) {
      const type = file.isDirectory ? 'DIR ' : 'FILE';
      console.log(`    [${type}] ${file.name} (${file.size} bytes) - ${file.path}`);
    }

    // =========================================
    section('4. Running Scripts');
    // =========================================

    // Create and run a shell script
    const shellScript = `#!/bin/bash
echo "Running shell script..."
echo "Date: $(date)"
echo "Hostname: $(hostname)"
echo "Files in /home:"
ls -la /home
`;
    console.log('Writing and running shell script...');
    await sandbox.writeFile('/home/script.sh', shellScript);
    const scriptResult = await sandbox.run('chmod +x /home/script.sh && /home/script.sh');
    printResult('Shell script', scriptResult);

    // Check if Node.js is available and run a JS script
    const nodeCheck = await sandbox.run('which node || echo "not found"');
    if (nodeCheck.stdout.includes('/node')) {
      console.log('\nNode.js is available, running JS script...');
      const jsScript = `
const os = require('os');
console.log('Node.js version:', process.version);
console.log('Platform:', os.platform());
console.log('Architecture:', os.arch());
console.log('Free memory:', Math.round(os.freemem() / 1024 / 1024), 'MB');
`;
      await sandbox.writeFile('/home/script.js', jsScript);
      const jsResult = await sandbox.run('node /home/script.js');
      printResult('Node.js script', jsResult);
    } else {
      console.log('\nNode.js not available in sandbox, skipping JS script test');
    }

    // =========================================
    section('5. Environment & Working Directory');
    // =========================================

    // Set additional environment variables
    console.log('Setting environment variables...');
    await sandbox.setEnv({
      API_KEY: 'test-api-key-12345',
      DEBUG: 'true',
      APP_ENV: 'development',
    });
    console.log('  Done!');

    // Verify environment
    const envCheckResult = await sandbox.run('echo "API_KEY=$API_KEY, DEBUG=$DEBUG, APP_ENV=$APP_ENV"');
    printResult('Environment check', envCheckResult);

    // Set working directory
    console.log('\nSetting working directory to /home...');
    await sandbox.setCwd('/home');
    console.log('  Done!');

    // Verify working directory
    const pwdResult = await sandbox.run('pwd && ls');
    printResult('Working directory check', pwdResult);

    // =========================================
    section('6. Resource Limits');
    // =========================================

    // Test with custom resource limits
    console.log('Running command with custom limits...');
    const limitedResult = await sandbox.run('echo "Running with limits"', {
      timeoutMs: 5000,  // 5 second timeout
      memKb: 65536,     // 64MB memory limit
    });
    printResult('Limited command', limitedResult);

    // =========================================
    section('7. Error Handling');
    // =========================================

    // Test command that fails
    console.log('Running a command that will fail...');
    const failResult = await sandbox.run('exit 42');
    printResult('Failed command', failResult);
    console.log(`  (Expected exit code 42, got ${failResult.exitCode})`);

    // Test reading non-existent file
    console.log('\nTrying to read non-existent file...');
    try {
      await sandbox.readFile('/home/does-not-exist.txt');
      console.log('  ERROR: Should have thrown an error!');
    } catch (error) {
      console.log(`  Caught expected error: ${(error as Error).message}`);
    }

    // =========================================
    section('Demo Complete!');
    // =========================================

    console.log('All tests passed successfully!\n');

  } catch (error) {
    console.error('\nError during demo:', error);
    process.exit(1);
  } finally {
    // Cleanup
    if (sandbox) {
      console.log('Cleaning up sandbox session...');
      await sandbox.destroy();
      console.log('  Session destroyed.');
    }
    await client.close();
    console.log('  Client closed.');
  }
}

// Run the demo
main().catch(console.error);
