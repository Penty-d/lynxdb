---
title: outliers
description: Mark numeric values that are unusual relative to the input set.
---

# outliers

Mark rows whose numeric field is unusual relative to the input set. Use
`outliers` after parsing or auto-detection has made the target field available.

## Syntax

```spl
| outliers field=<field> [method=iqr|zscore|mad] [threshold=<number>]
```

## Parameters

| Parameter | Default | Description |
|-----------|---------|-------------|
| `field` | required | Numeric field to score |
| `method` | `iqr` | Scoring method: interquartile range, z-score, or median absolute deviation |
| `threshold` | `1.5` for `iqr`, `3.0` for `zscore` | Method-specific cutoff |

## Output fields

| Field | Description |
|-------|-------------|
| `_outlier` | Boolean marker |
| `_score` | Distance score produced by the selected method |

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

Find high-latency outliers:

```bash
lynxdb query --file latency.log \
  '| outliers field=duration_ms method=zscore threshold=1 | where _outlier=true | table duration_ms, _score' \
  --format ndjson --no-stats
```

Expected output:

```json
{"_score":1.4555143280385763,"duration_ms":"900"}
```

## Notes

- `outliers` does not remove rows. Filter on `_outlier=true` when you only want
  unusual values.
- `zscore` is useful when values are roughly normally distributed.
- `iqr` and `mad` are less sensitive to extreme values.

## See Also

- [where](/docs/lynx-flow/commands/where) -- Filter rows
- [stats](/docs/lynx-flow/commands/stats) -- Summarize numeric fields
