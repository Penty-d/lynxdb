# LSG Storage Format

## 1. Compatibility Table

| LynxDB binary | Reads majors | Writes major | Marker accepts | Notes |
|---|---|---|---|---|
| v1.0.0 | 1 | 1 | 1 | Initial release |

## 2. Magic Bytes Registry

| Magic | Role |
|---|---|
| `LSG1` | Segment file magic for format-major v1 |
| `LSGE` | Footer payload magic |
| `LSIX` | Inverted-index region magic |
| `LSPK` | Sparse primary-index region magic |
| `LSBL` | Per-row-group bloom region magic |

## 3. Major Version Table

| Major | Features |
|---|---|
| 1 | 24-byte `LSG1` header, column chunks, const columns, per-row-group `LSBL` blooms, `LSIX` inverted index, optional `LSPK` primary index, `LSGE` footer, 12-byte trailer |

## 4. Capability Bits Table

| Bit | Name | Class | Description |
|---|---|---|---|
| 0 | `ColumnZSTD` | required | A column chunk uses ZSTD layer-2 compression. |

## 5. Upgrade Runbook

See [format-upgrade-runbook.md](operator/format-upgrade-runbook.md).

## 6. Downgrade Runbook

See [format-downgrade-runbook.md](operator/format-downgrade-runbook.md).

## 7. Deprecation Policy

A LynxDB binary at format-major `N` reads `{N-2, N-1, N}` and writes `N`. Major bumps target at most one per 12 calendar months. A binary refuses to start when a data directory marker is below `LSG_BINARY_MIN_MAJOR` or above `LSG_BINARY_MAX_MAJOR`.

