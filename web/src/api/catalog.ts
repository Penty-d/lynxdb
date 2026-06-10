/**
 * Client for GET /api/v1/catalog — the LynxFlow v2 language surface.
 *
 * Fetches the catalog once per session (the registry is frozen at compile time
 * and the server returns ETag + Cache-Control headers). Falls back to a minimal
 * hardcoded operator list when the server is unreachable (e.g. pipe mode, stale
 * embed, network error).
 */

import { authHeaders, handleAuthError } from "./auth";

// ---------------------------------------------------------------------------
// Types — mirrors the JSON shape from pkg/api/rest/catalog.go
// ---------------------------------------------------------------------------

export interface CatalogPositional {
  name: string;
  type: string;
  required: boolean;
  variadic: boolean;
  doc: string;
}

export interface CatalogOption {
  name: string;
  type: string;
  required: boolean;
  default: string;
  enum?: string[];
  doc: string;
}

export interface CatalogOperator {
  name: string;
  class: string;
  streaming: string;
  positionals?: CatalogPositional[];
  options?: CatalogOption[];
  desugars_to?: string;
  doc: string;
  examples?: string[];
}

export interface CatalogFunction {
  name: string;
  category: string;
  params?: CatalogParam[];
  result: string;
  fallibility: string;
  strict_variant?: boolean;
  doc: string;
}

export interface CatalogAggregate {
  name: string;
  params?: CatalogParam[];
  supports_where?: boolean;
  window_only?: boolean;
  result: string;
  doc: string;
}

export interface CatalogParam {
  name: string;
  type: string;
  optional: boolean;
  variadic: boolean;
}

export interface Catalog {
  operators: CatalogOperator[];
  functions: CatalogFunction[];
  aggregates: CatalogAggregate[];
  parse_formats: string[];
}

// ---------------------------------------------------------------------------
// Minimal fallback — operator names only, so completion works offline
// ---------------------------------------------------------------------------

const FALLBACK_OPERATOR_NAMES = [
  "from",
  "search",
  "where",
  "stats",
  "eval",
  "sort",
  "head",
  "tail",
  "fields",
  "table",
  "dedup",
  "rename",
  "rex",
  "bin",
  "timechart",
  "top",
  "rare",
  "streamstats",
  "eventstats",
  "join",
  "append",
  "multisearch",
  "transaction",
  "xyseries",
  "fillnull",
  "parse",
  "keep",
  "omit",
  "let",
  "group",
  "order",
  "take",
  "unroll",
  "pack",
  "explode",
  "lookup",
];

const FALLBACK_CATALOG: Catalog = {
  operators: FALLBACK_OPERATOR_NAMES.map((name) => ({
    name,
    class: "unknown",
    streaming: "unknown",
    doc: "",
  })),
  functions: [],
  aggregates: [],
  parse_formats: [],
};

// ---------------------------------------------------------------------------
// Fetch with memoization
// ---------------------------------------------------------------------------

let cached: Catalog | null = null;
let inflight: Promise<Catalog> | null = null;

async function doFetch(): Promise<Catalog> {
  const resp = await fetch("/api/v1/catalog", {
    headers: authHeaders(),
  });
  handleAuthError(resp);

  if (!resp.ok) {
    throw new Error(`catalog fetch failed: ${resp.status}`);
  }

  const data: Catalog = await resp.json();
  return data;
}

/**
 * Return the language catalog, fetching it at most once. On failure the
 * hardcoded fallback is returned and no further fetches are attempted for the
 * current page lifetime (the catalog is immutable per server binary).
 */
export async function fetchCatalog(): Promise<Catalog> {
  if (cached) return cached;

  if (!inflight) {
    inflight = doFetch().then(
      (data) => {
        cached = data;
        inflight = null;
        return data;
      },
      () => {
        // Network error / server down — use the fallback and don't retry.
        cached = FALLBACK_CATALOG;
        inflight = null;
        return FALLBACK_CATALOG;
      },
    );
  }

  return inflight;
}

// ---------------------------------------------------------------------------
// Test helpers — allow tests to inject/reset the cache
// ---------------------------------------------------------------------------

/** @internal — for tests only */
export function _resetCatalogCache(): void {
  cached = null;
  inflight = null;
}

/** @internal — for tests only */
export function _setCatalogCache(catalog: Catalog): void {
  cached = catalog;
  inflight = null;
}

/** @internal — expose fallback for test assertions */
export const _FALLBACK_CATALOG = FALLBACK_CATALOG;
