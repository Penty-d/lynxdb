import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { processLine, submitHybridQuery, subscribeJobProgress } from "./streaming";
import type { StreamCallbacks } from "./streaming";
import { HYBRID_WAIT_SECONDS, JOB_SSE_MAX_RECONNECTS } from "./config";

function makeCallbacks() {
  const onRow = vi.fn<StreamCallbacks["onRow"]>();
  const onMeta = vi.fn<StreamCallbacks["onMeta"]>();
  const onError = vi.fn<StreamCallbacks["onError"]>();
  const callbacks: StreamCallbacks = { onRow, onMeta, onError };
  return { callbacks, onRow, onMeta, onError };
}

describe("processLine", () => {
  it("dispatches a data row and reports it as non-control", () => {
    const cb = makeCallbacks();
    const isControl = processLine('{"a":1}', cb.callbacks);
    expect(isControl).toBe(false);
    expect(cb.onRow).toHaveBeenCalledWith({ a: 1 });
    expect(cb.onMeta).not.toHaveBeenCalled();
    expect(cb.onError).not.toHaveBeenCalled();
  });

  it("routes __meta control lines to onMeta", () => {
    const cb = makeCallbacks();
    const isControl = processLine('{"__meta":{"total":42}}', cb.callbacks);
    expect(isControl).toBe(true);
    expect(cb.onMeta).toHaveBeenCalledWith({ total: 42 });
    expect(cb.onRow).not.toHaveBeenCalled();
  });

  it("routes __error control lines to onError", () => {
    const cb = makeCallbacks();
    const isControl = processLine('{"__error":{"message":"boom"}}', cb.callbacks);
    expect(isControl).toBe(true);
    expect(cb.onError).toHaveBeenCalledWith("boom");
  });

  it("falls back to a default error message", () => {
    const cb = makeCallbacks();
    processLine('{"__error":{}}', cb.callbacks);
    expect(cb.onError).toHaveBeenCalledWith("Stream error");
  });

  it("skips malformed JSON without invoking callbacks", () => {
    const cb = makeCallbacks();
    const isControl = processLine("{not json", cb.callbacks);
    expect(isControl).toBe(true);
    expect(cb.onRow).not.toHaveBeenCalled();
    expect(cb.onMeta).not.toHaveBeenCalled();
    expect(cb.onError).not.toHaveBeenCalled();
  });
});

describe("submitHybridQuery", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  function stubFetch(status: number, json: unknown) {
    const fetchMock = vi.fn(
      async (_url: string, _init: { body?: string }) => ({
        ok: status >= 200 && status < 300,
        status,
        json: async () => json,
      }),
    );
    vi.stubGlobal("fetch", fetchMock);
    return fetchMock;
  }

  function sentBody(fetchMock: ReturnType<typeof stubFetch>) {
    return JSON.parse(fetchMock.mock.calls[0]?.[1]?.body ?? "{}");
  }

  it("defaults the wait window to HYBRID_WAIT_SECONDS", async () => {
    const fetchMock = stubFetch(200, {
      data: { type: "events", events: [] },
      meta: { took_ms: 1, scanned: 0 },
    });

    await submitHybridQuery("search x");

    const body = sentBody(fetchMock);
    expect(body.wait).toBe(HYBRID_WAIT_SECONDS);
    expect(body.q).toBe("search x");
  });

  it("honors an explicit wait override", async () => {
    const fetchMock = stubFetch(200, {
      data: { type: "events", events: [] },
      meta: {},
    });

    await submitHybridQuery("search x", undefined, undefined, undefined, undefined, undefined, 0.2);

    expect(sentBody(fetchMock).wait).toBe(0.2);
  });

  it("returns an async job handle on 202", async () => {
    stubFetch(202, { data: { job_id: "qry_abc" } });

    const res = await submitHybridQuery("search x");
    expect(res.status).toBe("async");
    expect(res.jobId).toBe("qry_abc");
  });
});

// Minimal driveable EventSource stand-in for testing reconnect control.
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 2;

  url: string;
  readyState = FakeEventSource.CONNECTING;
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  closed = false;
  private listeners: Record<string, ((e: MessageEvent) => void)[]> = {};

  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, fn: (e: MessageEvent) => void) {
    (this.listeners[type] ??= []).push(fn);
  }

  close() {
    this.closed = true;
    this.readyState = FakeEventSource.CLOSED;
  }

  emit(type: string, data: unknown) {
    for (const fn of this.listeners[type] ?? [])
      fn({ data: JSON.stringify(data) } as MessageEvent);
  }

  fireError() {
    this.onerror?.();
  }

  static latest(): FakeEventSource {
    const last =
      FakeEventSource.instances[FakeEventSource.instances.length - 1];
    if (!last) throw new Error("no EventSource instance created");
    return last;
  }

  static reset() {
    FakeEventSource.instances = [];
  }
}

describe("subscribeJobProgress", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    FakeEventSource.reset();
    vi.stubGlobal("EventSource", FakeEventSource);
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("delivers the result and closes the source on a terminal complete event", () => {
    const onComplete = vi.fn();
    subscribeJobProgress("qry_1", vi.fn(), onComplete, vi.fn(), vi.fn());

    const src = FakeEventSource.latest();
    src.emit("complete", { data: { type: "events", events: [] }, meta: {} });

    expect(onComplete).toHaveBeenCalledTimes(1);
    expect(src.closed).toBe(true);

    // No reconnection should be scheduled after a terminal event.
    const before = FakeEventSource.instances.length;
    vi.advanceTimersByTime(10_000);
    expect(FakeEventSource.instances.length).toBe(before);
  });

  it("reconnects at most JOB_SSE_MAX_RECONNECTS times, then recovers via one GET", async () => {
    const fetchMock = vi.fn(async () => ({
      ok: true,
      status: 200,
      json: async () => ({
        data: { status: "done", results: { type: "events", events: [] } },
        meta: { took_ms: 3 },
      }),
    }));
    vi.stubGlobal("fetch", fetchMock);

    const onComplete = vi.fn();
    const onFailed = vi.fn();
    subscribeJobProgress("qry_2", vi.fn(), onComplete, onFailed, vi.fn());

    // Drive transient errors; each schedules a backoff reconnect we flush.
    for (let i = 0; i < JOB_SSE_MAX_RECONNECTS; i++) {
      FakeEventSource.latest().fireError();
      await vi.advanceTimersByTimeAsync(5000);
    }
    // One more error after the budget is exhausted triggers the GET fallback.
    FakeEventSource.latest().fireError();
    await vi.advanceTimersByTimeAsync(0);

    // initial connection + JOB_SSE_MAX_RECONNECTS reconnects
    expect(FakeEventSource.instances.length).toBe(JOB_SSE_MAX_RECONNECTS + 1);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(onComplete).toHaveBeenCalledTimes(1);
    expect(onComplete).toHaveBeenCalledWith({
      data: { type: "events", events: [] },
      meta: { took_ms: 3 },
    });
    expect(onFailed).not.toHaveBeenCalled();
  });

  it("stops reconnecting once the cleanup function is called", () => {
    const unsubscribe = subscribeJobProgress(
      "qry_3",
      vi.fn(),
      vi.fn(),
      vi.fn(),
      vi.fn(),
    );

    FakeEventSource.latest().fireError(); // schedule a reconnect
    unsubscribe(); // ...then tear down before it fires

    const before = FakeEventSource.instances.length;
    vi.advanceTimersByTime(10_000);
    expect(FakeEventSource.instances.length).toBe(before);
  });
});
