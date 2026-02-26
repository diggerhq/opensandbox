#!/usr/bin/env python3
"""
Domain & TLS Verification Test

Tests:
  1. Each sandbox gets a unique subdomain
  2. HTTPS works with valid cert
  3. HTTP server inside sandbox is reachable via subdomain
  4. Subdomain routes to correct sandbox
  5. Multiple HTTP methods through TLS

Usage:
  python examples/test_domain_tls.py
"""

import asyncio
import ssl
import sys

import httpx

from opensandbox import Sandbox

GREEN = "\033[32m"
RED = "\033[31m"
BOLD = "\033[1m"
DIM = "\033[2m"
RESET = "\033[0m"

passed = 0
failed = 0


def green(msg: str) -> None:
    print(f"{GREEN}✓ {msg}{RESET}")


def red(msg: str) -> None:
    print(f"{RED}✗ {msg}{RESET}")


def bold(msg: str) -> None:
    print(f"{BOLD}{msg}{RESET}")


def dim(msg: str) -> None:
    print(f"{DIM}  {msg}{RESET}")


def check(desc: str, condition: bool, detail: str = "") -> None:
    global passed, failed
    if condition:
        green(desc)
        passed += 1
    else:
        red(f"{desc} ({detail})" if detail else desc)
        failed += 1


def get_tls_cert_info(hostname: str) -> dict:
    """Get TLS certificate info for a hostname."""
    import socket
    context = ssl.create_default_context()
    with socket.create_connection((hostname, 443), timeout=10) as sock:
        with context.wrap_socket(sock, server_hostname=hostname) as ssock:
            cert = ssock.getpeercert()
            issuer = dict(x[0] for x in cert.get("issuer", ()))
            subject = dict(x[0] for x in cert.get("subject", ()))
            return {
                "issuer": issuer.get("organizationName", "unknown"),
                "subject": subject.get("commonName", "unknown"),
                "valid_to": cert.get("notAfter", "unknown"),
                "valid": True,
            }


HTTP_SERVER = """
const http = require('http');
const os = require('os');
const server = http.createServer((req, res) => {
  res.writeHead(200, {'Content-Type':'application/json'});
  res.end(JSON.stringify({
    path: req.url,
    hostname: os.hostname(),
    sandboxId: process.env.SANDBOX_ID || 'unknown',
    timestamp: Date.now()
  }));
});
server.listen(80, '0.0.0.0', () => console.log('Server ready'));
"""


async def main() -> None:
    global passed, failed

    bold("\n╔══════════════════════════════════════════════════╗")
    bold("║       Domain & TLS Verification Test             ║")
    bold("╚══════════════════════════════════════════════════╝\n")

    sandboxes: list[Sandbox] = []

    try:
        # ── Test 1: Unique subdomains ──
        bold("━━━ Test 1: Unique subdomain assignment ━━━\n")

        for i in range(3):
            sb = await Sandbox.create(template="node", timeout=120)
            sandboxes.append(sb)
            dim(f"Sandbox {i + 1}: {sb.sandbox_id} → {sb.domain}")

        domains = [s.domain for s in sandboxes]
        unique_domains = set(domains)
        check("All 3 sandboxes got unique domains", len(unique_domains) == 3)
        check("All domains end with .workers.opensandbox.ai",
              all(d.endswith(".workers.opensandbox.ai") for d in domains))
        check("All domains are subdomains (single level)",
              all("." not in d.split(".workers.opensandbox.ai")[0] for d in domains))
        print()

        # ── Test 2: TLS certificate validation ──
        bold("━━━ Test 2: TLS certificate validation ━━━\n")

        # Start HTTP servers on all sandboxes
        for i, sb in enumerate(sandboxes):
            await sb.files.write("/tmp/server.js", HTTP_SERVER)
            await sb.commands.run(
                f"SANDBOX_ID={sb.sandbox_id} nohup node /tmp/server.js > /tmp/server.log 2>&1 &")

        await asyncio.sleep(2)

        try:
            cert_info = get_tls_cert_info(sandboxes[0].domain)
            check("TLS certificate is valid", cert_info["valid"])
            issuer = cert_info["issuer"]
            check("Certificate issued by Let's Encrypt or similar",
                  any(s in issuer for s in ["Let's Encrypt", "R3", "R10", "R11", "E5", "E6"]),
                  issuer)
            dim(f"Issuer: {cert_info['issuer']}")
            dim(f"Subject: {cert_info['subject']}")
            dim(f"Valid to: {cert_info['valid_to']}")
        except Exception as e:
            check("TLS connection succeeded", False, str(e))
        print()

        # ── Test 3: HTTPS requests work ──
        bold("━━━ Test 3: HTTPS requests to sandbox servers ━━━\n")

        async with httpx.AsyncClient(timeout=10.0) as client:
            for i, sb in enumerate(sandboxes):
                try:
                    resp = await client.get(f"https://{sb.domain}/test-path")
                    check(f"Sandbox {i + 1}: HTTPS 200", resp.status_code == 200,
                          f"status {resp.status_code}")
                    if resp.status_code == 200:
                        data = resp.json()
                        check(f"Sandbox {i + 1}: correct path",
                              data["path"] == "/test-path", data["path"])
                        check(f"Sandbox {i + 1}: has hostname", bool(data.get("hostname")))
                except Exception as e:
                    check(f"Sandbox {i + 1}: HTTPS request", False, str(e))
        print()

        # ── Test 4: Subdomain routing isolation ──
        bold("━━━ Test 4: Subdomain routing isolation ━━━\n")

        async with httpx.AsyncClient(timeout=10.0) as client:
            routing_results = []
            for i, sb in enumerate(sandboxes):
                try:
                    resp = await client.get(f"https://{sb.domain}/")
                    data = resp.json()
                    routing_results.append({
                        "index": i,
                        "sandbox_id": sb.sandbox_id,
                        "returned_id": data.get("sandboxId", "error"),
                    })
                except Exception as e:
                    routing_results.append({
                        "index": i,
                        "sandbox_id": sb.sandbox_id,
                        "returned_id": f"error: {e}",
                    })

            for r in routing_results:
                check(f"Sandbox {r['index'] + 1}: routed to correct container",
                      r["returned_id"] == r["sandbox_id"],
                      f"expected {r['sandbox_id']}, got {r['returned_id']}")

            if len(sandboxes) >= 2:
                resp1 = await client.get(f"https://{sandboxes[0].domain}/")
                resp2 = await client.get(f"https://{sandboxes[1].domain}/")
                data1 = resp1.json()
                data2 = resp2.json()
                check("Cross-routing: different sandboxes return different IDs",
                      data1["sandboxId"] != data2["sandboxId"])
        print()

        # ── Test 5: Multiple methods ──
        bold("━━━ Test 5: HTTP methods through TLS ━━━\n")

        domain = sandboxes[0].domain
        async with httpx.AsyncClient(timeout=10.0) as client:
            get_resp = await client.get(f"https://{domain}/get-test")
            check("GET request works", get_resp.status_code == 200)

            post_resp = await client.post(f"https://{domain}/post-test",
                                          json={"test": True})
            check("POST request works", post_resp.status_code == 200)

            put_resp = await client.put(f"https://{domain}/put-test",
                                        content=b"put-data")
            check("PUT request works", put_resp.status_code == 200)
        print()

    except Exception as e:
        red(f"Fatal error: {e}")
        failed += 1
    finally:
        for sb in sandboxes:
            try:
                await sb.kill()
            except Exception:
                pass
        if sandboxes:
            green(f"{len(sandboxes)} sandboxes killed")

    # --- Summary ---
    bold("========================================")
    bold(f" Results: {passed} passed, {failed} failed")
    bold("========================================\n")
    if failed > 0:
        sys.exit(1)


if __name__ == "__main__":
    asyncio.run(main())
