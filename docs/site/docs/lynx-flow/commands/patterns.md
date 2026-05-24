---
title: patterns
description: Group similar raw messages into reusable message patterns.
---

# patterns

Summarize repeated message shapes. Use `patterns` when you need to inspect a
new log source and identify common raw message forms before writing parsers or
filters.

## Syntax

```spl
| patterns [field=<field>] [max_templates=<n>] [similarity=<0.0-1.0>]
```

## Parameters

| Parameter | Default | Description |
|-----------|---------|-------------|
| `field` | `_raw` | Field to group into patterns |
| `max_templates` | `50` | Maximum number of templates to keep |
| `similarity` | `0.4` | Similarity threshold for grouping messages |

## Output fields

| Field | Description |
|-------|-------------|
| `pattern` | Representative pattern text |
| `count` | Number of rows assigned to the pattern |
| `percent` | Percentage of input rows assigned to the pattern |
| `example` | Example input row for the pattern |

## Example

Create a logfmt sample:

```bash
cat > latency.log <<'EOF'
level=info service=api duration_ms=15 message=ok
level=error service=api duration_ms=120 message=failed
level=error service=worker duration_ms=250 message=timeout
level=error service=worker duration_ms=900 message=timeout
EOF
```

List the first three raw message patterns:

```bash
lynxdb query --file latency.log \
  '| patterns max_templates=3 | table pattern, count | sort pattern' \
  --format ndjson --no-stats
```

Expected output:

```json
{"count":1,"pattern":"level=error service=api duration_ms=120 message=failed"}
{"count":1,"pattern":"level=error service=worker duration_ms=250 message=timeout"}
{"count":1,"pattern":"level=info service=api duration_ms=15 message=ok"}
```

## Notes

- `patterns` is an exploration command. It is useful before committing a
  parser, saved query, or dashboard.
- Raise `similarity` to split templates more aggressively. Lower it to group
  more messages together.
- Add `| sort -count` when you want the most frequent patterns first.

## See Also

- [rex](/docs/lynx-flow/commands/rex) -- Extract fields with regex
- [unpack-pattern](/docs/lynx-flow/commands/unpack-pattern) -- Parse a custom pattern
