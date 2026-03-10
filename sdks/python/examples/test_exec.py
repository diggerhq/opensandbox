"""
test_exec.py — End-to-end test for the session-based exec API (Python SDK)

Usage:
    cd sdks/python
    pip install -e .
    python examples/test_exec.py

Environment:
    OPENCOMPUTER_API_URL  (default: http://localhost:8080)
    OPENCOMPUTER_API_KEY  (default: opensandbox-dev)
"""

import asyncio
import os

from opencomputer import Sandbox

API_URL = os.environ.get("OPENCOMPUTER_API_URL", "http://localhost:8080")
API_KEY = os.environ.get("OPENCOMPUTER_API_KEY", "opensandbox-dev")

passed = 0
failed = 0


def check(condition: bool, msg: str):
    global passed, failed
    if condition:
        passed += 1
        print(f"  ✓ {msg}")
    else:
        failed += 1
        print(f"  ✗ {msg}")


async def main():
    print("=== OpenSandbox Exec API Test (Python) ===\n")
    print(f"API: {API_URL}")

    # 1. Create sandbox
    print("\n--- 1. Creating sandbox ---")
    sandbox = await Sandbox.create(api_url=API_URL, api_key=API_KEY, template="base")
    print(f"  Sandbox: {sandbox.sandbox_id} ({sandbox.status})")
    check(sandbox.status == "running", "sandbox is running")

    try:
        # 2. exec.run() — quick command
        print("\n--- 2. exec.run('echo hello world') ---")
        result = await sandbox.exec.run("echo hello world")
        print(f'  stdout: "{result.stdout.strip()}"')
        check(result.exit_code == 0, f"exit code is 0 (got {result.exit_code})")
        check(result.stdout.strip() == "hello world", "stdout matches")

        # 3. exec.run() — ls
        print("\n--- 3. exec.run('ls /') ---")
        ls_result = await sandbox.exec.run("ls /")
        check(ls_result.exit_code == 0, "exit code is 0")
        check("usr" in ls_result.stdout, "stdout contains 'usr'")
        check("bin" in ls_result.stdout, "stdout contains 'bin'")

        # 4. exec.run() with env vars
        print("\n--- 4. exec.run with env vars ---")
        env_result = await sandbox.exec.run(
            "echo $MY_VAR-$FOO", env={"MY_VAR": "hello", "FOO": "bar"}
        )
        print(f'  output: "{env_result.stdout.strip()}"')
        check(env_result.stdout.strip() == "hello-bar", "env vars passed correctly")

        # 5. exec.run() with cwd
        print("\n--- 5. exec.run('pwd') with cwd=/tmp ---")
        cwd_result = await sandbox.exec.run("pwd", cwd="/tmp")
        print(f'  pwd: "{cwd_result.stdout.strip()}"')
        check(cwd_result.stdout.strip() == "/tmp", "cwd is /tmp")

        # 6. exec.run() — non-zero exit code
        print("\n--- 6. exec.run('exit 42') ---")
        fail_result = await sandbox.exec.run("exit 42")
        print(f"  exit: {fail_result.exit_code}")
        check(fail_result.exit_code == 42, f"exit code is 42 (got {fail_result.exit_code})")

        # 7. exec.run() — stderr
        print("\n--- 7. exec.run stderr ---")
        stderr_result = await sandbox.exec.run("echo error-msg >&2")
        print(f'  stderr: "{stderr_result.stderr.strip()}"')
        check(stderr_result.stderr.strip() == "error-msg", "stderr captured")

        # 8. exec.start() + list + kill
        print("\n--- 8. exec.start('sleep 60') + list + kill ---")
        session_id = await sandbox.exec.start("sleep", args=["60"])
        print(f"  session: {session_id}")

        sessions = await sandbox.exec.list()
        sleep_sessions = [s for s in sessions if s.session_id == session_id]
        check(len(sleep_sessions) == 1, "session appears in list")
        check(sleep_sessions[0].running, "session is running")

        await sandbox.exec.kill(session_id)
        print("  killed")
        await asyncio.sleep(0.5)

        sessions_after = await sandbox.exec.list()
        killed = [s for s in sessions_after if s.session_id == session_id]
        if killed:
            check(not killed[0].running, "session is no longer running after kill")
        else:
            check(True, "session cleaned up after kill")

        # 9. File write + read via exec
        print("\n--- 9. Write file + cat ---")
        await sandbox.files.write("/tmp/test.txt", "Hello from Python SDK!\n")
        cat_result = await sandbox.exec.run("cat /tmp/test.txt")
        print(f'  cat: "{cat_result.stdout.strip()}"')
        check(cat_result.stdout.strip() == "Hello from Python SDK!", "file content matches")

        # 10. Multi-command script
        print("\n--- 10. Multi-command script ---")
        script_result = await sandbox.exec.run(
            "echo hostname=$(hostname); echo user=$(whoami); echo arch=$(uname -m)"
        )
        print(f"  {script_result.stdout.strip()}")
        check("hostname=" in script_result.stdout, "has hostname")
        check("user=" in script_result.stdout, "has user")

    finally:
        # 11. Cleanup
        print("\n--- 11. Killing sandbox ---")
        await sandbox.kill()
        check(sandbox.status == "stopped", "sandbox stopped")
        await sandbox.close()

    # Summary
    print(f"\n=== Results: {passed} passed, {failed} failed ===")
    if failed > 0:
        raise SystemExit(1)


if __name__ == "__main__":
    asyncio.run(main())
