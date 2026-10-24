import { describe, expect, it } from "vitest";
import {
  INITIAL_FRAME_RATE_CAP,
  nextFrameRateCap,
} from "../src/client/media/frame-rate-cap";

const window = (received: number | null, decoded: number | null, dropped = 0) => ({
  framesReceivedDelta: received,
  framesDecodedDelta: decoded,
  framesDroppedDelta: dropped,
});

describe("per-viewer frame-rate cap", () => {
  it("caps at once when a viewer decodes less than half of what arrives", () => {
    // A phone at 120 fps: every frame arrives, none is decoded.
    expect(nextFrameRateCap(INITIAL_FRAME_RATE_CAP, window(240, 0), 120).capped).toBe(true);
  });

  it("caps a viewer that keeps falling behind, but not after one bad window", () => {
    const once = nextFrameRateCap(INITIAL_FRAME_RATE_CAP, window(240, 190), 120);
    expect(once.capped).toBe(false);
    expect(nextFrameRateCap(once, window(240, 195), 120).capped).toBe(true);
  });

  it("caps a viewer that keeps dropping frames", () => {
    const once = nextFrameRateCap(INITIAL_FRAME_RATE_CAP, window(240, 230, 40), 120);
    expect(nextFrameRateCap(once, window(240, 230, 40), 120).capped).toBe(true);
  });

  it("forgets a single hiccup once the viewer keeps up again", () => {
    const hiccup = nextFrameRateCap(INITIAL_FRAME_RATE_CAP, window(240, 190), 120);
    const recovered = nextFrameRateCap(hiccup, window(240, 240), 120);
    expect(nextFrameRateCap(recovered, window(240, 190), 120).capped).toBe(false);
  });

  it("never caps below 50 fps: a 90 fps share stays whole", () => {
    expect(nextFrameRateCap(INITIAL_FRAME_RATE_CAP, window(180, 0), 90).capped).toBe(false);
  });

  it("keeps a viewer capped once capped, so it never re-enters the stall", () => {
    const capped = nextFrameRateCap(INITIAL_FRAME_RATE_CAP, window(240, 0), 120);
    expect(nextFrameRateCap(capped, window(120, 120), 120).capped).toBe(true);
  });

  it("ignores windows without receive counters or with too few frames", () => {
    expect(nextFrameRateCap(INITIAL_FRAME_RATE_CAP, window(null, 0), 120)).toEqual(INITIAL_FRAME_RATE_CAP);
    expect(nextFrameRateCap(INITIAL_FRAME_RATE_CAP, window(4, 0), 120)).toEqual(INITIAL_FRAME_RATE_CAP);
  });
});
