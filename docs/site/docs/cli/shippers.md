---
sidebar_position: 12
title: shippers
description: Inspect, configure, and test log shipper integrations.
---

# shippers

Manage log shipper configurations for Filebeat, Fluent Bit, Vector, OpenTelemetry Collector, and Splunk HEC.

## Subcommands

| Subcommand | Description |
|------------|-------------|
| `config <tool>` | Print a copy-pasteable shipper config |
| `test <tool>` | Send one synthetic event through a shipper-compatible endpoint |

With no subcommand, `shippers` lists registered shippers from the server.

## List Shippers

```bash
lynxdb shippers
```

Queries the server for known shipper integrations and prints them as a table. Use `--format json` for machine-readable output.

## Generate Shipper Config

```bash
lynxdb shippers config filebeat --remote http://lynxdb:3100
lynxdb shippers config fluent-bit --remote http://lynxdb:3100
lynxdb shippers config vector --remote http://lynxdb:3100
lynxdb shippers config otelcol --remote http://lynxdb:3100
lynxdb shippers config splunk-hec --remote http://lynxdb:3100
```

Prints a ready-to-use config file with the LynxDB endpoint baked in. Redirect to a file or copy-paste into your shipper config directory.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--remote` | | LynxDB endpoint URL to render into the config |

## Test Shipper Connectivity

```bash
lynxdb shippers test filebeat --remote http://lynxdb:3100
lynxdb shippers test splunk-hec --remote http://lynxdb:3100
```

Sends a single synthetic event through the shipper-compatible endpoint and reports success or failure. Useful for verifying that a shipper can reach the LynxDB ingest API.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--remote` | | LynxDB endpoint URL to test |

## Supported Tools

| Tool | Config Format | Ingest Endpoint |
|------|--------------|-----------------|
| `filebeat` | YAML | Elasticsearch `_bulk` compatibility |
| `fluent-bit` | INI | HTTP JSON ingest |
| `vector` | YAML | HTTP JSON ingest |
| `otelcol` | YAML | OTLP HTTP (`/api/v1/otlp/v1/logs`) |
| `splunk-hec` | Text | Splunk HEC (`/services/collector/event`) |

## Related

- [Ingest Data](/docs/guides/ingest-data)
- [Compatibility API](/docs/api/compatibility)
- [ingest & import](/docs/cli/ingest)
