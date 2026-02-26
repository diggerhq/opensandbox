"""Test script for the OpenSandbox Templates API.

Templates are standalone — they're not accessed through a Sandbox instance.
They let you build custom container images from Dockerfiles, which can then
be used as the `template` parameter when creating sandboxes.

Usage:
    python test_templates.py [API_URL] [API_KEY]

Defaults to http://localhost:8080 with no API key.
"""

import asyncio
import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "sdks", "python"))

from opencomputer import Sandbox, Template
import httpx


async def main():
    api_url = sys.argv[1] if len(sys.argv) > 1 else os.environ.get("OPENSANDBOX_API_URL", "http://localhost:8080")
    api_key = sys.argv[2] if len(sys.argv) > 2 else os.environ.get("OPENSANDBOX_API_KEY", "")

    api_url = api_url.rstrip("/")
    api_base = api_url if api_url.endswith("/api") else f"{api_url}/api"

    headers = {}
    if api_key:
        headers["X-API-Key"] = api_key

    client = httpx.AsyncClient(base_url=api_base, headers=headers, timeout=300.0)
    templates = Template._from_client(client)

    try:
        # ── 1. List default templates ────────────────────────────────
        print("=== 1. List default templates ===")
        all_templates = await templates.list()
        print(f"Found {len(all_templates)} templates:")
        for t in all_templates:
            print(f"  - {t.name} (id={t.template_id}, tag={t.tag}, status={t.status})")

        # ── 2. Get a specific template ───────────────────────────────
        print("\n=== 2. Get 'base' template ===")
        base = await templates.get("base")
        print(f"  name={base.name}, id={base.template_id}, tag={base.tag}, status={base.status}")

        # ── 3. Build a custom template ───────────────────────────────
        print("\n=== 3. Build custom template 'test-custom' ===")
        print("  (This runs `podman build` synchronously — may take a minute...)")
        custom = await templates.build("test-custom", """\
FROM ubuntu:22.04
RUN apt-get update -qq && apt-get install -y -qq curl > /dev/null 2>&1
RUN echo "custom template ready" > /etc/motd
""")
        print(f"  Built! id={custom.template_id}, name={custom.name}, status={custom.status}")

        # ── 4. Verify it shows up in the list ────────────────────────
        print("\n=== 4. Verify template appears in list ===")
        all_templates = await templates.list()
        names = [t.name for t in all_templates]
        assert "test-custom" in names, f"Expected 'test-custom' in {names}"
        print(f"  OK — found {len(all_templates)} templates: {names}")

        # ── 5. Get the custom template by name ───────────────────────
        print("\n=== 5. Get custom template ===")
        fetched = await templates.get("test-custom")
        print(f"  name={fetched.name}, status={fetched.status}")

        # ── 6. Create a sandbox using the custom template ────────────
        print("\n=== 6. Create sandbox with custom template ===")
        sandbox = await Sandbox.create(
            template="test-custom",
            timeout=60,
            api_url=api_url,
            api_key=api_key,
        )
        print(f"  Sandbox created: {sandbox.sandbox_id}")

        # Verify the custom image works
        result = await sandbox.commands.run("cat /etc/motd")
        print(f"  /etc/motd says: {result.stdout.strip()}")
        assert "custom template ready" in result.stdout, f"Unexpected motd: {result.stdout}"

        result = await sandbox.commands.run("curl --version | head -1")
        print(f"  curl: {result.stdout.strip()}")

        await sandbox.kill()
        print("  Sandbox killed.")

        # ── 7. Delete the custom template ────────────────────────────
        print("\n=== 7. Delete custom template ===")
        await templates.delete("test-custom")
        print("  Deleted 'test-custom'.")

        # ── 8. Verify deletion ───────────────────────────────────────
        print("\n=== 8. Verify deletion ===")
        try:
            await templates.get("test-custom")
            print("  ERROR: template still exists after deletion!")
        except httpx.HTTPStatusError as e:
            if e.response.status_code == 404:
                print("  OK — template not found (404), as expected.")
            else:
                print(f"  Unexpected error: {e}")

        # Final list
        all_templates = await templates.list()
        names = [t.name for t in all_templates]
        assert "test-custom" not in names, f"'test-custom' still in {names}"
        print(f"  Remaining templates: {names}")

        print("\n✅ All template tests passed!")

    except Exception as e:
        print(f"\n❌ Test failed: {e}")
        raise
    finally:
        await client.aclose()


asyncio.run(main())
