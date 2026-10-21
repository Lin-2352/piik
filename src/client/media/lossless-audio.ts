// Optional lossless playback of an App Host's mixed audio. The App sends the
// same PCM as its Opus track, FLAC-encoded, on one data channel per edge. This
// player decodes it with WebCodecs and plays it through its own 48 kHz context;
// Opus remains the synchronized default and the fallback on any problem.

export const LOSSLESS_CHANNEL_LABEL = "piik-lossless-audio";

const SAMPLE_RATE = 48_000;
const CHANNELS = 2;
// Underruns are tolerated (the buffer refills); repeated ones mean this
// network or device cannot sustain lossless, so Opus takes over.
const MAX_UNDERRUNS = 3;
const UNDERRUN_WINDOW_MS = 10_000;

// fLaC + STREAMINFO for 48 kHz, stereo, 16-bit, 960-sample blocks, as the App
// writes it; only used to ask the browser whether it decodes FLAC at all.
export const FLAC_DESCRIPTION = new Uint8Array([
  0x66, 0x4c, 0x61, 0x43, 0x80, 0x00, 0x00, 0x22, 0x03, 0xc0, 0x03, 0xc0,
  0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0b, 0xb8, 0x02, 0xf0, 0x00, 0x00,
  0x00, 0x00, ...new Array<number>(16).fill(0),
]);

// Plays from a ring buffer once it holds the target delay. Small clock drift
// between the App and this device is absorbed by skipping or repeating one
// sample per render quantum only while the fill is far from the target.
const WORKLET_SOURCE = `
class PiikLossless extends AudioWorkletProcessor {
  constructor() {
    super();
    this.capacity = sampleRate * 2;
    this.left = new Float32Array(this.capacity);
    this.right = new Float32Array(this.capacity);
    this.read = 0;
    this.size = 0;
    this.target = Math.round(sampleRate * 0.15);
    this.playing = false;
    this.port.onmessage = ({ data }) => {
      if (data.type === "target") this.target = data.samples;
      else if (data.type === "audio") this.push(data.left, data.right);
    };
  }
  push(left, right) {
    for (let index = 0; index < left.length; index++) {
      if (this.size === this.capacity) {
        this.read = (this.read + 1) % this.capacity;
        this.size--;
      }
      const at = (this.read + this.size) % this.capacity;
      this.left[at] = left[index];
      this.right[at] = right[index];
      this.size++;
    }
  }
  process(_inputs, outputs) {
    const left = outputs[0][0];
    const right = outputs[0][1];
    const frames = left.length;
    if (!this.playing) {
      if (this.size < this.target) return true;
      this.playing = true;
      this.port.postMessage({ type: "playing" });
    }
    const slack = sampleRate * 0.06;
    let step = 0;
    if (this.size > this.target + slack) step = 1;
    else if (this.size < this.target - slack) step = -1;
    if (this.size < frames + 1) {
      this.playing = false;
      this.port.postMessage({ type: "underrun" });
      return true;
    }
    for (let index = 0; index < frames; index++) {
      left[index] = this.left[this.read];
      right[index] = this.right[this.read];
      if (!(step < 0 && index === frames >> 1)) {
        this.read = (this.read + 1) % this.capacity;
        this.size--;
      }
    }
    if (step > 0) {
      this.read = (this.read + 1) % this.capacity;
      this.size--;
    }
    return true;
  }
}
registerProcessor("piik-lossless", PiikLossless);
`;

export async function losslessPlaybackSupported(): Promise<boolean> {
  if (
    typeof AudioDecoder === "undefined" ||
    typeof AudioWorkletNode === "undefined" ||
    typeof EncodedAudioChunk === "undefined"
  ) {
    return false;
  }
  try {
    const result = await AudioDecoder.isConfigSupported({
      codec: "flac",
      sampleRate: SAMPLE_RATE,
      numberOfChannels: CHANNELS,
      description: FLAC_DESCRIPTION,
    });
    return result.supported === true;
  } catch {
    return false;
  }
}

export type LosslessStopReason =
  | "unsupported"
  | "underrun"
  | "congested"
  | "closed"
  | "decoder";

export interface LosslessAudioEvents {
  onPlaying(): void;
  onStopped(reason: LosslessStopReason): void;
}

interface LosslessHeader {
  type: "flac";
  sampleRate: number;
  channels: number;
  description: string;
}

function decodeBase64(value: string): Uint8Array {
  const binary = atob(value);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index++) {
    bytes[index] = binary.charCodeAt(index);
  }
  return bytes;
}

export class LosslessAudioPlayer {
  private context: AudioContext | null = null;
  private node: AudioWorkletNode | null = null;
  private gain: GainNode | null = null;
  private decoder: AudioDecoder | null = null;
  private moduleUrl: string | null = null;
  private stopped = false;
  private underruns: number[] = [];
  private volume = 1;

  constructor(
    private readonly channel: RTCDataChannel,
    private readonly events: LosslessAudioEvents,
  ) {}

  /** Call from a user gesture: it creates and resumes the audio context. */
  async start(): Promise<void> {
    try {
      const context = new AudioContext({
        sampleRate: SAMPLE_RATE,
        latencyHint: "interactive",
      });
      this.context = context;
      const resumed = context.resume();
      this.moduleUrl = URL.createObjectURL(
        new Blob([WORKLET_SOURCE], { type: "text/javascript" }),
      );
      await context.audioWorklet.addModule(this.moduleUrl);
      if (this.stopped) return;
      const node = new AudioWorkletNode(context, "piik-lossless", {
        numberOfInputs: 0,
        numberOfOutputs: 1,
        outputChannelCount: [CHANNELS],
      });
      const gain = context.createGain();
      gain.gain.value = this.volume;
      node.connect(gain).connect(context.destination);
      node.port.onmessage = ({ data }: MessageEvent<{ type: string }>) => {
        if (data.type === "playing") this.events.onPlaying();
        else if (data.type === "underrun") this.noteUnderrun();
      };
      this.node = node;
      this.gain = gain;
      this.channel.binaryType = "arraybuffer";
      this.channel.addEventListener("message", this.onMessage);
      this.channel.addEventListener("close", this.onClose);
      this.channel.send(JSON.stringify({ type: "start" }));
      await resumed;
    } catch {
      this.fail("unsupported");
    }
  }

  setVolume(level: number): void {
    this.volume = level;
    if (this.gain) this.gain.gain.value = level;
  }

  /** Matches the Opus/video playout delay so the two stay in step. */
  setTargetDelayMs(delayMs: number): void {
    const bounded = Math.max(40, Math.min(800, delayMs));
    this.node?.port.postMessage({
      type: "target",
      samples: Math.round((bounded * SAMPLE_RATE) / 1_000),
    });
  }

  stop(): void {
    if (this.stopped) return;
    this.stopped = true;
    this.channel.removeEventListener("message", this.onMessage);
    this.channel.removeEventListener("close", this.onClose);
    if (this.channel.readyState === "open") {
      try {
        this.channel.send(JSON.stringify({ type: "stop" }));
      } catch {
        // A closing channel already ends the App's stream.
      }
    }
    if (this.decoder && this.decoder.state !== "closed") this.decoder.close();
    this.decoder = null;
    this.node?.disconnect();
    this.gain?.disconnect();
    void this.context?.close().catch(() => undefined);
    if (this.moduleUrl) URL.revokeObjectURL(this.moduleUrl);
    this.node = null;
    this.gain = null;
    this.context = null;
  }

  private fail(reason: LosslessStopReason): void {
    if (this.stopped) return;
    this.stop();
    this.events.onStopped(reason);
  }

  private noteUnderrun(): void {
    const now = performance.now();
    this.underruns = this.underruns.filter(
      (at) => now - at < UNDERRUN_WINDOW_MS,
    );
    this.underruns.push(now);
    if (this.underruns.length >= MAX_UNDERRUNS) this.fail("underrun");
  }

  private configure(header: LosslessHeader): void {
    if (
      header.sampleRate !== SAMPLE_RATE ||
      header.channels !== CHANNELS ||
      typeof header.description !== "string"
    ) {
      this.fail("unsupported");
      return;
    }
    const decoder = new AudioDecoder({
      output: (data) => this.play(data),
      error: () => this.fail("decoder"),
    });
    decoder.configure({
      codec: "flac",
      sampleRate: SAMPLE_RATE,
      numberOfChannels: CHANNELS,
      description: decodeBase64(header.description),
    });
    this.decoder = decoder;
  }

  private play(data: AudioData): void {
    try {
      const frames = data.numberOfFrames;
      const left = new Float32Array(frames);
      const right = new Float32Array(frames);
      data.copyTo(left, { planeIndex: 0, format: "f32-planar" });
      data.copyTo(right, { planeIndex: 1, format: "f32-planar" });
      this.node?.port.postMessage({ type: "audio", left, right }, [
        left.buffer,
        right.buffer,
      ]);
    } finally {
      data.close();
    }
  }

  private readonly onMessage = (event: MessageEvent<unknown>): void => {
    if (this.stopped) return;
    if (typeof event.data === "string") {
      let message: { type?: string; reason?: string };
      try {
        message = JSON.parse(event.data) as { type?: string; reason?: string };
      } catch {
        return;
      }
      if (message.type === "flac") this.configure(message as LosslessHeader);
      else if (message.type === "stopped") {
        this.fail(message.reason === "congested" ? "congested" : "closed");
      }
      return;
    }
    if (!(event.data instanceof ArrayBuffer) || event.data.byteLength <= 8) {
      return;
    }
    const decoder = this.decoder;
    if (!decoder || decoder.state !== "configured") return;
    const first = new DataView(event.data).getBigUint64(0);
    decoder.decode(
      new EncodedAudioChunk({
        type: "key",
        timestamp: Number((first * 1_000_000n) / BigInt(SAMPLE_RATE)),
        data: new Uint8Array(event.data, 8),
      }),
    );
  };

  private readonly onClose = (): void => this.fail("closed");
}
