import { afterEach, describe, expect, it, vi } from "vitest";
import { deriveParticipantStatus } from "../src/client/ui/media-status";

function stubReceiver(mimeTypes: string[]): void {
  vi.stubGlobal("RTCRtpReceiver", {
    getCapabilities: () => ({
      codecs: mimeTypes.map((mimeType) => ({ mimeType, clockRate: 90_000 })),
      headerExtensions: [],
    }),
  });
}

function offer(...rtpmaps: string[]): string {
  return [
    "v=0",
    "m=audio 9 UDP/TLS/RTP/SAVPF 111",
    "a=rtpmap:111 opus/48000/2",
    "m=video 9 UDP/TLS/RTP/SAVPF 49 50",
    ...rtpmaps.map((codec, index) => `a=rtpmap:${49 + index} ${codec}/90000`),
    "",
  ].join("\r\n");
}

async function load() {
  vi.resetModules();
  return import("../src/client/media/video-decode");
}

afterEach(() => vi.unstubAllGlobals());

describe("viewer video decode", () => {
  it("names the codec of an offer this browser cannot receive", async () => {
    stubReceiver(["video/H264", "video/VP8", "video/rtx"]);
    const { unplayableOfferCodec } = await load();
    expect(unplayableOfferCodec(offer("H265", "rtx"))).toBe("h265");
    expect(unplayableOfferCodec(offer("AV1"))).toBe("av1");
    // A playable alternative, or an unknown failure, is not a codec issue.
    expect(unplayableOfferCodec(offer("H265", "H264"))).toBeNull();
    expect(unplayableOfferCodec(offer("H264"))).toBeNull();
    expect(unplayableOfferCodec(offer("VP9"))).toBeNull();
  });

  it("ignores audio codecs and repair formats when judging an offer", async () => {
    stubReceiver(["video/H265", "audio/opus"]);
    const { unplayableOfferCodec } = await load();
    expect(unplayableOfferCodec(offer("H265", "rtx"))).toBeNull();
  });

  it("classifies hardware, software and unknown decode", async () => {
    stubReceiver(["video/H264", "video/AV1", "video/H265"]);
    vi.stubGlobal("navigator", {
      mediaCapabilities: {
        decodingInfo: vi.fn(async ({ video }: { video: { contentType: string } }) =>
          video.contentType === "video/av1"
            ? { supported: true, smooth: false, powerEfficient: false }
            : { supported: true, smooth: true, powerEfficient: true }),
      },
    });
    const { videoDecodeProbe, currentVideoDecode } = await load();
    await expect(videoDecodeProbe).resolves.toEqual({ av1: "software", h265: "hardware" });
    expect(currentVideoDecode()).toEqual({ av1: "software", h265: "hardware" });
  });

  it("reports a listed codec as supported when decode efficiency is unknown", async () => {
    stubReceiver(["video/H264", "video/H265"]);
    vi.stubGlobal("navigator", {
      mediaCapabilities: { decodingInfo: vi.fn(async () => { throw new TypeError("webrtc"); }) },
    });
    const { videoDecodeProbe } = await load();
    await expect(videoDecodeProbe).resolves.toEqual({ av1: "none", h265: "supported" });
  });
});

describe("host viewer decode status", () => {
  const ready = { mediaReady: true as const, upstream: { kind: "sfu" as const } };

  it("explains a viewer that cannot play the share codec before it connects", () => {
    const viewer = { upstream: { kind: "sfu" as const }, videoDecode: { av1: "none", h265: "none" } as const };
    expect(deriveParticipantStatus(viewer, true, undefined, "h265").labelKey)
      .toBe("state.peer.unplayable.h265");
    expect(deriveParticipantStatus(viewer, true, undefined, "h264").labelKey)
      .toBe("state.peer.connecting");
    expect(deriveParticipantStatus(viewer, false, undefined, "h265").labelKey)
      .toBe("state.peer.waiting");
  });

  it("marks software decode of the share codec on a connected viewer", () => {
    const viewer = { ...ready, videoDecode: { av1: "software", h265: "hardware" } as const };
    expect(deriveParticipantStatus(viewer, true, undefined, "av1").labelKey)
      .toBe("state.peer.softwareDecode.av1");
    expect(deriveParticipantStatus(viewer, true, undefined, "h265").labelKey)
      .toBe("state.peer.connected");
    expect(deriveParticipantStatus(ready, true, undefined, "av1").labelKey)
      .toBe("state.peer.connected");
  });
});
