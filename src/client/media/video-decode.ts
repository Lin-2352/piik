import type {
  VideoDecode,
  VideoDecodeSupport,
} from "../../shared/protocol";

// Browsers decode H264 and VP8 everywhere WebRTC exists. AV1 and H265 depend
// on the browser and, for smooth playback at screen sizes, on hardware decode.
export type OptionalVideoCodec = keyof VideoDecode;

const OPTIONAL_CODEC_MIME_TYPES: Record<OptionalVideoCodec, string> = {
  av1: "video/av1",
  h265: "video/h265",
};

const REPAIR_CODEC_NAMES = new Set(["rtx", "red", "ulpfec", "flexfec-03"]);

function receivableVideoMimeTypes(): Set<string> {
  if (typeof RTCRtpReceiver === "undefined") return new Set();
  const codecs = RTCRtpReceiver.getCapabilities?.("video")?.codecs ?? [];
  return new Set(codecs.map(({ mimeType }) => mimeType.toLowerCase()));
}

async function decodeSupport(
  mimeType: string,
  receivable: Set<string>,
): Promise<VideoDecodeSupport> {
  if (!receivable.has(mimeType)) return "none";
  try {
    const info = await navigator.mediaCapabilities.decodingInfo({
      type: "webrtc",
      video: {
        contentType: mimeType,
        width: 1920,
        height: 1080,
        bitrate: 20_000_000,
        framerate: 60,
      },
    });
    // The browser says it cannot decode this codec; the Host must know.
    if (!info.supported) return "none";
    return info.powerEfficient ? "hardware" : "software";
  } catch {
    // The receiver lists the codec; this browser cannot say how it decodes.
    return "supported";
  }
}

let probed: VideoDecode | undefined;

/** Starts once per page; the Viewer reports the result when it authenticates. */
export const videoDecodeProbe: Promise<VideoDecode | undefined> = (async () => {
  try {
    const receivable = receivableVideoMimeTypes();
    probed = {
      av1: await decodeSupport(OPTIONAL_CODEC_MIME_TYPES.av1, receivable),
      h265: await decodeSupport(OPTIONAL_CODEC_MIME_TYPES.h265, receivable),
    };
    return probed;
  } catch {
    return undefined;
  }
})();

export function currentVideoDecode(): VideoDecode | undefined {
  return probed;
}

/**
 * Names the optional codec an offer depends on when this browser cannot
 * receive any of the offer's video codecs. A playable offer, or one whose
 * failure has another cause, returns null.
 */
export function unplayableOfferCodec(sdp: string): OptionalVideoCodec | null {
  const receivable = receivableVideoMimeTypes();
  if (receivable.size === 0) return null;
  const offered = new Set<string>();
  let video = false;
  for (const line of sdp.split(/\r?\n/)) {
    if (line.startsWith("m=")) video = line.startsWith("m=video");
    const match = video ? /^a=rtpmap:\d+ ([^/]+)\//.exec(line) : null;
    if (match && !REPAIR_CODEC_NAMES.has(match[1]!.toLowerCase())) {
      offered.add(`video/${match[1]!.toLowerCase()}`);
    }
  }
  if (offered.size === 0 || [...offered].some((mime) => receivable.has(mime))) {
    return null;
  }
  for (const [codec, mimeType] of Object.entries(OPTIONAL_CODEC_MIME_TYPES)) {
    if (offered.has(mimeType)) return codec as OptionalVideoCodec;
  }
  return null;
}

type CodecIssueListener = (codec: OptionalVideoCodec | null) => void;
const codecIssueListeners = new Set<CodecIssueListener>();

/** Receive paths report an unplayable share codec; the Viewer page shows it. */
export function reportUnplayableCodec(codec: OptionalVideoCodec | null): void {
  for (const listener of codecIssueListeners) listener(codec);
}

export function onUnplayableCodec(listener: CodecIssueListener): () => void {
  codecIssueListeners.add(listener);
  return () => codecIssueListeners.delete(listener);
}
