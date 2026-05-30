import { create } from "zustand";
import type {
  QueryResult,
  QueryStats,
  IndexInfo,
  ViewSummary,
  ExplainResult,
  HistogramBucket,
  HistogramBucketGrouped,
  FieldInfo,
} from "../api/client";
import { fetchFields } from "../api/client";
import { FIELDS_CACHE_TTL_MS } from "../api/config";
import type { TailEvent } from "../api/sse";

interface SearchState {
  query: string;
  from: string;
  to: string | undefined;
  result: QueryResult | null;
  stats: QueryStats | null;
  loading: boolean;
  error: string | null;

  // Part 3: sidebar & timeline
  sidebarVisible: boolean;
  timelineBuckets: HistogramBucket[];
  groupedBuckets: HistogramBucketGrouped[];
  histogramBrushed: boolean;
  hasQueried: boolean;

  // Flow sidebar
  sidebarIndexes: IndexInfo[];
  sidebarViews: ViewSummary[];
  explainResult: ExplainResult | null;
  fieldTypeMap: Map<string, string>;
  catalogFields: FieldInfo[];
  /** Epoch ms of the last field-catalog fetch; null = never fetched. Used to
   *  skip redundant per-query catalog refetches within FIELDS_CACHE_TTL_MS. */
  catalogFetchedAt: number | null;

  // Part 4: Live Tail
  tailActive: boolean;
  tailEvents: TailEvent[];
  tailNewCount: number;
  tailCatchupDone: boolean;
  tailReconnecting: boolean;

  // Explain inspector
  explainOpen: boolean;

  // Streaming & Progress
  queryActive: boolean;
  streaming: boolean;
  streamingCount: number;
  progressData: {
    percent: number;
    scanned: number;
    total: number;
    elapsedMs: number;
  } | null;
  canceled: boolean;
  elapsedMs: number;
  isPreview: boolean;

  // Pagination, view mode, toolbar
  page: number;
  pageSize: number;
  viewMode: "table" | "list";
  copyTooltip: { visible: boolean; x: number; y: number };
}

export const useSearchStore = create<SearchState>(() => ({
  query: "",
  from: "-1h",
  to: undefined,
  result: null,
  stats: null,
  loading: false,
  error: null,

  sidebarVisible: true,
  timelineBuckets: [],
  groupedBuckets: [],
  histogramBrushed: false,
  hasQueried: false,

  sidebarIndexes: [],
  sidebarViews: [],
  explainResult: null,
  fieldTypeMap: new Map(),
  catalogFields: [],
  catalogFetchedAt: null,

  tailActive: false,
  tailEvents: [],
  tailNewCount: 0,
  tailCatchupDone: false,
  tailReconnecting: false,

  explainOpen: false,

  queryActive: false,
  streaming: false,
  streamingCount: 0,
  progressData: null,
  canceled: false,
  elapsedMs: 0,
  isPreview: false,

  page: 1,
  pageSize: 100,
  viewMode: "table",
  copyTooltip: { visible: false, x: 0, y: 0 },
}));

/** In-flight catalog fetch, shared so concurrent callers coalesce to one request. */
let fieldsInFlight: Promise<void> | null = null;

/**
 * Load the field catalog into the store, reusing a recently-fetched catalog
 * instead of re-fetching on every query. Pass `force` to bypass the TTL (e.g.
 * an explicit refresh). Errors are swallowed — the catalog is non-critical.
 */
export function ensureFieldsLoaded(force = false): Promise<void> {
  const { catalogFetchedAt } = useSearchStore.getState();
  const fresh =
    catalogFetchedAt !== null &&
    Date.now() - catalogFetchedAt < FIELDS_CACHE_TTL_MS;
  if (!force && fresh) return Promise.resolve();
  if (fieldsInFlight) return fieldsInFlight;

  fieldsInFlight = fetchFields()
    .then((fields) => {
      const typeMap = new Map<string, string>();
      for (const f of fields) typeMap.set(f.name, f.type);
      useSearchStore.setState({
        catalogFields: fields,
        fieldTypeMap: typeMap,
        catalogFetchedAt: Date.now(),
      });
    })
    .catch(() => {
      /* non-critical: leave the previous catalog in place */
    })
    .finally(() => {
      fieldsInFlight = null;
    });

  return fieldsInFlight;
}
