---
sidebar_position: 3
title: Segment Format
description: The columnar .lsg v1 segment format.
---

# Segment Format (`.lsg`)

LynxDB stores immutable columnar segments in the `.lsg` format. The current format-major is v1 and every v1 file starts with the `LSG1` magic.

## Layout

| Region | Magic | Purpose |
|---|---|---|
| Header | `LSG1` | 24-byte file header with major version and capability summaries |
| Column chunks | none | Encoded column data referenced from the footer |
| Bloom region | `LSBL` | Per-row-group, per-column bloom filters |
| Inverted index | `LSIX` | FST term dictionary plus roaring bitmap postings |
| Primary index | `LSPK` | Optional sparse sort-key index for materialized-view segments |
| Footer | `LSGE` | Event count, row-group metadata, catalog, region offsets |

The footer trailer is 12 bytes: footer payload length, capability summary, and CRC32-IEEE over the footer payload plus trailer fields up to the CRC.

## Versioning

The data-directory root contains a `FORMAT` marker:

```text
LSGFMT v1
```

On boot, LynxDB validates the marker before loading segments. Unknown majors, unknown required capability bits, invalid magic bytes, and marker mismatches refuse startup. Physical corruption such as footer checksum failure remains a per-file warning so the rest of the directory can still load.

## Capabilities

Format-major v1 defines one required capability bit:

| Bit | Name | Meaning |
|---|---|---|
| 0 | `ColumnZSTD` | At least one column chunk uses ZSTD layer-2 compression |

See the canonical storage-format document in `docs/storage-format.md` for the compatibility table, capability registry, and runbooks.

