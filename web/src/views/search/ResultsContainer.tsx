import { ResultsTable } from "../../components/ResultsTable";
import { ListView } from "../../components/ListView";
import { useSearchStore } from "../../stores/search";
import type { QueryResult, EventsResult } from "../../api/client";
import styles from "../SearchView.module.css";

function resultCount(r: QueryResult | null): number {
  if (!r) return 0;
  if (r.type === "events") return r.events.length;
  return r.rows.length;
}

function EmptyStateInitial() {
  return (
    <div className={styles.emptyState}>
      <div className={styles.emptyTitle}>No events yet</div>
      <div className={styles.emptyHint}>
        Run a query to explore your data, or try:
      </div>
      <code className={styles.emptyCode}>lynxdb demo</code>
      <div className={styles.emptySubHint}>to generate sample log data</div>
    </div>
  );
}

function EmptyStateNoResults() {
  return (
    <div className={styles.emptyState}>
      <div className={styles.emptyTitle}>No matching events</div>
      <div className={styles.emptyHint}>
        Try adjusting your query or expanding the time range
      </div>
    </div>
  );
}

interface ResultsContainerProps {
  resultsAreaRef: React.RefObject<HTMLDivElement | null>;
  onSort: (newQuery: string) => void;
  onFilter: (field: string, value: string, exclude: boolean) => void;
  onCellCopy: (value: string, x: number, y: number) => void;
  onNewEventsBadgeClick: () => void;
}

export function ResultsContainer({
  resultsAreaRef,
  onSort,
  onFilter,
  onCellCopy,
  onNewEventsBadgeClick,
}: ResultsContainerProps) {
  const query = useSearchStore((s) => s.query);
  const result = useSearchStore((s) => s.result);
  const loading = useSearchStore((s) => s.loading);
  const error = useSearchStore((s) => s.error);
  const hasQueried = useSearchStore((s) => s.hasQueried);
  const tailActive = useSearchStore((s) => s.tailActive);
  const tailEvents = useSearchStore((s) => s.tailEvents);
  const tailNewCount = useSearchStore((s) => s.tailNewCount);
  const queryActive = useSearchStore((s) => s.queryActive);
  const canceled = useSearchStore((s) => s.canceled);
  const viewMode = useSearchStore((s) => s.viewMode);

  // Build an EventsResult from live tail events for ResultsTable
  const activeResult: QueryResult | null = tailActive
    ? ({
        type: "events",
        events: tailEvents as unknown as Record<string, unknown>[],
        total: tailEvents.length,
        has_more: false,
      } satisfies EventsResult)
    : result;

  // Determine which content to show in the results area
  const showInitialEmpty =
    !tailActive && !hasQueried && !loading && !queryActive && !error;
  const showNoResults =
    !tailActive &&
    hasQueried &&
    !loading &&
    !queryActive &&
    !error &&
    !canceled &&
    resultCount(result) === 0;

  return (
    <div className={styles.resultsArea} ref={resultsAreaRef}>
      {tailActive && tailNewCount > 0 && (
        <button
          type="button"
          className={styles.newEventsBadge}
          onClick={onNewEventsBadgeClick}
          aria-label={`${tailNewCount} new events, click to scroll to top`}
        >
          &#8593; {tailNewCount} new{" "}
          {tailNewCount === 1 ? "event" : "events"}
        </button>
      )}
      {showInitialEmpty && <EmptyStateInitial />}
      {showNoResults && <EmptyStateNoResults />}
      {!showInitialEmpty &&
        !showNoResults &&
        (viewMode === "table" ? (
          <ResultsTable
            result={activeResult}
            onSort={onSort}
            currentQuery={query}
            onFilter={onFilter}
          />
        ) : (
          <ListView
            result={activeResult}
            onCellCopy={onCellCopy}
            onFilter={onFilter}
          />
        ))}
    </div>
  );
}
