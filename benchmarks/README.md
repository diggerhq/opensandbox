# OpenSandbox vs E2B Benchmarks

Benchmark suite to compare OpenSandbox with E2B cloud sandboxes.

## Results Summary

Benchmarks run against OpenSandbox deployed on Fly.io (`https://opensandbox-test.fly.dev`) and E2B cloud.

### 1. Sandbox Creation Time

| Provider | Create Avg (ms) | Destroy Avg (ms) |
|----------|-----------------|------------------|
| OpenSandbox | 131 | 85 |
| E2B | 274 | 113 |

**OpenSandbox is ~2x faster** at creating new sandbox sessions.

### 2. Command Execution Latency

| Command | OpenSandbox (ms) | E2B (ms) | Winner |
|---------|------------------|----------|--------|
| echo | 67 | 61 | E2B |
| pwd | 89 | 39 | E2B |
| ls | 93 | 42 | E2B |
| uname | 65 | 38 | E2B |
| git --version | 110 | 62 | E2B |
| **Overall Avg** | **111** | **51** | **E2B** |

**E2B is ~2x faster** for individual command execution. This is likely due to E2B's native SDK vs OpenSandbox's HTTP API overhead.

### 3. File Operations

| Operation | OpenSandbox (ms) | E2B (ms) |
|-----------|------------------|----------|
| Write 100B | 209 | 34 |
| Write 1KB | 68 | 62 |
| Read 100B | 114 | 33 |
| Read 1KB | 204 | 32 |

**E2B is faster** for file operations. OpenSandbox uses shell commands (base64 encoding) for file transfers while E2B has native file APIs.

### 4. Realistic Workflow (Git Clone + Edit)

| Step | OpenSandbox (ms) | E2B (ms) |
|------|------------------|----------|
| sandbox_create | 122 | 125 |
| mkdir | 220 | 130 |
| git_clone | 374 | 648 |
| list_files | 69 | 43 |
| write_file | 211 | 34 |
| read_readme | 88 | 38 |
| git_status | 67 | 38 |
| git_diff | 73 | 60 |
| cleanup | 95 | 45 |
| sandbox_destroy | 201 | 107 |
| **Total** | **1529** | **1268** |

Mixed results. **OpenSandbox is faster for git clone** (374ms vs 648ms), but E2B wins on file operations and overall workflow time.

### 5. Concurrency (Parallel Sandboxes)

| Concurrency | OpenSandbox Wall Time | OpenSandbox Throughput/s | E2B Wall Time | E2B Throughput/s |
|-------------|----------------------|--------------------------|---------------|------------------|
| 1 | 505ms | 1.98 | 483ms | 2.07 |
| 2 | 408ms | 4.90 | 652ms | 3.07 |
| 4 | 460ms | 8.69 | 584ms | 6.85 |
| 8 | 537ms | **14.90** | 3139ms | 2.55 |

**OpenSandbox scales much better** with concurrent workloads:
- At 8 concurrent sandboxes, OpenSandbox maintains ~537ms wall time while E2B degrades to 3.1 seconds
- OpenSandbox achieves **14.9 sandboxes/sec** throughput vs E2B's **2.55 sandboxes/sec** at high concurrency
- E2B appears to hit rate limiting or queuing at 8 concurrent requests

### Key Takeaways

| Metric | Winner | Notes |
|--------|--------|-------|
| Sandbox creation | OpenSandbox | 2x faster startup |
| Command execution | E2B | 2x lower latency per command |
| File operations | E2B | Native SDK vs HTTP+shell |
| Git clone | OpenSandbox | Faster network/clone performance |
| Concurrency | OpenSandbox | 6x better throughput at scale |
| Total workflow | E2B | 17% faster end-to-end |

**Choose OpenSandbox when:**
- Running many sandboxes in parallel (agent workloads)
- Git operations are the bottleneck
- Self-hosting is preferred

**Choose E2B when:**
- Single-sandbox workflows
- Many small file operations
- Minimal infrastructure management desired

> **Note:** These benchmarks compare E2B's native Python SDK against OpenSandbox's HTTP API. The command execution and file operation differences are largely due to SDK vs HTTP overhead. A native OpenSandbox SDK is planned, which should significantly reduce per-operation latency and provide a more direct comparison. Results will be updated once the SDK is available.

---

## Prerequisites

### 1. OpenSandbox (Local)
Must be running locally:
```bash
# From the repository root
docker compose up --build
```

### 2. E2B (Cloud)
Requires an E2B API key:
```bash
export E2B_API_KEY=your_api_key
```

### 3. Python Dependencies
```bash
cd benchmarks
pip install -r requirements.txt
```

## Quick Test

Before running full benchmarks, verify connectivity:

```bash
# Test OpenSandbox
python quick_test.py

# Test E2B
python quick_test.py --provider e2b

# Test both
python quick_test.py --provider all
```

## Running Benchmarks

### Full Suite
```bash
# Run all benchmarks for all available providers
python run_benchmarks.py

# Run with more iterations for better accuracy
python run_benchmarks.py --iterations 5
```

### Provider-Specific
```bash
# Only test OpenSandbox
python run_benchmarks.py --provider opensandbox

# Only test E2B
python run_benchmarks.py --provider e2b
```

### Individual Benchmarks
```bash
# Run specific benchmarks only
python run_benchmarks.py --only creation
python run_benchmarks.py --only creation commands
python run_benchmarks.py --only workflow

# Or run them directly
python benchmark_creation.py --provider opensandbox --iterations 5
python benchmark_commands.py --provider e2b --iterations 3
python benchmark_files.py --provider opensandbox
python benchmark_workflow.py --provider opensandbox
python benchmark_concurrency.py --provider opensandbox
```

## Benchmark Categories

### 1. Creation (`benchmark_creation.py`)
Measures sandbox startup and teardown time.
- **Create time**: How long to spin up a new sandbox session
- **Destroy time**: How long to tear down and cleanup

### 2. Commands (`benchmark_commands.py`)
Measures command execution latency for various operations:
- Simple commands (echo, pwd, ls)
- System info (uname, env)
- Tool availability (git, python versions)
- Loops and calculations

### 3. Files (`benchmark_files.py`)
Measures file read/write performance across different sizes:
- Tiny: 100 bytes
- Small: 1 KB
- Medium: 10 KB
- Large: 100 KB
- XLarge: 1 MB

### 4. Workflow (`benchmark_workflow.py`)
Measures a realistic git workflow:
1. Create sandbox
2. Git clone (shallow)
3. List files
4. Write a new file
5. Read README
6. Git status/diff
7. Cleanup and destroy

### 5. Concurrency (`benchmark_concurrency.py`)
Measures parallel sandbox operations:
- Tests 1, 2, 4, 8 concurrent sandboxes
- Measures wall clock time and throughput
- Identifies bottlenecks in parallel workloads

## Output

Results are saved to the `results/` directory:
- `results/benchmark_YYYYMMDD_HHMMSS.json` - Raw JSON results
- `results/benchmark_YYYYMMDD_HHMMSS.md` - Formatted markdown report

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `OPENSANDBOX_URL` | `https://opensandbox-test.fly.dev` | OpenSandbox server URL |
| `E2B_API_KEY` | - | E2B API key (required for E2B tests) |

## Example Output

```
============================================================
SUMMARY
============================================================

## 1. Sandbox Creation Time

| Provider | Create Avg (ms) | Create Std | Destroy Avg (ms) |
|----------|-----------------|------------|------------------|
| opensandbox | 45.23 | 5.12 | 12.45 |
| e2b | 2345.67 | 234.56 | 156.78 |

## 2. Command Execution Latency

| Command | opensandbox (ms) | e2b (ms) |
|---------|------------------|----------|
| echo | 15.23 | 89.45 |
| pwd | 14.56 | 87.23 |
| ls | 18.34 | 95.67 |
...
```

## Notes

- OpenSandbox benchmarks require Docker with `--privileged` mode
- E2B benchmarks require internet connectivity and valid API key
- File operations on OpenSandbox use commands (cat, base64) while E2B uses native SDK
- Concurrency tests may hit rate limits on E2B free tier
