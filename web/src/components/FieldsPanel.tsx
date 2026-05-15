import React, {
  useState,
  useMemo,
  useCallback,
  useRef,
  useEffect,
} from "react";
import type { FieldInfo } from "../api/client";
import { FieldValuePopover } from "./FieldValuePopover";
import { typeAbbrev } from "../utils/fieldType";
import styles from "./FieldsPanel.module.css";

interface FieldsPanelProps {
  selectedFields: string[];
  catalogFields: FieldInfo[];
  onFilter?: (field: string, value: string, exclude: boolean) => void;
}

function typeBadgeClass(abbrev: string): string {
  switch (abbrev) {
    case "str":
      return styles.typeBadgeStr ?? "";
    case "int":
      return styles.typeBadgeInt ?? "";
    case "flt":
      return styles.typeBadgeFlt ?? "";
    case "ts":
      return styles.typeBadgeTs ?? "";
    case "bool":
      return styles.typeBadgeBool ?? "";
    default:
      return styles.typeBadgeStr ?? "";
  }
}

export function FieldsPanel({
  selectedFields,
  catalogFields,
  onFilter,
}: FieldsPanelProps) {
  const [search, setSearch] = useState("");
  const [popoverField, setPopoverField] = useState<string | null>(null);
  const [popoverAnchor, setPopoverAnchor] = useState<DOMRect | null>(null);
  const searchTimerRef = useRef<ReturnType<typeof setTimeout> | undefined>(
    undefined,
  );
  const [debouncedSearch, setDebouncedSearch] = useState("");

  const handleSearchChange = useCallback((e: React.FormEvent<HTMLInputElement>) => {
    const value = (e.target as HTMLInputElement).value;
    setSearch(value);
    clearTimeout(searchTimerRef.current);
    searchTimerRef.current = setTimeout(() => {
      setDebouncedSearch(value);
    }, 150);
  }, []);

  // Clear the pending debounce timer on unmount so it cannot fire against
  // an unmounted component.
  useEffect(() => () => clearTimeout(searchTimerRef.current), []);

  // Build a lookup map from catalog fields
  const catalogMap = useMemo(() => {
    const m = new Map<string, FieldInfo>();
    for (const f of catalogFields) {
      m.set(f.name, f);
    }
    return m;
  }, [catalogFields]);

  // Selected fields set for O(1) lookup
  const selectedSet = useMemo(() => new Set(selectedFields), [selectedFields]);

  // Filter by search term
  const searchLower = debouncedSearch.toLowerCase();

  const filteredSelected = useMemo(() => {
    const fields = selectedFields.filter(
      (name) => !searchLower || name.toLowerCase().includes(searchLower),
    );
    return fields;
  }, [selectedFields, searchLower]);

  // Available = catalog fields NOT in selectedFields, filtered by search, sorted alphabetically
  const filteredAvailable = useMemo(() => {
    return catalogFields
      .filter(
        (f) =>
          !selectedSet.has(f.name) &&
          (!searchLower || f.name.toLowerCase().includes(searchLower)),
      )
      .sort((a, b) => a.name.localeCompare(b.name));
  }, [catalogFields, selectedSet, searchLower]);

  const handleFieldClick = useCallback(
    (fieldName: string, e: React.MouseEvent) => {
      const target = e.currentTarget as HTMLElement;
      const rect = target.getBoundingClientRect();
      if (popoverField === fieldName) {
        setPopoverField(null);
        setPopoverAnchor(null);
      } else {
        setPopoverField(fieldName);
        setPopoverAnchor(rect);
      }
    },
    [popoverField],
  );

  const handlePopoverClose = useCallback(() => {
    setPopoverField(null);
    setPopoverAnchor(null);
  }, []);

  const handlePopoverFilter = useCallback(
    (field: string, value: string, exclude: boolean) => {
      onFilter?.(field, value, exclude);
      setPopoverField(null);
      setPopoverAnchor(null);
    },
    [onFilter],
  );

  function renderFieldRow(fieldName: string) {
    const catalog = catalogMap.get(fieldName);
    const abbrev = typeAbbrev(catalog?.type);

    return (
      <div className={styles.fieldRow} key={fieldName}>
        <button
          type="button"
          className={styles.fieldName}
          onClick={(e: React.MouseEvent) => handleFieldClick(fieldName, e)}
          title={fieldName}
        >
          {fieldName}
        </button>
        {abbrev && (
          <span className={`${styles.typeBadge} ${typeBadgeClass(abbrev)}`}>
            {abbrev}
          </span>
        )}
        {catalog && catalog.coverage > 0 && (
          <span className={styles.coverage}>{catalog.coverage}%</span>
        )}
      </div>
    );
  }

  return (
    <div className={styles.fieldsPanel}>
      <input
        type="text"
        className={styles.searchInput}
        placeholder="Filter fields..."
        value={search}
        onInput={handleSearchChange}
      />

      {/* Selected Fields */}
      <div>
        <div className={styles.sectionHeader}>
          Selected Fields ({filteredSelected.length})
        </div>
        {filteredSelected.length > 0 ? (
          filteredSelected.map((name) => renderFieldRow(name))
        ) : (
          <div className={styles.emptyFields}>No selected fields</div>
        )}
      </div>

      <div className={styles.divider} />

      {/* Available Fields */}
      <div>
        <div className={styles.sectionHeader}>
          Available Fields ({filteredAvailable.length})
        </div>
        {filteredAvailable.length > 0 ? (
          filteredAvailable.map((f) => renderFieldRow(f.name))
        ) : (
          <div className={styles.emptyFields}>No available fields</div>
        )}
      </div>

      {/* Field value popover */}
      {popoverField && popoverAnchor && (
        <FieldValuePopover
          fieldName={popoverField}
          anchorRect={popoverAnchor}
          onFilter={handlePopoverFilter}
          onClose={handlePopoverClose}
        />
      )}
    </div>
  );
}
