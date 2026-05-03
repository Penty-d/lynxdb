# Format Upgrade Runbook

Use `lynxdb admin format-upgrade` to ratchet the data-directory `FORMAT` marker after installing a binary that supports the target format major.

```bash
lynxdb admin format-upgrade --data-dir /var/lib/lynxdb --to 1 --confirm
```

At v1 launch, `--to 1` is a no-op when the directory is already at v1. A target outside the binary-supported range exits with code 78.

Before running a future major upgrade, stop all LynxDB processes using the data directory, verify no compaction or ingest jobs are active, back up the directory, then run the command with `--confirm`.

