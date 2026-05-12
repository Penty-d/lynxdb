---
title: regex
description: Filter rows with a regular expression.
---

# regex

Filter rows by matching a regular expression against `_raw` or a selected field.

## Syntax

```spl
| regex "<pattern>"
| regex <field>="<pattern>"
| regex <field>!="<pattern>"
```

When the field is omitted, `_raw` is used.

## Examples

```spl
-- Keep raw events containing either term
| regex "timeout|fatal"

-- Keep rows where message starts with error
| regex message="^error"

-- Keep rows where message is missing or does not start with debug
| regex message!="^debug"
```

## Notes

- `regex` filters rows; it does not extract fields. Use `rex` or `parse regex(...)` for extraction.
- `field!="pattern"` keeps rows where the field does not match and rows where the field is null.
- The default engine is Go's linear-time `regexp` engine. PCRE2-only features are deferred.

## See Also

- [rex](/docs/lynx-flow/commands/rex) -- Extract fields with regex
- [where](/docs/lynx-flow/commands/where) -- Filter with expressions
