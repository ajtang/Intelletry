"use client";

import { useCallback, useEffect, useRef, useState, useSyncExternalStore, type RefObject } from "react";
import { apiGet } from "./api";

export interface Poll<T> {
  data: T | null;
  error: string | null;
  updatedAt: number | null;
  refresh: () => void;
}

/**
 * Polls an API path. The next request is only scheduled after the last one finishes, so
 * a slow backend is never stacked with requests. Hidden tabs stop polling, and old data
 * stays on screen during failures so that operators keep their context.
 */
export function usePoll<T>(path: string | null, intervalMs: number): Poll<T> {
  const [state, setState] = useState<{ path: string | null; data: T | null; error: string | null; updatedAt: number | null }>({
    path,
    data: null,
    error: null,
    updatedAt: null,
  });
  const [tick, setTick] = useState(0);
  const refresh = useCallback(() => setTick((t) => t + 1), []);

  useEffect(() => {
    if (!path) return;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const run = async () => {
      if (!document.hidden) {
        try {
          const data = await apiGet<T>(path, controller.signal);
          setState({ path, data, error: null, updatedAt: Date.now() });
        } catch (err) {
          if (controller.signal.aborted) return;
          const error = err instanceof Error ? err.message : "request failed";
          setState((s) => (s.path === path ? { ...s, error } : { path, data: null, error, updatedAt: null }));
        }
      }
      if (!controller.signal.aborted) timer = setTimeout(run, intervalMs);
    };
    const onVisible = () => {
      if (!document.hidden) {
        clearTimeout(timer);
        void run();
      }
    };
    void run();
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      controller.abort();
      clearTimeout(timer);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [path, intervalMs, tick]);

  // A new path must not show the previous path's data.
  const current = state.path === path ? state : { data: null, error: null, updatedAt: null };
  return { data: current.data, error: current.error, updatedAt: current.updatedAt, refresh };
}

export type StreamStatus = "connecting" | "live" | "reconnecting";

/**
 * Subscribes to a server-sent event stream. EventSource retries dropped connections by
 * itself but gives up on an HTTP error, such as a 502 during a deployment, so the retry
 * here covers that case. onOpen runs on every connect, because events sent during a
 * disconnect are not replayed and the caller must reload them.
 */
export function useEventStream<T>(path: string | null, onMessage: (msg: T) => void, onOpen?: () => void): StreamStatus {
  const [status, setStatus] = useState<StreamStatus>("connecting");
  const handlers = useRef({ onMessage, onOpen });
  useEffect(() => {
    handlers.current = { onMessage, onOpen };
  });

  useEffect(() => {
    if (!path) return;
    let source: EventSource | undefined;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let delay = 1000;
    let stopped = false;
    const connect = () => {
      source = new EventSource(`/api/v1${path}`);
      source.onopen = () => {
        delay = 1000;
        setStatus("live");
        handlers.current.onOpen?.();
      };
      source.onmessage = (e: MessageEvent<string>) => {
        try {
          handlers.current.onMessage(JSON.parse(e.data) as T);
        } catch {
          // A malformed message must not break the stream.
        }
      };
      source.onerror = () => {
        setStatus("reconnecting");
        if (source?.readyState === EventSource.CLOSED && !stopped) {
          timer = setTimeout(connect, delay);
          delay = Math.min(delay * 2, 15000);
        }
      };
    };
    connect();
    return () => {
      stopped = true;
      clearTimeout(timer);
      source?.close();
      setStatus("connecting");
    };
  }, [path]);
  return status;
}

const nowSubscribers = new Set<() => void>();
let nowValue = 0;
let nowTimer: ReturnType<typeof setInterval> | undefined;

function subscribeNow(cb: () => void) {
  nowSubscribers.add(cb);
  if (!nowTimer) {
    nowValue = Date.now();
    nowTimer = setInterval(() => {
      nowValue = Date.now();
      nowSubscribers.forEach((s) => s());
    }, 1000);
  }
  return () => {
    nowSubscribers.delete(cb);
    if (nowSubscribers.size === 0 && nowTimer) {
      clearInterval(nowTimer);
      nowTimer = undefined;
    }
  };
}

/** The current time, updated each second. It is 0 during server rendering. */
export function useNow(): number {
  return useSyncExternalStore(
    subscribeNow,
    () => nowValue,
    () => 0,
  );
}

const OPERATOR_KEY = "intelletry.operator";
const operatorListeners = new Set<() => void>();
let operatorMemory = "";

function readOperator(): string {
  try {
    return window.localStorage.getItem(OPERATOR_KEY) ?? operatorMemory;
  } catch {
    return operatorMemory;
  }
}

/** The operator name recorded in the alert audit trail. It is kept in this browser only. */
export function useOperator(): [string, (name: string) => void] {
  const name = useSyncExternalStore(
    (cb) => {
      operatorListeners.add(cb);
      return () => operatorListeners.delete(cb);
    },
    readOperator,
    () => "",
  );
  const set = useCallback((value: string) => {
    operatorMemory = value;
    try {
      window.localStorage.setItem(OPERATOR_KEY, value);
    } catch {
      // Storage can be unavailable. The in-memory value still works for this page.
    }
    operatorListeners.forEach((l) => l());
  }, []);
  return [name, set];
}

export function useWidth(ref: RefObject<HTMLElement | null>): number {
  const [width, setWidth] = useState(0);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const observer = new ResizeObserver((entries) => setWidth(entries[0]?.contentRect.width ?? 0));
    observer.observe(el);
    return () => observer.disconnect();
  }, [ref]);
  return width;
}
