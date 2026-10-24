// A viewer whose browser cannot present the share's full frame rate (a phone
// at 120 fps) does not degrade gracefully: Chrome discards every decoded frame
// until the rate drops. The Host then sends that viewer only the base temporal
// layer, half the frame rate, for the rest of its connection.

export interface FrameRateCapState {
  behind: number;
  capped: boolean;
}

export const INITIAL_FRAME_RATE_CAP: FrameRateCapState = { behind: 0, capped: false };

// Halving must keep motion smooth: a 90 fps share is never cut to 45.
const MIN_CAPPED_FRAMERATE = 50;
const MIN_WINDOW_FRAMES = 10;
const COLLAPSED_RATIO = 0.5; // decoded/received: the viewer has stalled
const BEHIND_RATIO = 0.85;
const DROPPED_RATIO = 0.1;
const BEHIND_WINDOWS = 2;

export function nextFrameRateCap(
  state: FrameRateCapState,
  metrics: {
    framesReceivedDelta?: number | null;
    framesDecodedDelta: number | null;
    framesDroppedDelta: number | null;
    packetsLostDelta?: number | null;
  },
  shareFramerate: number,
): FrameRateCapState {
  if (state.capped || shareFramerate / 2 < MIN_CAPPED_FRAMERATE) return state;
  const received = metrics.framesReceivedDelta ?? null;
  const decoded = metrics.framesDecodedDelta;
  if (received === null || decoded === null || received < MIN_WINDOW_FRAMES) return state;
  // Undecodable frames in a lossy window are a network burst, not the decoder:
  // that window only counts as falling behind.
  const lossFree = (metrics.packetsLostDelta ?? 0) === 0;
  if (lossFree && decoded < received * COLLAPSED_RATIO) return { behind: 0, capped: true };
  const dropped = metrics.framesDroppedDelta ?? 0;
  if (decoded >= received * BEHIND_RATIO && dropped <= received * DROPPED_RATIO) {
    return state.behind === 0 ? state : INITIAL_FRAME_RATE_CAP;
  }
  const behind = state.behind + 1;
  return { behind, capped: behind >= BEHIND_WINDOWS };
}
