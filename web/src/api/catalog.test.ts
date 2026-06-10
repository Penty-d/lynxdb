import { describe, it, expect, vi, beforeEach } from "vitest";
import {
  fetchCatalog,
  _resetCatalogCache,
  _setCatalogCache,
  _FALLBACK_CATALOG,
  type Catalog,
} from "./catalog";

const MOCK_CATALOG: Catalog = {
  operators: [
    {
      name: "where",
      class: "filter",
      streaming: "yes",
      doc: "Boolean row filter",
    },
    {
      name: "stats",
      class: "aggregate",
      streaming: "no",
      doc: "Aggregate rows",
    },
  ],
  functions: [
    {
      name: "lower",
      category: "string",
      params: [{ name: "value", type: "string", optional: false, variadic: false }],
      result: "string",
      fallibility: "infallible",
      doc: "Lowercase string",
    },
  ],
  aggregates: [
    {
      name: "count",
      result: "int",
      doc: "Count rows",
    },
  ],
  parse_formats: ["json", "logfmt", "csv"],
};

beforeEach(() => {
  _resetCatalogCache();
  vi.restoreAllMocks();
});

describe("fetchCatalog", () => {
  it("returns server data on successful fetch", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify(MOCK_CATALOG), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );

    const result = await fetchCatalog();
    expect(result.operators).toHaveLength(2);
    expect(result.operators[0]!.name).toBe("where");
    expect(result.functions[0]!.name).toBe("lower");
    expect(result.aggregates[0]!.name).toBe("count");
    expect(result.parse_formats).toEqual(["json", "logfmt", "csv"]);
  });

  it("returns memoized result on second call", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify(MOCK_CATALOG), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );

    const first = await fetchCatalog();
    const second = await fetchCatalog();
    expect(first).toBe(second);
    expect(fetchSpy).toHaveBeenCalledTimes(1);
  });

  it("returns fallback on network error", async () => {
    vi.spyOn(globalThis, "fetch").mockRejectedValue(new Error("network down"));

    const result = await fetchCatalog();
    expect(result).toBe(_FALLBACK_CATALOG);
    expect(result.operators.length).toBeGreaterThan(0);
    // Verify the fallback has some expected operator names
    const names = result.operators.map((op) => op.name);
    expect(names).toContain("from");
    expect(names).toContain("where");
    expect(names).toContain("stats");
  });

  it("returns fallback on non-OK response", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response("not found", { status: 404 }),
    );

    const result = await fetchCatalog();
    expect(result).toBe(_FALLBACK_CATALOG);
  });

  it("does not retry after fallback is cached", async () => {
    const fetchSpy = vi
      .spyOn(globalThis, "fetch")
      .mockRejectedValueOnce(new Error("fail"));

    const first = await fetchCatalog();
    expect(first).toBe(_FALLBACK_CATALOG);

    // Second call should not fetch again — the fallback is cached
    const second = await fetchCatalog();
    expect(second).toBe(_FALLBACK_CATALOG);
    expect(fetchSpy).toHaveBeenCalledTimes(1);
  });

  it("deduplicates concurrent in-flight requests", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify(MOCK_CATALOG), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );

    const [a, b] = await Promise.all([fetchCatalog(), fetchCatalog()]);
    expect(a).toBe(b);
    expect(fetchSpy).toHaveBeenCalledTimes(1);
  });

  it("_setCatalogCache bypasses fetch", async () => {
    _setCatalogCache(MOCK_CATALOG);
    const fetchSpy = vi.spyOn(globalThis, "fetch");

    const result = await fetchCatalog();
    expect(result).toBe(MOCK_CATALOG);
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it("fallback catalog has no functions/aggregates/parse_formats", () => {
    expect(_FALLBACK_CATALOG.functions).toEqual([]);
    expect(_FALLBACK_CATALOG.aggregates).toEqual([]);
    expect(_FALLBACK_CATALOG.parse_formats).toEqual([]);
  });
});
