"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { num } from "@/lib/format";
import { COMPONENT_ORDER, twinState, type ComponentId, type Tone } from "@/lib/twin";
import { TwinScene } from "@/lib/twin-scene";
import type { Alert, LiveState } from "@/lib/types";
import { LevelBadge, State } from "./ui";

function toneState(tone: Tone, stale: boolean) {
  if (tone === "ok") return <State tone="ok" label="Normal" />;
  if (tone === "inactive") return <State tone="offline" label={stale ? "No data" : "Inactive"} />;
  return <State tone={tone} />;
}

/** Loaded on the client only, because WebGL and its large library are not needed elsewhere. */
export default function DigitalTwin({ live, alerts, online }: { live: LiveState; alerts: Alert[]; online: boolean }) {
  const mount = useRef<HTMLDivElement>(null);
  const scene = useRef<TwinScene | null>(null);
  const [selected, setSelected] = useState<ComponentId | null>(null);
  const [failed, setFailed] = useState(false);

  const state = useMemo(() => twinState(live, alerts, online), [live, alerts, online]);

  useEffect(() => {
    const el = mount.current;
    if (!el) return;
    let twin: TwinScene;
    try {
      twin = new TwinScene(el, {
        onSelect: (id) => setSelected(id),
        onHover: () => {},
      });
    } catch (err) {
      // The dashboard must stay useful on a machine without WebGL.
      console.error("digital twin unavailable", err);
      // eslint-disable-next-line react-hooks/set-state-in-effect -- reports a failure outside React's control
      setFailed(true);
      return;
    }
    scene.current = twin;
    const onLost = (e: Event) => {
      e.preventDefault();
      setFailed(true);
    };
    const canvas = el.querySelector("canvas");
    canvas?.addEventListener("webglcontextlost", onLost);
    if (process.env.NODE_ENV !== "production" || new URLSearchParams(window.location.search).has("twinDebug")) {
      (window as unknown as { __twin?: TwinScene }).__twin = twin;
    }
    return () => {
      canvas?.removeEventListener("webglcontextlost", onLost);
      twin.dispose();
      scene.current = null;
    };
  }, []);

  // Runs for every sample from the live stream, so the model moves as data arrives.
  useEffect(() => {
    scene.current?.update(state);
  }, [state]);

  useEffect(() => {
    scene.current?.select(selected);
  }, [selected]);

  const current = selected ? state.components[selected] : null;

  if (failed) {
    return <p className="empty">The 3D model needs WebGL, which is unavailable in this browser. The live readings on this page are unaffected.</p>;
  }

  return (
    <div className="twin">
      <div className="twin-view">
        <div className="twin-canvas" ref={mount} />
        <div className="twin-readout" aria-hidden="true">
          <span>{num(state.speedKph)} km/h</span>
          <span>{num(state.socPct)} % charge</span>
          {state.charging.active ? (
            <span>
              {state.charging.kind} charging, {num(state.charging.powerKw)} kW
            </span>
          ) : null}
        </div>
        {state.stale ? <div className="twin-stale">No recent reports. Showing the last known state.</div> : null}
        <button type="button" className="btn twin-reset" onClick={() => { setSelected(null); scene.current?.resetView(); }}>
          Reset view
        </button>
      </div>

      <div className="twin-side">
        <div className="twin-parts" role="group" aria-label="Vehicle parts">
          {COMPONENT_ORDER.map((id) => {
            const c = state.components[id];
            return (
              <button
                key={id}
                type="button"
                className="twin-part"
                aria-pressed={selected === id}
                onClick={() => setSelected(selected === id ? null : id)}
                onPointerEnter={() => scene.current?.hover(id)}
                onPointerLeave={() => scene.current?.hover(null)}
              >
                <span>{c.label}</span>
                {toneState(c.tone, state.stale)}
              </button>
            );
          })}
        </div>

        <div className="twin-detail">
          {current ? (
            <>
              <div className="alert-title">{current.label}</div>
              <p className="note">{current.summary}</p>
              <dl className="meta" style={{ marginTop: 6 }}>
                {current.readings.map((r) => (
                  <div key={r.label} style={{ display: "contents" }}>
                    <dt>{r.label}</dt>
                    <dd>{r.value}</dd>
                  </div>
                ))}
              </dl>
              {current.alerts.map((a) => (
                <div key={a.id} className="alert" style={{ marginTop: 8 }}>
                  <div className="alert-head">
                    <LevelBadge level={a.level} />
                    <span className="alert-title">{a.title}</span>
                  </div>
                  <p className="alert-body">
                    <span className="field-label">Why: </span>
                    {a.reason}
                  </p>
                  <p className="alert-body">
                    <span className="field-label">Likely cause: </span>
                    {a.likely_cause}
                  </p>
                </div>
              ))}
            </>
          ) : (
            <p className="note">Select a part to see its readings and any open diagnosis. Drag to rotate, scroll to zoom. Lit battery modules show state of charge, and their colour shows battery condition.</p>
          )}
        </div>
      </div>
    </div>
  );
}
