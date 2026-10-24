// A viewer whose browser cannot present the share's full frame rate (a phone
// at 120 fps) does not degrade gracefully: Chrome discards every decoded frame
// until the rate drops. The Host then sends that viewer only the base temporal
// layer, half the frame rate, for the rest of its connection.
//
// The cap is permanent, so it must not fire on a transient. A viewer that has
// just joined, or just stalled for a moment, receives a catch-up burst of more
// frames than the stream carries; judging it against what *arrived* makes a
// healthy decoder look behind. It is judged against the frames the stream
// should deliver in the window instead.

export interface FrameRateCapState {
  behind: number;
  capped: boolean;
  // The previous window lost packets: the decoder may still be waiting for a
  // key frame, which looks like a stall but is the network's doing.
  recovering?: boolean;
}

export const INITIAL_FRAME_RATE_CAP: FrameRateCapState = { behind: 0, capped: false };

// Halving must keep motion smooth: a 90 fps share is never cut to 45.
const MIN_CAPPED_FRAMERATE = 50;
const MIN_WINDOW_FRAMES = 10;
// Decoder warm-up and the join's key-frame burst: not evidence of anything.
const WARMUP_WINDOWS = 2;
// Decoding under this share of what should flow is a stalled decoder.
const STALLED_RATIO = 0.25;
const BEHIND_RATIO = 0.85;
// Weak windows in a row before a viewer that is not stalled is capped.
const BEHIND_WINDOWS = 3;

export function nextFrameRateCap(
  state: FrameRateCapState,
  metrics: {
    framesReceivedDelta?: number | null;
    framesDecodedDelta: number | null;
    packetsLostDelta?: number | null;
    sequence?: number;
    windowMs?: number;
  },
  shareFramerate: number,
): FrameRateCapState {
  if (state.capped || shareFramerate / 2 < MIN_CAPPED_FRAMERATE) return state;
  if ((metrics.sequence ?? WARMUP_WINDOWS) < WARMUP_WINDOWS) return state;
  const received = metrics.framesReceivedDelta ?? null;
  const decoded = metrics.framesDecodedDelta;
  const lossy = (metrics.packetsLostDelta ?? 0) > 0;
  if (received === null || decoded === null || received < MIN_WINDOW_FRAMES) {
    // A blackout delivers almost nothing; it still marks the recovery after it.
    return lossy ? { ...state, recovering: true } : state;
  }
  // What a healthy decoder could have decoded: no more than the stream carried
  // and no more than actually arrived (a network shortfall is not its fault).
  const expected = (shareFramerate * (metrics.windowMs ?? 2_000)) / 1_000;
  const available = Math.min(received, expected);
  // Undecodable frames in a lossy window, or the one after it, are a network
  // burst and its key-frame wait, not the decoder: those windows only count as
  // falling behind.
  if (!lossy && !state.recovering && decoded < available * STALLED_RATIO) {
    return { behind: 0, capped: true };
  }
  if (decoded >= available * BEHIND_RATIO) {
    return state.behind === 0 && !lossy && !state.recovering
      ? state
      : { behind: 0, capped: false, ...(lossy ? { recovering: true } : {}) };
  }
  const behind = state.behind + 1;
  return { behind, capped: behind >= BEHIND_WINDOWS, ...(lossy ? { recovering: true } : {}) };
}
