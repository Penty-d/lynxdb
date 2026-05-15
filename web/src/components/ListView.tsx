import React, { useState, useCallback } from "react";
import type { QueryResult, EventsResult, AggregateResult } from "../api/client";
import { EventDetailInline } from "./EventDetail";
import { rowKey } from "../utils/rowKey";
import styles from "./ListView.module.css";

interface ListViewProps {
  result: QueryResult | null;
  onCellCopy?: (value: string, x: number, y: number) => void;
  onFilter?: (field: string, value: string, exclude: boolean) => void;
}

/** Derive columns from events: _time first, then _raw, _source, source, then alphabetical */
function deriveColumnsFromEvents(events: Record<string, unknown>[]): string[] {
  const keySet = new Set<string>();
  const limit = Math.min(events.length, 100);
  for (let i = 0; i < limit; i++) {
    for (const key of Object.keys(events[i])) {
      keySet.add(key);
    }
  }

  const priority = ["_time", "_raw", "_source", "source"];
  const ordered: string[] = [];
  for (const p of priority) {
    if (keySet.has(p)) {
      ordered.push(p);
      keySet.delete(p);
    }
  }

  const rest = Array.from(keySet).sort();
  return ordered.concat(rest);
}

/** Normalize result data into columns and rows */
function useTableData(result: QueryResult | null): {
  columns: string[];
  rowCount: number;
  getRow: (index: number) => Record<string, unknown>;
} {
  if (!result) {
    return { columns: [], rowCount: 0, getRow: () => ({}) };
  }

  if (result.type === "events") {
    const evts = (result as EventsResult).events;
    const columns = deriveColumnsFromEvents(evts);
    return {
      columns,
      rowCount: evts.length,
      getRow: (i: number) => evts[i] ?? {},
    };
  }

  const agg = result as AggregateResult;
  const columns = agg.columns;
  return {
    columns,
    rowCount: agg.rows.length,
    getRow: (i: number) => {
      const row: Record<string, unknown> = {};
      const data = agg.rows[i];
      if (data) {
        for (let c = 0; c < columns.length; c++) {
          row[columns[c]] = data[c];
        }
      }
      return row;
    },
  };
}

export function ListView({ result, onCellCopy, onFilter }: ListViewProps) {
  const { columns, rowCount, getRow } = useTableData(result);
  const [expandedIndex, setExpandedIndex] = useState<number | null>(null);

  const handleToggle = useCallback((i: number) => {
    setExpandedIndex((prev) => (prev === i ? null : i));
  }, []);

  if (!result || rowCount === 0) {
    return <div className={styles.empty}>No results</div>;
  }

  const events = [];
  for (let i = 0; i < rowCount; i++) {
    const row = getRow(i);
    const isExpanded = expandedIndex === i;

    events.push(
      <div
        key={rowKey(row)}
        className={`${styles.event} ${isExpanded ? styles.eventSelected : ""}`}
        onClick={() => handleToggle(i)}
      >
        <div className={styles.eventHeader}>Event {i + 1}</div>
        {columns.map((col) => {
          const value = row[col] == null ? "" : String(row[col]);
          return (
            <div key={col} className={styles.field}>
              <span className={styles.fieldName}>{col}</span>
              <span
                className={styles.fieldValue}
                title={value}
                onClick={(e: React.MouseEvent) => {
                  e.stopPropagation();
                  if (onCellCopy && value) {
                    onCellCopy(value, e.clientX, e.clientY);
                  }
                }}
              >
                {value}
              </span>
            </div>
          );
        })}
      </div>,
    );

    if (isExpanded) {
      events.push(
        <div key={`acc-${rowKey(row)}`} className={styles.accordionRow}>
          <EventDetailInline event={row} onFilter={onFilter} />
        </div>,
      );
    }
  }

  return <div className={styles.wrapper}>{events}</div>;
}
