# Format Downgrade Runbook

Downgrading a ratcheted data directory in place is not supported.

For v1, there is no older supported `.lsg` format. Rollback means stopping LynxDB, removing the v1 data directory, installing the older binary, and re-ingesting from the original log source or restoring a backup produced by that older binary.

