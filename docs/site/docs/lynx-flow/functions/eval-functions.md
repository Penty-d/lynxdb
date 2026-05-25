---
title: Eval Functions
description: Complete reference for eval functions -- conditionals, type checks, string, math, time, network, hash, and multivalue.
---

# Eval Functions

Eval functions are used in `let`, [`eval`](/docs/lynx-flow/commands/eval), [`where`](/docs/lynx-flow/commands/where), and any expression context.

Function names are case-insensitive. Arguments accept any expression unless noted otherwise.

## Conditional

### IF

```spl
| eval severity = if(status >= 500, "critical", "ok")
| eval label    = if(duration_ms > 1000, "slow", if(duration_ms > 100, "normal", "fast"))
```

### CASE

Multi-way conditional. Pairs of `condition, value`. The first true condition wins. The final condition is typically `1=1` as the default.

```spl
| eval tier = case(
    duration_ms < 100,  "fast",
    duration_ms < 1000, "normal",
    duration_ms < 5000, "slow",
    1=1,                "very_slow"
  )
```

### validate

Inverse of `case`: pairs are `must_be_true, error_value`. Returns the error value paired with the first condition that is **false**, or `null` when every condition is true. Useful for accumulating validation errors.

```spl
| eval err = validate(
    status >= 200, "status too low",
    status < 600, "status too high"
  )
```

### coalesce

Returns the first non-null argument.

```spl
| eval name = coalesce(display_name, username, email, "anonymous")
```

### nullif

Returns `null` if the two arguments are equal, otherwise the first argument.

```spl
| eval clean = nullif(host, "unknown")
```

### searchmatch

Returns true when the current event matches a search expression (the same syntax accepted by [`search`](/docs/lynx-flow/commands/search)).

```spl
| eval is_5xx = searchmatch("status>=500 source=nginx")
```

## Null and Type Tests

| Function | Returns |
|----------|---------|
| `isnull(x)` | true if `x` is null |
| `isnotnull(x)` | true if `x` is not null |
| `isnum(x)` / `isnumeric(x)` | true if `x` is a number (int or float) |
| `isint(x)` | true if `x` is an integer |
| `isbool(x)` | true if `x` is a boolean |
| `isarray(x)` | true if `x` is a JSON array |
| `isobject(x)` | true if `x` is a JSON object |
| `isstr(x)` | true if `x` is a non-null value (in schema-on-read, everything stored is a string) |
| `typeof(x)` | type name: `"null"`, `"int"`, `"float"`, `"bool"`, `"string"`, `"array"`, `"object"` |

```spl
| where isnotnull(error_message)
| eval kind = typeof(payload)
```

## Type Conversion

| Function | Description |
|----------|-------------|
| `tonumber(s)` / `todouble(s)` | Parse string to float. Returns null on failure. |
| `toint(s)` | Parse string to integer. Returns null on failure. |
| `tostring(x)` | Convert any value to string. |
| `tobool(x)` | Convert to boolean (`"true"`/`"false"`, `0`/`1`, etc.). Returns null on failure. |

```spl
| eval status_num = tonumber(status_str)
| eval enabled    = tobool(flag)
```

## String

### Case

```spl
| eval level_upper = upper(level)
| eval host_lower  = lower(host)
```

### Length

```spl
| eval msg_length = len(message)
| where len(uri) > 100
```

### Substring

`substr(s, start [, length])`. `start` is 1-indexed. With no `length`, returns the rest of the string.

```spl
| eval prefix = substr(uri, 1, 4)
| eval rest   = substr(host, 5)
```

### Trimming

`trim(s [, chars])`, `ltrim(s [, chars])`, `rtrim(s [, chars])`. Without `chars`, strips whitespace (` \t\r\n`).

```spl
| eval cleaned = trim(message)
| eval no_prefix = ltrim(path, "/")
```

### Matching

| Function | Description |
|----------|-------------|
| `match(s, regex)` | True if the regex matches anywhere in `s`. Pattern must be a literal; use `^`/`$` to anchor. |
| `startswith(s, prefix)` | True if `s` begins with `prefix`. |
| `endswith(s, suffix)` | True if `s` ends with `suffix`. |
| `contains(s, substr)` | True if `s` contains `substr`. |

```spl
| where match(uri, "^/api/v[0-9]+/users")
| eval is_health = startswith(uri, "/health")
```

### Replacement

`replace(s, regex, replacement)`. Pattern must be a literal; supports capture groups via `\1`, `\2`, etc.

```spl
| eval anonymized = replace(message, "user_[0-9]+", "user_***")
```

### Splitting

`split(s, delim)` returns a multivalue field.

```spl
| eval parts = split(path, "/")
| eval first = mvjoin(mvdedup(parts), ",")
```

### URL Decoding

```spl
| eval url = urldecode(encoded)
```

### printf

`printf(format, args...)`. Go-style format string.

```spl
| eval line = printf("%s %d %s", method, status, uri)
```

## Math

| Function | Description |
|----------|-------------|
| `round(n [, d])` | Round to `d` decimal places. Default `d = 0`. |
| `abs(n)` | Absolute value |
| `ceil(n)` / `ceiling(n)` | Smallest integer not less than `n` |
| `floor(n)` | Largest integer not greater than `n` |
| `sqrt(n)` | Square root |
| `ln(n)` | Natural logarithm |
| `log(n [, base])` | Logarithm. Default base 10. |
| `exp(n)` | `e^n` |
| `pow(x, y)` | `x^y` |
| `max(a, b, ...)` | Largest of two or more arguments |
| `min(a, b, ...)` | Smallest of two or more arguments |
| `pi()` | Constant pi |
| `random()` | Pseudo-random float in `[0, 1)` |

```spl
| eval rate = round(errors / total * 100, 1)
| eval log_dur = log(duration_ms, 2)
| eval worst   = max(t1, t2, t3)
```

### Trigonometric

Single-argument: `sin`, `cos`, `tan`, `asin`, `acos`, `atan`, `sinh`, `cosh`, `tanh`, `asinh`, `acosh`, `atanh`. Two-argument: `atan2(y, x)`, `hypot(x, y)`. Inputs and outputs are in radians.

## Time

### strftime

Format a timestamp.

```spl
| eval formatted = strftime(_time, "%Y-%m-%d %H:%M:%S")
| eval hour      = strftime(_time, "%H")
```

Common format specifiers:

| Specifier | Description | Example |
|-----------|-------------|---------|
| `%Y` | 4-digit year | `2026` |
| `%m` | Month (01-12) | `03` |
| `%d` | Day (01-31) | `15` |
| `%H` | Hour (00-23) | `14` |
| `%M` | Minute (00-59) | `30` |
| `%S` | Second (00-59) | `45` |
| `%A` | Weekday name | `Monday` |

### strptime

Parse a string into a timestamp.

```spl
| eval ts = strptime("2026-01-15 14:30:00", "%Y-%m-%d %H:%M:%S")
```

## Network

### cidrmatch

Test whether an IP lies inside a CIDR block. The CIDR must be a literal string.

```spl
| where cidrmatch("10.0.0.0/8", client_ip)
| eval is_internal = cidrmatch("192.168.0.0/16", client_ip)
```

### ipmask

Apply a network mask to an IP address.

```spl
| eval subnet = ipmask(client_ip, "255.255.255.0")
```

## Hash

`md5(s)`, `sha1(s)`, `sha256(s)`, `sha512(s)` return lowercase hex digests.

```spl
| eval fingerprint = sha256(_raw)
```

## Multivalue

| Function | Description |
|----------|-------------|
| `mvcount(mv)` | Number of values in a multivalue field |
| `mvjoin(mv, sep)` | Join values into a single string |
| `mvappend(a, b, ...)` | Build a multivalue from arguments |
| `mvdedup(mv)` | Remove duplicate values |

```spl
| eval all_levels = mvjoin(values(level), ", ")
| eval tags       = mvdedup(mvappend(source, level))
```

## String Concatenation

Use the `.` operator. The right-hand side is coerced to string when needed.

```spl
| eval url      = "https://" . host . uri
| eval full_msg = source . ": " . message
```

## See Also

- [Aggregation Functions](/docs/lynx-flow/functions/aggregation-functions)
- [JSON Functions](/docs/lynx-flow/functions/json-functions)
- [Search Syntax](/docs/lynx-flow/search-syntax)
- [eval command](/docs/lynx-flow/commands/eval)
- [where command](/docs/lynx-flow/commands/where)
