import { useEffect, useCallback, useState, useRef } from "react";
import { fetchStatus } from "../api/client";
import { formatUptime, formatBytes, formatCount } from "../utils/format";
import styles from "./StatusView.module.css";

interface Props {
  path?: string;
}

// Helpers

function safeNumber(value: unknown): number {
  if (typeof value === "number" && !isNaN(value)) return value;
  return 0;
}

function safeString(value: unknown, fallback = "--"): string {
  if (typeof value === "string" && value.length > 0) return value;
  return fallback;
}

function nested(
  obj: Record<string, unknown> | null,
  key: string,
): Record<string, unknown> {
  if (!obj) return {};
  const v = obj[key];
  if (v && typeof v === "object" && !Array.isArray(v)) {
    return v as Record<string, unknown>;
  }
  return {};
}

function healthClass(health: string): string {
  switch (health) {
    case "healthy":
      return styles.healthHealthy ?? "";
    case "degraded":
      return styles.healthDegraded ?? "";
    default:
      return styles.healthUnhealthy ?? "";
  }
}

function formatLastUpdated(date: Date | null): string {
  if (!date) return "";
  const h = String(date.getHours()).padStart(2, "0");
  const m = String(date.getMinutes()).padStart(2, "0");
  const s = String(date.getSeconds()).padStart(2, "0");
  return `Last updated ${h}:${m}:${s}`;
}

// Component

export function StatusView(_props: Props) {
  const [status, setStatus] = useState<Record<string, unknown> | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [lastUpdatedAt, setLastUpdatedAt] = useState<Date | null>(null);

  // Use ref to keep loadStatus stable for the interval
  const loadStatusRef = useRef<(() => Promise<void>) | undefined>(undefined);

  const loadStatus = useCallback(async () => {
    try {
      const data = await fetchStatus();
      setStatus(data);
      setError(null);
      setLastUpdatedAt(new Date());
    } catch (err: unknown) {
      const message =
        err instanceof Error ? err.message : "Failed to fetch status";
      setError(message);
    } finally {
      setLoading(false);
    }
  }, []);

  loadStatusRef.current = loadStatus;

  useEffect(() => {
    // Initial fetch
    setLoading(true);
    loadStatus();

    // Auto-refresh every 5 seconds
    const interval = setInterval(() => loadStatusRef.current?.(), 5000);
    return () => clearInterval(interval);
  }, [loadStatus]);

  // Loading state (only on first load)
  if (loading && !status) {
    return (
      <div className={styles.loadingState} role="status" aria-live="polite">
        Loading status...
      </div>
    );
  }

  // Error state (only if we never got data)
  if (error && !status) {
    return (
      <div className={styles.errorState} role="alert">
        <div>Unable to connect to server</div>
        <div className={styles.errorMessage}>{error}</div>
        <button type="button" className={styles.retryBtn} onClick={loadStatus}>
          Retry
        </button>
      </div>
    );
  }

  const data = status;
  const storageData = nested(data, "storage");
  const eventsData = nested(data, "events");
  const queriesData = nested(data, "queries");
  const viewsData = nested(data, "views");
  const tailData = nested(data, "tail");

  const health = safeString(data?.health as string | undefined, "unknown");
  const version = safeString(data?.version as string | undefined, "unknown");
  const uptimeSeconds = safeNumber(data?.uptime_seconds);
  const usedBytes = safeNumber(storageData.used_bytes);
  const segmentCount = safeNumber(storageData.segment_count);
  const totalEvents = safeNumber(eventsData.total);
  const todayEvents = safeNumber(eventsData.today);
  const activeQueries = safeNumber(queriesData.active);
  const totalViews = safeNumber(viewsData.total);
  const activeViews = safeNumber(viewsData.active);
  const tailSessions = safeNumber(tailData.active_sessions);
  const tailDropped = safeNumber(tailData.total_dropped_events);

  return (
    <div className={styles.page}>
      <div className={styles.pageHeader}>
        <h1 className={styles.pageTitle}>Server Status</h1>
        <span className={styles.lastUpdated} aria-live="off">
          {formatLastUpdated(lastUpdatedAt)}
        </span>
      </div>

      <div className={styles.grid}>
        {/* Server card */}
        <div className={styles.card}>
          <div className={styles.cardTitle}>Server</div>
          <div className={styles.healthRow}>
            <span
              className={`${styles.healthDot} ${healthClass(health)}`}
              aria-hidden="true"
            />
            <span className={styles.healthLabel}>{health}</span>
          </div>
          <div className={styles.cardSubtext}>
            v{version} &middot; up {formatUptime(uptimeSeconds)}
          </div>
        </div>

        {/* Events card */}
        <div className={styles.card}>
          <div className={styles.cardTitle}>Events</div>
          <div className={styles.cardValue}>{formatCount(totalEvents)}</div>
          <div className={styles.cardSubtext}>{formatCount(todayEvents)} today</div>
        </div>

        {/* Storage card */}
        <div className={styles.card}>
          <div className={styles.cardTitle}>Storage</div>
          <div className={styles.cardValue}>{formatBytes(usedBytes)}</div>
          {segmentCount > 0 && (
            <div className={styles.cardSubtext}>
              {formatCount(segmentCount)}{" "}
              {segmentCount === 1 ? "segment" : "segments"}
            </div>
          )}
        </div>

        {/* Queries card */}
        <div className={styles.card}>
          <div className={styles.cardTitle}>Queries</div>
          <div className={styles.cardValue}>{activeQueries}</div>
          <div className={styles.cardSubtext}>active</div>
        </div>

        {/* Views card */}
        <div className={styles.card}>
          <div className={styles.cardTitle}>Materialized Views</div>
          <div className={styles.cardValue}>{totalViews}</div>
          <div className={styles.cardSubtext}>{activeViews} active</div>
        </div>

        {/* Tail card */}
        <div className={styles.card}>
          <div className={styles.cardTitle}>Live Tail</div>
          <div className={styles.cardValue}>{tailSessions}</div>
          <div className={styles.cardSubtext}>
            {tailSessions === 1 ? "session" : "sessions"}
            {tailDropped > 0 && ` · ${formatCount(tailDropped)} dropped`}
          </div>
        </div>
      </div>
    </div>
  );
}
