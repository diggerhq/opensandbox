# Integration tests for fork/hibernate/wake fixes

One test per fix in PR #128. Each test is self-contained, creates its own
resources, cleans up in finally, and exits non-zero on regression.

| # | Test file                                 | Validates                                                                                   | Commit  |
| - | ----------------------------------------- | ------------------------------------------------------------------------------------------- | ------- |
| 1 | `01-fork-sync-hibernate.ts`               | `createFromCheckpoint` blocks until worker has the VM registered (no "sandbox not found")  | 08d5320 |
| 2 | `02-fork-no-corruption.ts`                | Forks from savevm-based checkpoints have correct workspace state (no EBADMSG / git segv)   | 1caf148 + b2aa416 |
| 3 | `03-hibernate-wake-routing.ts`            | Data-plane requests route to the current worker after wake (no "auto-wake failed")         | f4e3600 |
| 4 | `04-hibernate-wake-data-preserved.ts`     | Wake does not shadow `/home/sandbox` with an empty mount; all files are readable post-wake | 9f550e2 |

## Running

```
OPENCOMPUTER_API_URL=... OPENCOMPUTER_API_KEY=... \
  npx tsx scripts/integration-tests/01-fork-sync-hibernate.ts
```

Each script exits 0 on success, 1 on any failure.
