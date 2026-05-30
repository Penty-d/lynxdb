/**
 * Streaming & async query API layer.
 *
 * Provides four functions for adaptive query execution:
 *   - submitHybridQuery() -- POST /api/v1/query with wait:0.2 (sync or async)
 *   - streamQuery()       -- POST /api/v1/query/stream (NDJSON stream consumer)
 *   - subscribeJobProgress() -- GET /api/v1/query/jobs/{id}/stream (SSE)
 *   - cancelJob()         -- DELETE /api/v1/query/jobs/{id}
 */

import type { QueryResult, QueryStats } from "./client";
import { authHeaders, handleAuthError, useAuthStore } from "./auth";
import {
  HYBRID_WAIT_SECONDS,
  JOB_SSE_MAX_RECONNECTS,
  JOB_SSE_BACKOFF_BASE_MS,
  JOB_SSE_BACKOFF_MAX_MS,
  JOB_SSE_OVERALL_DEADLINE_MS,
} from "./config";

// Types

export interface HybridResult {
  status: "sync" | "async";
  /** Present when status is "sync" (200 response). */
  syncResult?: { result: QueryResult; stats: QueryStats };
  /** Present when status is "async" (202 response). */
  jobId?: string;
}

export interface StreamCallbacks {
  onRow: (row: Record<string, unknown>) => void;
  onMeta: (meta: {
    total?: number;
    scanned?: number;
    took_ms?: number;
  }) => void;
  onError: (message: string) => void;
}

export interface ProgressData {
  phase: string;
  scanned: number;
  percent: number;
  events_matched: number;
  elapsed_ms: number;
  eta_ms?: number;
  /** Total number of segments to scan. Present in backend SearchProgress struct;
      used by Plan 02's onProgress handler to compute the 'total' for progress bar display. */
  segments_total?: number;
  /** Sample of matched rows emitted during pipeline execution. Present when
      new preview data is available since last progress event. */
  preview?: Record<string, unknown>[];
  /** Monotonically increasing counter. Frontend skips re-render if unchanged. */
  preview_version?: number;
}

// submitHybridQuery

/**
 * Submit a query with hybrid execution (wait up to `wait` seconds for a sync
 * result). Defaults to HYBRID_WAIT_SECONDS so most interactive queries return
 * inline (HTTP 200) and only genuinely long scans degrade to an async job.
 *
 * - On 200: returns `{ status: "sync", syncResult }`.
 * - On 202: returns `{ status: "async", jobId }`.
 */
export async function submitHybridQuery(
  q: string,
  from?: string,
  to?: string,
  limit?: number,
  offset?: number,
  signal?: AbortSignal,
  wait: number = HYBRID_WAIT_SECONDS,
): Promise<HybridResult> {
  const body: Record<string, unknown> = { q, wait };
  if (from) body.from = from;
  if (to) body.to = to;
  if (limit) body.limit = limit;
  if (offset) body.offset = offset;

  const resp = await fetch("/api/v1/query", {
    method: "POST",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: JSON.stringify(body),
    signal,
  });

  handleAuthError(resp);

  if (!resp.ok && resp.status !== 202) {
    const err = await resp
      .json()
      .catch(() => ({ error: { message: resp.statusText } }));
    throw new Error(err.error?.message || err.data?.error || resp.statusText);
  }

  const json = await resp.json();

  if (resp.status === 202) {
    return { status: "async", jobId: json.data?.job_id };
  }

  // 200 -- synchronous result
  return {
    status: "sync",
    syncResult: {
      result: json.data as QueryResult,
      stats: {
        took_ms: json.meta?.took_ms ?? 0,
        scanned: json.meta?.scanned ?? 0,
        query_id: json.meta?.query_id,
        stats: json.meta?.stats,
      },
    },
  };
}

// streamQuery

/**
 * Consume an NDJSON stream from /api/v1/query/stream.
 *
 * Handles partial line buffering (chunks may split across JSON boundaries).
 * Optionally stops after `maxRows` rows.
 */
export async function streamQuery(
  q: string,
  from?: string,
  to?: string,
  callbacks?: StreamCallbacks,
  signal?: AbortSignal,
  maxRows?: number,
): Promise<void> {
  if (!callbacks) return;

  const body: Record<string, unknown> = { q };
  if (from) body.from = from;
  if (to) body.to = to;

  const resp = await fetch("/api/v1/query/stream", {
    method: "POST",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: JSON.stringify(body),
    signal,
  });

  handleAuthError(resp);

  if (!resp.ok) {
    const err = await resp
      .json()
      .catch(() => ({ error: { message: resp.statusText } }));
    callbacks.onError(err.error?.message || err.data?.error || resp.statusText);
    return;
  }

  if (!resp.body) {
    callbacks.onError("Response body is not readable");
    return;
  }

  const reader = resp.body.pipeThrough(new TextDecoderStream()).getReader();
  let buffer = "";
  let rowCount = 0;

  try {
    for (;;) {
      const { done, value } = await reader.read();

      if (done) {
        // Process any remaining content in the buffer.
        if (buffer.trim()) {
          processLine(buffer, callbacks);
        }
        break;
      }

      buffer += value;
      const lines = buffer.split("\n");
      // Last element may be incomplete -- keep it in the buffer.
      buffer = lines.pop() ?? "";

      for (const line of lines) {
        if (!line.trim()) continue;
        const isMeta = processLine(line, callbacks);
        if (!isMeta) {
          rowCount++;
          if (maxRows !== undefined && rowCount >= maxRows) {
            await reader.cancel();
            return;
          }
        }
      }
    }
  } catch (err: unknown) {
    // AbortError is expected when the caller cancels via signal.
    if (err instanceof DOMException && err.name === "AbortError") return;
    throw err;
  }
}

/**
 * Parse and dispatch a single NDJSON line.
 * Returns true if the line was a __meta or __error control line.
 *
 * Exported for unit testing: this control-line classification is the
 * regression oracle the framework migration is verified against.
 */
export function processLine(
  line: string,
  callbacks: StreamCallbacks,
): boolean {
  try {
    const parsed = JSON.parse(line);
    if (parsed.__meta) {
      callbacks.onMeta(parsed.__meta);
      return true;
    }
    if (parsed.__error) {
      callbacks.onError(parsed.__error.message ?? "Stream error");
      return true;
    }
    callbacks.onRow(parsed);
    return false;
  } catch {
    // Skip malformed JSON lines gracefully.
    return true;
  }
}

// fetchJobOnce

/**
 * Fetch a job's current state once via GET /api/v1/query/jobs/{id}.
 *
 * Used as the terminal fallback when the progress SSE drops: the job has very
 * likely finished during the gap, so a single GET recovers the result instead
 * of reconnecting forever.
 */
export async function fetchJobOnce(
  jobId: string,
  signal?: AbortSignal,
): Promise<{ status: string; data?: unknown; meta?: unknown; error?: string }> {
  const resp = await fetch(
    `/api/v1/query/jobs/${encodeURIComponent(jobId)}`,
    { headers: authHeaders(), signal },
  );
  handleAuthError(resp);

  const json = await resp.json().catch(() => null);
  const data = (json?.data ?? {}) as {
    status?: string;
    results?: unknown;
    error?: { message?: string };
  };

  return {
    status: data.status ?? (resp.ok ? "unknown" : "error"),
    data: data.results,
    meta: json?.meta,
    error: data.error?.message,
  };
}

// subscribeJobProgress

/**
 * Subscribe to job progress via SSE with BOUNDED reconnection.
 *
 * A raw `EventSource` auto-reconnects forever on any transient drop, and each
 * reconnect opens a fresh server-side SSE goroutine — the source of the
 * "infinite retries" / duplicated-connection problem. This wrapper instead:
 *  - closes the source on every terminal event (complete/failed/canceled);
 *  - on error, retries at most JOB_SSE_MAX_RECONNECTS times with exponential
 *    backoff, closing the source first so the browser's native auto-reconnect
 *    never races us;
 *  - bounds total lifetime with JOB_SSE_OVERALL_DEADLINE_MS;
 *  - on give-up, performs ONE final GET to recover an already-finished result.
 *
 * Auth uses the same `_token` query param as `api/sse.ts`.
 *
 * @returns Cleanup function that cancels pending reconnects and closes the
 *   EventSource. Safe to call multiple times (idempotent).
 */
export function subscribeJobProgress(
  jobId: string,
  onProgress: (p: ProgressData) => void,
  onComplete: (data: unknown) => void,
  onFailed: (message: string) => void,
  onCanceled: () => void,
): () => void {
  const tokenValue = useAuthStore.getState().token;
  const qs = tokenValue
    ? `?${new URLSearchParams({ _token: tokenValue }).toString()}`
    : "";
  const url = `/api/v1/query/jobs/${encodeURIComponent(jobId)}/stream${qs}`;

  let source: EventSource | null = null;
  let reconnectTimer: ReturnType<typeof setTimeout> | undefined;
  let attempts = 0;
  let settled = false;
  const startedAt = Date.now();
  const fallbackController = new AbortController();

  const cleanup = () => {
    if (reconnectTimer !== undefined) {
      clearTimeout(reconnectTimer);
      reconnectTimer = undefined;
    }
    if (source) {
      source.close();
      source = null;
    }
  };

  const settle = (fn: () => void) => {
    if (settled) return;
    settled = true;
    cleanup();
    fn();
  };

  // Terminal fallback: the SSE gave up, so ask the job endpoint directly. The
  // job most likely finished during the connection gap.
  const recoverViaFetch = () => {
    if (settled) return;
    fetchJobOnce(jobId, fallbackController.signal)
      .then((res) => {
        switch (res.status) {
          case "done":
          case "complete":
            settle(() => onComplete({ data: res.data, meta: res.meta }));
            break;
          case "error":
          case "failed":
            settle(() => onFailed(res.error ?? "Query failed"));
            break;
          case "canceled":
            settle(onCanceled);
            break;
          default:
            settle(() => onFailed("Progress connection lost"));
        }
      })
      .catch((err: unknown) => {
        if (err instanceof DOMException && err.name === "AbortError") return;
        settle(() => onFailed("Progress connection lost"));
      });
  };

  const connect = () => {
    if (settled) return;
    source = new EventSource(url);

    source.onopen = () => {
      attempts = 0; // a clean connection resets the backoff budget
    };

    source.addEventListener("progress", (e: MessageEvent) => {
      try {
        onProgress(JSON.parse(e.data) as ProgressData);
      } catch {
        /* skip malformed progress data */
      }
    });

    source.addEventListener("complete", (e: MessageEvent) => {
      let payload: unknown = null;
      try {
        payload = JSON.parse(e.data);
      } catch {
        /* keep null */
      }
      settle(() => onComplete(payload));
    });

    source.addEventListener("failed", (e: MessageEvent) => {
      let message = "Query failed";
      try {
        const data = JSON.parse(e.data);
        message = data.message ?? data.code ?? message;
      } catch {
        /* keep default */
      }
      settle(() => onFailed(message));
    });

    source.addEventListener("canceled", () => {
      settle(onCanceled);
    });

    source.onerror = () => {
      if (settled) {
        cleanup();
        return;
      }
      const closed = source?.readyState === EventSource.CLOSED;
      const exhausted = attempts >= JOB_SSE_MAX_RECONNECTS;
      const expired = Date.now() - startedAt > JOB_SSE_OVERALL_DEADLINE_MS;
      if (closed || exhausted || expired) {
        cleanup(); // stop the browser's native auto-reconnect
        recoverViaFetch();
        return;
      }
      // Transient: seize control from native reconnect, then retry with backoff.
      cleanup();
      attempts += 1;
      const delay = Math.min(
        JOB_SSE_BACKOFF_BASE_MS * 2 ** (attempts - 1),
        JOB_SSE_BACKOFF_MAX_MS,
      );
      reconnectTimer = setTimeout(connect, delay);
    };
  };

  connect();

  return () => {
    settled = true;
    fallbackController.abort();
    cleanup();
  };
}

// cancelJob

/**
 * Cancel a running async query job.
 */
export async function cancelJob(jobId: string): Promise<void> {
  const resp = await fetch(`/api/v1/query/jobs/${encodeURIComponent(jobId)}`, {
    method: "DELETE",
    headers: authHeaders(),
  });

  handleAuthError(resp);

  if (!resp.ok) {
    const err = await resp
      .json()
      .catch(() => ({ error: { message: resp.statusText } }));
    throw new Error(err.error?.message || err.data?.error || resp.statusText);
  }
}
