import React, { useState, useCallback, useRef } from "react";
import { FieldCommandMenu } from "./FieldCommandMenu";
import { typeAbbrev } from "../../utils/fieldType";
import styles from "./flow.module.css";

export interface FieldValue {
  value: string;
  count: number;
}

interface FieldItemProps {
  name: string;
  type?: string;
  isAdded?: boolean;
  onInsertCommand?: (template: string) => void;
}

export function FieldItem({
  name,
  type,
  isAdded,
  onInsertCommand,
}: FieldItemProps) {
  const [menuOpen, setMenuOpen] = useState(false);
  const moreBtnRef = useRef<HTMLButtonElement>(null);

  const handleNameClick = useCallback(() => {
    if (onInsertCommand) {
      onInsertCommand(`| where ${name}!=""`);
    }
  }, [onInsertCommand, name]);

  const handleMoreClick = useCallback((e: React.MouseEvent) => {
    e.stopPropagation();
    setMenuOpen((prev) => !prev);
  }, []);

  const handleCloseMenu = useCallback(() => {
    setMenuOpen(false);
  }, []);

  const abbrev = typeAbbrev(type);

  return (
    <div className={`${styles.fieldItem} ${isAdded ? styles.fieldItemAdded : ""}`}>
      <div className={styles.fieldItemRow}>
        <button
          type="button"
          className={styles.fieldItemName}
          onClick={handleNameClick}
          title={`Filter: ${name}!=""`}
        >
          {name}
        </button>
        {abbrev && <span className={styles.fieldTypeLabel}>{abbrev}</span>}
        <button
          ref={moreBtnRef}
          type="button"
          className={styles.fieldMoreBtn}
          onClick={handleMoreClick}
          aria-label={`Commands for ${name}`}
          title="Insert command"
        >
          &#8943;
        </button>
      </div>

      {menuOpen && moreBtnRef.current && onInsertCommand && (
        <FieldCommandMenu
          field={name}
          fieldType={abbrev}
          anchorRect={moreBtnRef.current.getBoundingClientRect()}
          onInsertCommand={onInsertCommand}
          onClose={handleCloseMenu}
        />
      )}
    </div>
  );
}
