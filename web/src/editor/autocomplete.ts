import {
  autocompletion,
  type CompletionContext,
  type CompletionResult,
  type Completion,
} from "@codemirror/autocomplete";
import {
  fetchFields,
  fetchIndexes,
  fetchFieldValues,
  type FieldInfo,
  type IndexInfo,
} from "../api/client";
import {
  fetchCatalog,
  type CatalogOperator,
  type CatalogFunction,
  type CatalogAggregate,
} from "../api/catalog";
import {
  AGG_FUNCTIONS,
  BUILTIN_FIELDS,
  COMMAND_DOCS,
  COMMANDS,
  EVAL_FUNCTIONS,
  LATENCY_AGG_SHORTHANDS,
  QUERY_TEMPLATES,
  REGEX_TEMPLATES,
  SOURCE_TEMPLATES,
  TIME_TEMPLATES,
  TIME_VALUES,
} from "./lynxflow-catalog";

// Completion cache -- module-level, refreshed every 60 s or on first trigger

let cachedFields: FieldInfo[] = [];
let cachedIndexes: IndexInfo[] = [];
let lastFetchTime = 0;

// Catalog-derived completion lists (built once from the /api/v1/catalog response)
let catalogOperators: CatalogOperator[] = [];
let catalogFunctions: CatalogFunction[] = [];
let catalogAggregates: CatalogAggregate[] = [];
let catalogParseFormats: string[] = [];
let catalogLoaded = false;

const CACHE_TTL_MS = 60_000;

async function ensureCatalog(): Promise<void> {
  if (catalogLoaded) return;
  const cat = await fetchCatalog();
  catalogOperators = cat.operators;
  catalogFunctions = cat.functions;
  catalogAggregates = cat.aggregates;
  catalogParseFormats = cat.parse_formats;
  catalogLoaded = true;
}

async function ensureCache(): Promise<void> {
  // Kick off catalog load (no-ops after first success)
  const catalogPromise = ensureCatalog();

  const now = Date.now();
  if (
    now - lastFetchTime < CACHE_TTL_MS &&
    (cachedFields.length > 0 || cachedIndexes.length > 0)
  ) {
    await catalogPromise;
    return;
  }

  // Fetch both in parallel; failures are non-critical -- keep stale cache
  const [fieldsResult, indexesResult] = await Promise.allSettled([
    fetchFields(),
    fetchIndexes(),
    catalogPromise,
  ]);

  if (fieldsResult.status === "fulfilled") {
    cachedFields = fieldsResult.value;
  }
  if (indexesResult.status === "fulfilled") {
    cachedIndexes = indexesResult.value;
  }

  lastFetchTime = now;
}

// Per-field value cache so we don't spam the API
const fieldValueCache = new Map<
  string,
  { values: Completion[]; fetched: number }
>();
const VALUE_CACHE_TTL_MS = 30_000;

// Context detection helpers

/** Return the word fragment currently being typed (if any). */
function currentWord(line: string): { word: string } {
  const match = line.match(/([A-Za-z_][A-Za-z0-9_.:-]*|\*)?$/);
  if (match) {
    return { word: match[1] ?? "" };
  }
  return { word: "" };
}

function allFieldCompletions(lowerWord: string): Completion[] {
  const seen = new Set<string>();
  const out: Completion[] = [];

  for (const name of BUILTIN_FIELDS) {
    seen.add(name);
    out.push({
      label: name,
      type: "property",
      detail: "built-in",
      boost: lowerWord && name.toLowerCase().startsWith(lowerWord) ? 3 : 0,
    });
  }

  for (const f of cachedFields) {
    if (seen.has(f.name)) continue;
    seen.add(f.name);
    out.push({
      label: f.name,
      type: "property",
      detail: f.type,
      boost: lowerWord && f.name.toLowerCase().startsWith(lowerWord) ? 2 : 0,
    });
  }

  return out;
}

function commandCompletions(lowerWord: string): Completion[] {
  // Start with the hardcoded catalog (always available)
  const seen = new Set<string>();
  const commands: Completion[] = COMMANDS.map((cmd) => {
    seen.add(cmd);
    return {
      label: cmd,
      type: "keyword",
      detail: COMMAND_DOCS[cmd] || "command",
      boost: lowerWord && cmd.startsWith(lowerWord) ? 2 : 0,
    };
  });

  // Merge operators from the server catalog, deduped by name
  for (const op of catalogOperators) {
    if (seen.has(op.name)) continue;
    seen.add(op.name);
    commands.push({
      label: op.name,
      type: "keyword",
      detail: op.doc || op.class || "command",
      boost: lowerWord && op.name.startsWith(lowerWord) ? 2 : 0,
    });
  }

  return [...commands, ...QUERY_TEMPLATES];
}

function functionCompletions(
  fns: readonly string[],
  lowerWord: string,
): Completion[] {
  return fns.map((fn) => ({
    label: fn,
    type: "function",
    detail: "function",
    apply: fn,
    boost: lowerWord && fn.toLowerCase().startsWith(lowerWord) ? 2 : 0,
  }));
}

/** Merge catalog scalar functions into the hardcoded eval function list. */
function mergedEvalFunctions(lowerWord: string): Completion[] {
  const base = functionCompletions(EVAL_FUNCTIONS, lowerWord);
  const seen = new Set(EVAL_FUNCTIONS.map((f) => f.replace(/\(\)$/, "")));
  for (const fn of catalogFunctions) {
    if (seen.has(fn.name)) continue;
    seen.add(fn.name);
    base.push({
      label: `${fn.name}()`,
      type: "function",
      detail: fn.doc || fn.category || "function",
      apply: `${fn.name}()`,
      boost: lowerWord && fn.name.startsWith(lowerWord) ? 2 : 0,
    });
  }
  return base;
}

/** Merge catalog aggregates into the hardcoded agg function list. */
function mergedAggFunctions(lowerWord: string): Completion[] {
  const base = functionCompletions(AGG_FUNCTIONS, lowerWord);
  const seen = new Set(AGG_FUNCTIONS.map((f) => f.replace(/\(\)$/, "")));
  for (const agg of catalogAggregates) {
    if (seen.has(agg.name)) continue;
    seen.add(agg.name);
    base.push({
      label: `${agg.name}()`,
      type: "function",
      detail: agg.doc || "aggregate",
      apply: `${agg.name}()`,
      boost: lowerWord && agg.name.startsWith(lowerWord) ? 2 : 0,
    });
  }
  return base;
}

/** Build completions for parse format names (after `parse`). */
function parseFormatCompletions(lowerWord: string): Completion[] {
  return catalogParseFormats.map((fmt) => ({
    label: fmt,
    type: "type",
    detail: "parse format",
    boost: lowerWord && fmt.startsWith(lowerWord) ? 2 : 0,
  }));
}

function escapeValue(value: string): string {
  return value.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
}

function valueCompletion(value: string, count: number): Completion {
  const needsQuotes = value === "" || /[\s|()[\]{},<>!=]/.test(value);
  return {
    label: value,
    apply: needsQuotes ? `"${escapeValue(value)}"` : value,
    type: "text",
    detail: `${count}`,
  };
}

// Completion source

async function lynxflowCompletion(
  context: CompletionContext,
): Promise<CompletionResult | null> {
  // Only complete when the user has typed something or explicitly invoked
  const textBefore = context.state.doc.sliceString(0, context.pos);

  // Do not trigger on empty input or pure whitespace unless explicit
  if (!context.explicit && textBefore.trim() === "") return null;

  // Lazy-load the cache (non-blocking; uses stale data if fetch fails)
  await ensureCache();

  const beforeCursor = textBefore;
  const { word } = currentWord(beforeCursor);
  const absFrom = context.pos - word.length;
  const lowerWord = word.toLowerCase();

  // --- After regex operators -> regex snippets ---
  const regexMatch = beforeCursor.match(/(?:=~|!~)(\s*["']?[^"'\s|()]*)$/);
  if (regexMatch) {
    const typedPattern = (regexMatch[1] ?? "").trimStart();
    return {
      from: context.pos - typedPattern.length,
      options: REGEX_TEMPLATES,
      filter: true,
    };
  }

  // --- After "field=" or "field!=" -> field values ---
  // Match patterns like: level=err, level="err, status!=2
  const fieldValueMatch = beforeCursor.match(
    /([A-Za-z_][A-Za-z0-9_.:-]*)\s*(?:==|!=|=|<=|>=|<|>)(\s*["']?([^"'\s|()]*))$/,
  );
  if (fieldValueMatch) {
    const fieldName = fieldValueMatch[1] ?? "";
    const typedValue = (fieldValueMatch[2] ?? "").trimStart();
    const partialValue = fieldValueMatch[3] ?? "";
    // Verify it's a known field
    const isKnownField =
      BUILTIN_FIELDS.includes(fieldName) ||
      cachedFields.some((f) => f.name === fieldName);
    if (isKnownField) {
      const values = await getFieldValues(fieldName);
      if (values.length > 0) {
        const options =
          typedValue.startsWith('"') || typedValue.startsWith("'")
            ? values.map((v) => ({ ...v, apply: `"${escapeValue(v.label)}"` }))
            : values;
        return {
          from: context.pos - Math.max(typedValue.length, partialValue.length),
          options,
          filter: true,
        };
      }
    }
  }

  // --- After time modifiers -> relative time values ---
  const timeMatch = beforeCursor.match(
    /\b(?:earliest|latest|_index_earliest|_index_latest)\s*=\s*([A-Za-z0-9@+-]*)$/i,
  );
  if (timeMatch) {
    return {
      from: context.pos - (timeMatch[1] ?? "").length,
      options: TIME_VALUES,
      filter: true,
    };
  }

  // --- After source name when typing a compact time range: from app[ ---
  if (/\b(?:from|index)\s+[^|\s]+\[$/i.test(beforeCursor)) {
    return {
      from: context.pos - 1,
      options: TIME_TEMPLATES,
      filter: true,
    };
  }

  // --- After pipe or at very start -> commands ---
  if (/\|\s*\w*$/.test(beforeCursor) || beforeCursor.trim() === word) {
    // Only suggest commands if the only thing typed is the partial word,
    // or we are right after a pipe.
    const isPipe = /\|\s*\w*$/.test(beforeCursor);
    const isStart = beforeCursor.trim() === word;
    if (isPipe || isStart) {
      // Need at least 1 char or explicit trigger to avoid noise
      if (!context.explicit && word.length === 0 && !isPipe) return null;
      return {
        from: absFrom,
        options: commandCompletions(lowerWord),
        filter: true,
      };
    }
  }

  // --- After "parse " -> parse format names from catalog ---
  if (/\bparse\s+[A-Za-z0-9_]*$/i.test(beforeCursor)) {
    const formats = parseFormatCompletions(lowerWord);
    if (formats.length > 0) {
      return {
        from: absFrom,
        options: formats,
        filter: true,
      };
    }
  }

  // --- After "from " -> index names ---
  if (/\b(?:from|index)\s+[A-Za-z0-9_.:$!*-]*$/i.test(beforeCursor)) {
    return {
      from: absFrom,
      options: [
        ...cachedIndexes.map((idx) => ({
          label: idx.name,
          type: "variable",
          detail: "source",
          boost:
            lowerWord && idx.name.toLowerCase().startsWith(lowerWord) ? 3 : 0,
        })),
        ...SOURCE_TEMPLATES,
      ],
      filter: true,
    };
  }

  // --- After "by ", "where ", "group ", "order ", "keep ", "omit " -> field names ---
  if (
    /\b(?:by|where|group|order|keep|omit|on|table|fields|dedup|rename|using)\s+[A-Za-z0-9_.:-]*$/i.test(
      beforeCursor,
    )
  ) {
    return {
      from: absFrom,
      options: allFieldCompletions(lowerWord),
      filter: true,
    };
  }

  // --- After comma in a field list (by field1, field2) -> field names ---
  if (
    /\b(?:by|keep|omit|table|fields|dedup)\s+[\w.:-]+(?:\s*,\s*[\w.:-]+)*,\s*[A-Za-z0-9_.:-]*$/i.test(
      beforeCursor,
    )
  ) {
    return {
      from: absFrom,
      options: allFieldCompletions(lowerWord),
      filter: true,
    };
  }

  // --- Latency compute uses shorthand aggregations such as p50, p99, avg ---
  if (
    /\blatency\s+[A-Za-z0-9_.:-]+\s+every\s+[0-9smhdw]+\s+(?:by\s+[A-Za-z0-9_.:-]+\s+)?compute\s+\w*$/i.test(
      beforeCursor,
    )
  ) {
    return {
      from: absFrom,
      options: functionCompletions(LATENCY_AGG_SHORTHANDS, lowerWord),
      filter: true,
    };
  }

  // --- After "compute ", "stats ", "timechart " -> aggregation functions ---
  if (
    /\b(?:compute|stats|timechart|eventstats|streamstats|enrich|running)\s+\w*$/i.test(
      beforeCursor,
    )
  ) {
    return {
      from: absFrom,
      options: mergedAggFunctions(lowerWord),
      filter: true,
    };
  }

  // --- After comma in compute/stats list -> aggregation functions ---
  if (
    /\b(?:compute|stats|timechart|eventstats|streamstats|enrich|running)\s+[\w().,\s]+,\s*\w*$/i.test(
      beforeCursor,
    )
  ) {
    return {
      from: absFrom,
      options: mergedAggFunctions(lowerWord),
      filter: true,
    };
  }

  // --- Eval/where expression contexts -> eval functions + fields ---
  if (/\b(?:eval|let|where)\b[^|]*\w*$/i.test(beforeCursor)) {
    return {
      from: absFrom,
      options: [
        ...mergedEvalFunctions(lowerWord),
        ...allFieldCompletions(lowerWord),
      ],
      filter: true,
    };
  }

  // --- Fallback: if user typed 2+ chars, try field names ---
  if (word.length >= 2) {
    return {
      from: absFrom,
      options: allFieldCompletions(lowerWord),
      filter: true,
    };
  }

  return null;
}

// Field value fetching with cache

async function getFieldValues(fieldName: string): Promise<Completion[]> {
  const cached = fieldValueCache.get(fieldName);
  if (cached && Date.now() - cached.fetched < VALUE_CACHE_TTL_MS) {
    return cached.values;
  }

  try {
    const raw = await fetchFieldValues(fieldName, 20);
    const completions: Completion[] = raw.map((v) =>
      valueCompletion(String(v.value), v.count),
    );
    fieldValueCache.set(fieldName, {
      values: completions,
      fetched: Date.now(),
    });
    return completions;
  } catch {
    return cached?.values ?? [];
  }
}

// Public API -- returns a CodeMirror extension

export function lynxflowAutocompletion() {
  return autocompletion({
    override: [lynxflowCompletion],
    activateOnTyping: true,
    // Override the default "accept with Enter" so Enter still runs the query.
    // Users can accept completions with Tab instead.
    defaultKeymap: true,
  });
}
