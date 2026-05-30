/**
 * Tuning constants for query execution timing and async-job handling.
 *
 * Kept in one place so the latency/UX trade-offs are easy to find and adjust.
 */

/**
 * How long (seconds) the server may block before falling back to an async job.
 *
 * Sent as the `wait` parameter to POST /api/v1/query. Most interactive queries
 * finish well under this window and return inline (HTTP 200) — matching CLI
 * latency. Only genuinely long scans exceed it and degrade to the async
 * progress UI. Raising this keeps more queries inline at the cost of holding
 * the request open longer; lowering it surfaces the progress UI sooner.
 */
export const HYBRID_WAIT_SECONDS = 2.0;

/** Max bounded reconnect attempts for the job-progress SSE before giving up. */
export const JOB_SSE_MAX_RECONNECTS = 4;

/** Base backoff (ms) between job-progress SSE reconnect attempts. */
export const JOB_SSE_BACKOFF_BASE_MS = 500;

/** Ceiling (ms) on the exponential reconnect backoff. */
export const JOB_SSE_BACKOFF_MAX_MS = 4000;

/**
 * Overall lifetime (ms) for a single job-progress subscription. A connection
 * that keeps flapping is abandoned after this, independent of the attempt
 * counter, so a subscription can never live forever.
 */
export const JOB_SSE_OVERALL_DEADLINE_MS = 120_000;

/**
 * How long (ms) a fetched field catalog is considered fresh. Within this window
 * repeat queries reuse the cached catalog instead of re-fetching it.
 */
export const FIELDS_CACHE_TTL_MS = 60_000;
