import { describe, expect, it } from "vitest";
import {
  INITIAL_FRAME_RATE_CAP,
  nextFrameRateCap,
  type FrameRateCapState,
} from "../src/client/media/frame-rate-cap";

const SHARE_FPS = 120;

// One evidence window; a steady window at 120 fps is 240 frames in 2 s.
const window = (
  received: number | null,
  decoded: number | null,
  options: { lost?: number; sequence?: number; windowMs?: number } = {},
) => ({
  framesReceivedDelta: received,
  framesDecodedDelta: decoded,
  packetsLostDelta: options.lost ?? 0,
  sequence: options.sequence ?? 10,
  windowMs: options.windowMs ?? 2_000,
});

function run(windows: ReturnType<typeof window>[]): FrameRateCapState[] {
  const states: FrameRateCapState[] = [];
  let state = INITIAL_FRAME_RATE_CAP;
  for (const next of windows) {
    state = nextFrameRateCap(state, next, SHARE_FPS);
    states.push(state);
  }
  return states;
}

describe("per-viewer frame-rate cap", () => {
  it("caps at once when a viewer's decoder stalls: frames arrive, almost none is decoded", () => {
    // The phone at 120 fps: 240 frames arrive per window, none is decoded.
    expect(run([window(240, 0)])[0].capped).toBe(true);
    expect(run([window(240, 40)])[0].capped).toBe(true);
  });

  it("ignores the first windows after a join: decoder warm-up and the key-frame burst", () => {
    const states = run([window(298, 0, { sequence: 0 }), window(301, 0, { sequence: 1 })]);
    expect(states.map((state) => state.capped)).toEqual([false, false]);
  });

  it("does not cap a viewer that decodes the full rate through a catch-up burst", () => {
    // Real PC windows after a stall: more frames arrive than the stream sends.
    const states = run([window(298, 250), window(291, 227), window(293, 214), window(300, 235)]);
    expect(states.every((state) => !state.capped)).toBe(true);
  });

  it("forgives the real PC join transient: one slow window between healthy ones", () => {
    const states = run([
      window(298, 250, { sequence: 2 }),
      window(301, 158, { sequence: 3 }), // 48 frames dropped, decoded 66% of the rate
      window(240, 240, { sequence: 4 }),
      window(75, 75, { sequence: 5 }), // fewer arrive (network), all decoded: not the decoder
      window(240, 238, { sequence: 6 }),
    ]);
    expect(states.every((state) => !state.capped)).toBe(true);
    expect(states[4]).toEqual(INITIAL_FRAME_RATE_CAP);
  });

  it("caps a viewer that stays behind for three windows, not before", () => {
    const states = run([window(240, 150), window(240, 160), window(240, 155)]);
    expect(states.map((state) => state.capped)).toEqual([false, false, true]);
  });

  it("restarts the count when a window is healthy", () => {
    const states = run([window(240, 150), window(240, 150), window(240, 240), window(240, 150), window(240, 150)]);
    expect(states.every((state) => !state.capped)).toBe(true);
  });

  it("never caps on a lossy stall: undecodable frames there are a network burst", () => {
    const states = run([window(240, 0, { lost: 40 })]);
    expect(states[0].capped).toBe(false);
    expect(states[0].behind).toBe(1);
  });

  it("does not cap a viewer recovering from a network blackout", () => {
    // Real PC windows: 2,707 packets lost, then a loss-free catch-up burst while
    // the decoder waits for a key frame, then normal again.
    const states = run([
      window(240, 240),
      window(4, 5, { lost: 2707 }),
      window(363, 49), // loss-free, but still recovering from the lossy window
      window(36, 19),
      window(120, 120),
      window(240, 238),
    ]);
    expect(states.every((state) => !state.capped)).toBe(true);
    expect(states[5]).toEqual(INITIAL_FRAME_RATE_CAP);
  });

  it("still caps a stalled decoder once the link has been clean for a window", () => {
    const states = run([window(240, 240), window(4, 5, { lost: 50 }), window(240, 240), window(240, 10)]);
    expect(states.map((state) => state.capped)).toEqual([false, false, false, true]);
  });

  it("scales the expected frame count by the window length", () => {
    // A 2.6 s window at 120 fps carries ~312 frames; decoding 300 is healthy.
    expect(run([window(310, 300, { windowMs: 2_630 })])[0]).toEqual(INITIAL_FRAME_RATE_CAP);
  });

  it("never caps below 50 fps: a 90 fps share stays whole", () => {
    const state = nextFrameRateCap(INITIAL_FRAME_RATE_CAP, window(180, 0), 90);
    expect(state.capped).toBe(false);
  });

  it("keeps a viewer capped once capped, so it never re-enters the stall", () => {
    const capped = run([window(240, 0)])[0];
    expect(nextFrameRateCap(capped, window(120, 120), SHARE_FPS).capped).toBe(true);
  });

  it("ignores windows without receive counters or with too few frames", () => {
    expect(run([window(null, 0)])[0]).toEqual(INITIAL_FRAME_RATE_CAP);
    expect(run([window(4, 0)])[0]).toEqual(INITIAL_FRAME_RATE_CAP);
  });
});
