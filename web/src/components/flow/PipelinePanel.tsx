import { useState, useEffect } from "react";
import type { PipelineStage } from "../../api/client";
import { StageNode } from "./StageNode";
import { FieldList } from "./FieldList";
import styles from "./flow.module.css";

interface PipelinePanelProps {
  stages: PipelineStage[];
  fieldTypes?: Map<string, string>;
  onInsertCommand?: (template: string) => void;
}

export function PipelinePanel({
  stages,
  fieldTypes,
  onInsertCommand,
}: PipelinePanelProps) {
  const [selectedIndex, setSelectedIndex] = useState(stages.length - 1);

  // Reset to last stage when stages change
  useEffect(() => {
    setSelectedIndex(stages.length - 1);
  }, [stages]);

  if (stages.length === 0) {
    return null;
  }

  const selected = stages[selectedIndex] ?? stages[stages.length - 1];

  if (!selected) return null;

  return (
    <div className={styles.pipelinePanel}>
      {/* Hero: Fields for selected stage */}
      <div className={styles.pipelineSectionHeader}>
        <span className={styles.sectionTitle}>Fields</span>
        {selected.fields_unknown && (
          <span className={styles.sectionCount}>+ dynamic</span>
        )}
      </div>
      <div className={styles.fieldsHero}>
        <FieldList
          fields={selected.fields_out ?? []}
          fieldsAdded={selected.fields_added}
          fieldTypes={fieldTypes}
          onInsertCommand={onInsertCommand}
        />
        {(!selected.fields_out || selected.fields_out.length === 0) &&
          !selected.fields_unknown && (
            <div className={styles.stageNoFields}>No field info</div>
          )}
      </div>

      {/* Compact stage selector */}
      <div className={styles.pipelineSectionHeader}>
        <span className={styles.sectionTitle}>Pipeline</span>
        <span className={styles.sectionCount}>
          {stages.length} {stages.length === 1 ? "stage" : "stages"}
        </span>
      </div>
      <div className={styles.pipelineTree}>
        {stages.map((stage, i) => (
          <StageNode
            key={i}
            stage={stage}
            isSelected={i === selectedIndex}
            onSelect={() => setSelectedIndex(i)}
          />
        ))}
      </div>
    </div>
  );
}
