---
title: replace
description: Replace exact or wildcard field values.
---

# replace

Replace field values using exact matches or `*` wildcard matches.

## Syntax

```spl
| replace <old> WITH <new>
| replace <old> WITH <new>, <old2> WITH <new2>
| replace <old> WITH <new> IN <field> [<field2> ...]
```

## Examples

```spl
-- Replace a value in all non-internal fields
| replace *localhost WITH localhost

-- Replace values in a specific field
| replace 0 WITH Critical, 1 WITH Error IN msg_level

-- Move the wildcard capture in the replacement
| replace "* localhost" WITH "localhost *" IN host

-- Replace an internal field by naming it explicitly
| replace *XYZ* WITH *ALL* IN _time
```

## Notes

- Without `IN`, internal fields such as `_raw` and `_time` are not modified.
- A replacement can use `*` captures from the old value. If the replacement contains `*`, it must contain the same number of wildcards as the old value.
- Values are compared as strings. Replaced values are written as strings.

## See Also

- [eval](/docs/lynx-flow/commands/eval) -- Use `replace(field, regex, value)` for regex string replacement
- [regex](/docs/lynx-flow/commands/regex) -- Filter rows by regular expression
