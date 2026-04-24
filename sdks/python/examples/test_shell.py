"""test_shell.py — End-to-end test for exec.shell() (stateful shell sessions).

Usage:
    cd sdks/python
    python examples/test_shell.py

Environment:
    OPENCOMPUTER_API_URL  (default: http://localhost:8080)
    OPENCOMPUTER_API_KEY  (default: opensandbox-dev)
"""

from __future__ import annotations

import asyncio
import os

from opencomputer import Sandbox, ShellBusyError, ShellClosedError


API_URL = os.environ.get("OPENCOMPUTER_API_URL", "http://localhost:8080")
API_KEY = os.environ.get("OPENCOMPUTER_API_KEY", "opensandbox-dev")

passed = 0
failed = 0


def check(cond: bool, msg: str) -> None:
    global passed, failed
    if cond:
        passed += 1
        print(f"  ✓ {msg}")
    else:
        failed += 1
        print(f"  ✗ {msg}")


async def main() -> None:
    print("=== OpenSandbox Python Shell API Test ===\n")
    print(f"API: {API_URL}")

    print("\n--- 1. Creating sandbox ---")
    sandbox = await Sandbox.create(api_url=API_URL, api_key=API_KEY, template="base")
    print(f"  Sandbox: {sandbox.sandbox_id} ({sandbox.status})")
    check(sandbox.status == "running", "sandbox is running")

    try:
        # 2. basic run + exit code
        print("\n--- 2. shell.run('echo hello') ---")
        sh = await sandbox.exec.shell()
        r1 = await sh.run("echo hello")
        print(f'  stdout: "{r1.stdout.strip()}" exit={r1.exit_code}')
        check(r1.exit_code == 0, "exit 0")
        check(r1.stdout.strip() == "hello", "stdout matches")
        check(r1.stderr == "", "stderr empty")

        # 3. cwd persists
        print("\n--- 3. cwd persists across run() calls ---")
        await sh.run("cd /tmp")
        pwd = await sh.run("pwd")
        print(f'  pwd: "{pwd.stdout.strip()}"')
        check(pwd.stdout.strip() == "/tmp", "cwd persisted")

        # 4. exported env persists
        print("\n--- 4. exported env persists ---")
        await sh.run("export MY_SHELL_VAR=persistence-works")
        env_r = await sh.run("echo $MY_SHELL_VAR")
        print(f'  echo: "{env_r.stdout.strip()}"')
        check(env_r.stdout.strip() == "persistence-works", "env persisted")

        # 5. non-zero exit — subshell so it doesn't kill the outer shell
        print("\n--- 5. non-zero exit code ---")
        r_fail = await sh.run("( exit 7 )")
        print(f"  exit: {r_fail.exit_code}")
        check(r_fail.exit_code == 7, f"exit 7 (got {r_fail.exit_code})")
        r_fail2 = await sh.run("false && echo nope")
        check(r_fail2.exit_code == 1, f"exit 1 from false (got {r_fail2.exit_code})")

        # 6. stderr vs stdout separation
        print("\n--- 6. stderr separated from stdout ---")
        r_err = await sh.run("echo to-out; echo to-err >&2")
        print(f'  stdout="{r_err.stdout.strip()}" stderr="{r_err.stderr.strip()}"')
        check(r_err.stdout.strip() == "to-out", "stdout is to-out")
        check(r_err.stderr.strip() == "to-err", "stderr is to-err")
        check("__OC_" not in r_err.stderr, "sentinel token hidden from returned stderr")

        # 7. streaming callbacks
        print("\n--- 7. streaming callbacks ---")
        out_chunks: list[bytes] = []
        err_chunks: list[bytes] = []
        r_stream = await sh.run(
            "for i in 1 2 3; do echo stdout-$i; echo stderr-$i >&2; sleep 0.05; done",
            on_stdout=lambda b: out_chunks.append(b),
            on_stderr=lambda b: err_chunks.append(b),
        )
        joined_out = b"".join(out_chunks).decode()
        joined_err = b"".join(err_chunks).decode()
        check("stdout-1" in joined_out and "stdout-3" in joined_out, "stdout stream callbacks fired")
        check("stderr-1" in joined_err and "stderr-3" in joined_err, "stderr stream callbacks fired")
        check("__OC_" not in joined_err, "sentinel token hidden from on_stderr")
        check(r_stream.exit_code == 0, "streaming run exits 0")

        # 8. concurrent run rejects
        print("\n--- 8. concurrent run rejects ---")
        slow_task = asyncio.create_task(sh.run("sleep 0.3; echo done"))
        await asyncio.sleep(0.05)
        busy_err: Exception | None = None
        try:
            await sh.run("echo should-not-run")
        except Exception as e:
            busy_err = e
        check(isinstance(busy_err, ShellBusyError), "got ShellBusyError on concurrent run")
        slow_r = await slow_task
        check(slow_r.stdout.strip() == "done", "original run still completes")

        # 9. shell functions persist
        print("\n--- 9. shell functions persist ---")
        await sh.run("greet() { echo hi-$1; }")
        fn_r = await sh.run("greet world")
        check(fn_r.stdout.strip() == "hi-world", "defined function callable in later run")

        # 10. close
        print("\n--- 10. close ---")
        await sh.close()
        closed_err: Exception | None = None
        try:
            await sh.run("echo after-close")
        except Exception as e:
            closed_err = e
        check(isinstance(closed_err, ShellClosedError), "run after close rejects with ShellClosedError")

        # 11. shell with cwd/env at construction
        print("\n--- 11. shell(cwd=..., env=...) initial state ---")
        sh2 = await sandbox.exec.shell(cwd="/etc", env={"SHELL_INIT_VAR": "from-init"})
        r11a = await sh2.run("pwd")
        check(r11a.stdout.strip() == "/etc", "initial cwd honored")
        r11b = await sh2.run("echo $SHELL_INIT_VAR")
        check(r11b.stdout.strip() == "from-init", "initial env honored")
        await sh2.close()

        # 12. terminal-tab semantic: `exit` in user command closes the shell
        print("\n--- 12. exit N closes the shell (terminal-tab semantic) ---")
        sh_exit = await sandbox.exec.shell()
        exit_err: Exception | None = None
        try:
            await sh_exit.run("exit 42")
        except Exception as e:
            exit_err = e
        check(isinstance(exit_err, ShellClosedError), "exit 42 rejects the pending run with ShellClosedError")
        after_exit_err: Exception | None = None
        try:
            await sh_exit.run("echo after")
        except Exception as e:
            after_exit_err = e
        check(isinstance(after_exit_err, ShellClosedError), "subsequent run rejects once shell is closed")

        # 13. reattach to an open shell by sessionId
        print("\n--- 13. reattach_shell revisits an open shell ---")
        sh_a = await sandbox.exec.shell()
        await sh_a.run("cd /tmp")
        await sh_a.run("export REATTACH_VAR=round-trip")
        reattach_id = sh_a.session_id
        # Drop the reference without closing — server-side bash keeps running.
        sh_b = await sandbox.exec.reattach_shell(reattach_id)
        check(sh_b.session_id == reattach_id, "reattached shell has the same sessionId")
        r_pwd = await sh_b.run("pwd")
        check(r_pwd.stdout.strip() == "/tmp", f'reattach preserves cwd (got "{r_pwd.stdout.strip()}")')
        r_env2 = await sh_b.run("echo $REATTACH_VAR")
        check(r_env2.stdout.strip() == "round-trip", f'reattach preserves env (got "{r_env2.stdout.strip()}")')
        await sh_b.close()

        # 14. exec.background alias
        print("\n--- 14. exec.background alias ---")
        bg_exit = {"code": -999}

        def on_bg_exit(code: int) -> None:
            bg_exit["code"] = code

        bg_sess = await sandbox.exec.background(
            "sh", args=["-c", "echo bg-ok; sleep 0.2"], on_exit=on_bg_exit
        )
        await bg_sess.done
        await bg_sess.close()
        check(bg_exit["code"] == 0, f"exec.background returns exit code (got {bg_exit['code']})")

    finally:
        print("\n--- Killing sandbox ---")
        await sandbox.kill()
        check(sandbox.status == "stopped", "sandbox stopped")

    print(f"\n=== Results: {passed} passed, {failed} failed ===")
    if failed > 0:
        raise SystemExit(1)


if __name__ == "__main__":
    asyncio.run(main())
