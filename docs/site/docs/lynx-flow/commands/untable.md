---
title: untable
description: Convert wide tabular rows into name and value rows.
---

# untable

Convert tabular rows into individual name/value rows. This is the inverse shape of `xyseries`.

## Syntax

```spl
| untable <x-field> <y-name-field> <y-data-field>
```

Every input field except `<x-field>` becomes one output row. The output row contains:

- `<x-field>` with the original row label value
- `<y-name-field>` with the original field name
- `<y-data-field>` with the original field value

## Examples

```spl
-- Convert count and percent columns into metric/value rows
| top categoryId
| untable categoryId metric value

-- Prepare host metrics for charting
| stats avg(cpu) as cpu avg(memory) as memory by host
| untable host metric value
```

## Notes

- The output column order is `<x-field>`, `<y-name-field>`, `<y-data-field>`.
- Missing or null input values are preserved as null values.

## See Also

- [xyseries](/docs/lynx-flow/commands/xyseries) -- Pivot name/value rows into columns
- [table](/docs/lynx-flow/commands/table) -- Select fields before unpivoting
