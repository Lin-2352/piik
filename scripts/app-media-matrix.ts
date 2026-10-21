// Measures the App's native capture path end to end on a real display:
// codec x resolution x frame rate x bitrate cells, reliability loops and
// endurance runs. It drives the App's loopback control socket directly and
// receives every edge in a headful Chrome page, so results include capture,
// encode, Pion forwarding and Browser decode. Usage:
//   npx tsx scripts/app-media-matrix.ts matrix|reliability|endurance [--key=value ...]
// Options: --codecs=h264,vp8 --source=display|window --seconds=15 --warmup=5
//   --fps=60,90,120,240 --bitrates=20,51,100,160 --resolutions=source,1440p,1080p,720p
//   --repeat=10 --duration=3600 --profile=source@120@51 --edges=1
import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import { appendFile, mkdir, mkdtemp, open, writeFile } from "node:fs/promises";
import { createServer } from "node:http";
import { cpus, tmpdir } from "node:os";
import { join, resolve } from "node:path";

import { readAppEndpoint, type AppEndpoint } from "./app-gate-endpoint";
import {
  CdpConnection,
  cleanupRun,
  createPage,
  evaluate,
  launchChrome,
  reservePort,
  waitForVersion,
  type PageHandle,
} from "./browser-gate-harness";
import {
  NATIVE_CLIENT_PROTOCOL,
  NATIVE_CLIENT_SUBPROTOCOL,
} from "../src/client/native/wire";

const ROOT = resolve(import.meta.dirname, "..");
const PATTERN_TITLE = "Piik Matrix Pattern";

interface Profile {
  resolution: string;
  maxFramerate: number;
  maxBitrate: number;
  degradationPreference: string;
}

const options = new Map(
  process.argv.slice(3).map((argument) => {
    const [key, value = "true"] = argument.replace(/^--/, "").split("=", 2);
    return [key!, value];
  }),
);
const scenario = process.argv[2] ?? "matrix";
const option = (key: string, fallback: string) => options.get(key) ?? fallback;
const list = (key: string, fallback: string) => option(key, fallback).split(",").filter(Boolean);
const numbers = (key: string, fallback: string) => list(key, fallback).map(Number);

function profile(resolution: string, fps: number, mbps: number): Profile {
  return {
    resolution,
    maxFramerate: fps,
    maxBitrate: Math.round(mbps * 1_000_000),
    degradationPreference: option("preference", "balanced"),
  };
}

function parseProfile(value: string): Profile {
  const [resolution, fps, mbps] = value.split("@");
  return profile(resolution!, Number(fps), Number(mbps));
}

// One deterministic drawing function renders the captured source and, in the
// receiving page, the reference frame its barcode names (integer rectangles and
// unscaled blits only, so the reference is exact).
const PATTERN_JS = String.raw`
const BAR_BITS = 24, BAR_BLOCK = 32;
let detail = null;
function patternDetail() {
  if (detail) return detail;
  detail = document.createElement("canvas");
  detail.width = 512; detail.height = 256;
  const context = detail.getContext("2d");
  const image = context.createImageData(512, 256);
  let seed = 0x2545f491;
  for (let index = 0; index < 512 * 256; index++) {
    seed ^= seed << 13; seed ^= seed >>> 17; seed ^= seed << 5;
    const row = Math.floor(index / 512);
    const value = (row % 8 < 6 && (seed & 3) !== 0) ? 20 : 235;
    image.data[index * 4] = value; image.data[index * 4 + 1] = value;
    image.data[index * 4 + 2] = value; image.data[index * 4 + 3] = 255;
  }
  context.putImageData(image, 0, 0);
  return detail;
}
function drawPattern(context, width, height, index) {
  context.imageSmoothingEnabled = false;
  const tile = 64;
  const shift = (index * 8) % (tile * 4096);
  const first = Math.floor(shift / tile), offset = shift % tile;
  for (let ty = 0; ty * tile < height; ty++) {
    for (let tx = 0; tx * tile - offset < width; tx++) {
      let value = Math.imul(tx + first, 73856093) ^ Math.imul(ty, 19349663);
      value ^= value >>> 13; value = Math.imul(value, 1274126177); value ^= value >>> 16;
      context.fillStyle = "rgb(" + (value & 255) + "," + ((value >>> 8) & 255) + "," + ((value >>> 16) & 255) + ")";
      context.fillRect(tx * tile - offset, ty * tile, tile, tile);
    }
  }
  const text = patternDetail();
  context.drawImage(text, 64, height - 320);
  context.drawImage(text, (index * 3) % Math.max(1, width - 512), Math.floor(height / 2) - 128);
  context.fillStyle = "#000";
  context.fillRect(0, 0, (BAR_BITS + 2) * BAR_BLOCK, BAR_BLOCK);
  context.fillStyle = "#fff";
  context.fillRect(0, 0, BAR_BLOCK, BAR_BLOCK);
  for (let bit = 0; bit < BAR_BITS; bit++) {
    if ((index >>> bit) & 1) context.fillRect((bit + 2) * BAR_BLOCK, 0, BAR_BLOCK, BAR_BLOCK);
  }
}
// The sync blocks are known white and black. Windows may scale the whole
// composition (panel brightness under Auto Color Management), so they also
// calibrate the reference: white/black are the captured levels of 255 and 0.
function readBarcode(context) {
  const data = context.getImageData(0, 0, (BAR_BITS + 2) * BAR_BLOCK, BAR_BLOCK).data;
  const luma = (x) => {
    const at = (Math.floor(BAR_BLOCK / 2) * (BAR_BITS + 2) * BAR_BLOCK + x) * 4;
    return 0.2126 * data[at] + 0.7152 * data[at + 1] + 0.0722 * data[at + 2];
  };
  const white = luma(BAR_BLOCK / 2), black = luma(BAR_BLOCK + BAR_BLOCK / 2);
  if (white - black < 40) return null;
  const threshold = (white + black) / 2;
  let index = 0;
  for (let bit = 0; bit < BAR_BITS; bit++) {
    if (luma((bit + 2) * BAR_BLOCK + BAR_BLOCK / 2) > threshold) index |= 1 << bit;
  }
  return { index, white, black };
}
`;

const PATTERN_HTML = `<!doctype html><title>${PATTERN_TITLE}</title>
<style>html,body{margin:0;overflow:hidden;background:#000;cursor:none}canvas{display:block}</style>
<canvas></canvas><script>${PATTERN_JS}
const canvas = document.querySelector("canvas");
const context = canvas.getContext("2d", { alpha: false });
let frame = 0;
function resize() {
  canvas.width = Math.round(innerWidth * devicePixelRatio);
  canvas.height = Math.round(innerHeight * devicePixelRatio);
  canvas.style.width = innerWidth + "px"; canvas.style.height = innerHeight + "px";
}
addEventListener("resize", resize); resize();
// ?noise overlays changing grain below the barcode, so the encoder spends
// most of its ceiling and the run measures real throughput.
const noise = new URLSearchParams(location.search).has("noise");
const noiseTiles = noise ? Array.from({ length: 4 }, () => {
  const tile = document.createElement("canvas");
  tile.width = tile.height = 512;
  const tileContext = tile.getContext("2d");
  const image = tileContext.createImageData(512, 512);
  for (let index = 0; index < image.data.length; index++) image.data[index] = index % 4 === 3 ? 255 : Math.random() * 255;
  tileContext.putImageData(image, 0, 0);
  return tile;
}) : [];
function tick() {
  drawPattern(context, canvas.width, canvas.height, frame++);
  if (noise) {
    // Film-grain strength: hard to compress like real video, not degenerate.
    context.globalAlpha = 0.12;
    for (let y = 256; y < canvas.height; y += 512) {
      for (let x = 0; x < canvas.width; x += 512) context.drawImage(noiseTiles[(Math.floor(x / 512) + Math.floor(y / 512) + frame) % 4], x, y);
    }
    context.globalAlpha = 1;
  }
  requestAnimationFrame(tick);
}
requestAnimationFrame(tick);
globalThis.patternState = () => ({ width: canvas.width, height: canvas.height, frame });
globalThis.startTone = async () => {
  const context = new AudioContext({ sampleRate: 48000 });
  const noise = context.createBuffer(2, context.sampleRate * 2, context.sampleRate);
  for (let channel = 0; channel < 2; channel++) {
    const data = noise.getChannelData(channel);
    for (let index = 0; index < data.length; index++) data[index] = Math.random() * 2 - 1;
  }
  const source = context.createBufferSource();
  source.buffer = noise;
  source.loop = true;
  const band = context.createBiquadFilter();
  band.type = "bandpass";
  band.frequency.value = 2500;
  band.Q.value = 0.4;
  const tone = context.createOscillator();
  tone.frequency.value = 440;
  const gain = context.createGain();
  gain.gain.value = 0.04;
  source.connect(band).connect(gain);
  tone.connect(gain);
  gain.connect(context.destination);
  source.start();
  tone.start();
  await context.resume();
  globalThis.stopTone = () => context.close();
  return context.state;
};
</script>`;

// The receiving page keeps one control socket and its edges alive across
// steps; Node drives it through small calls and polls sample() every second.
const CONTROL_JS = String.raw`
const matrix = {
  socket: null, sequence: 0, pending: new Map(), edges: new Map(), events: [],
  endpoint: null, shareId: null, edgeSequence: 0, pattern: null,
  async connect(endpoint, subprotocol) {
    // A restarted App starts clean: the previous socket's close and edges are history.
    for (const edge of this.edges.values()) { edge.peer.close(); edge.video.remove(); }
    this.edges.clear(); this.events = []; this.shareId = null; this.lastQuality = null;
    this.endpoint = endpoint;
    const socket = new WebSocket("ws://127.0.0.1:" + endpoint.port + "/control", [subprotocol + "." + endpoint.instanceToken]);
    await new Promise((resolve, reject) => { socket.onopen = resolve; socket.onerror = () => reject(new Error("control open failed")); });
    socket.onmessage = (event) => {
      const message = JSON.parse(String(event.data));
      const active = typeof message.id === "string" ? this.pending.get(message.id) : null;
      if (active) { this.pending.delete(message.id); clearTimeout(active.timer); active.resolve(message); return; }
      if (message.type === "edge-candidate") {
        const edge = this.edges.get(message.connectionId);
        if (edge?.described) edge.peer.addIceCandidate(message.candidate).catch(() => {});
        else edge?.queued.push(message.candidate);
        return;
      }
      if (message.type === "edge-state") {
        const edge = this.edges.get(message.connectionId);
        if (edge) edge.state = message.state;
      }
      if (message.type !== "edge-quality") this.events.push({ at: Date.now(), ...message });
      else this.lastQuality = message;
    };
    socket.onclose = () => {
      this.events.push({ at: Date.now(), type: "control-closed" });
      for (const active of this.pending.values()) { clearTimeout(active.timer); active.reject(new Error("control closed")); }
      this.pending.clear();
    };
    this.socket = socket;
    const ready = await this.request("hello");
    if (ready.type !== "ready") throw new Error("hello rejected: " + JSON.stringify(ready));
    return true;
  },
  request(type, fields = {}, timeoutMs = 20000) {
    const id = "matrix_" + String(++this.sequence).padStart(8, "0");
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => { this.pending.delete(id); reject(new Error("request timed out: " + type)); }, timeoutMs);
      this.pending.set(id, { resolve, reject, timer });
      this.socket.send(JSON.stringify({ version: ${NATIVE_CLIENT_PROTOCOL}, id, type, ...fields }));
    });
  },
  async selection(kind, title) {
    const options = await this.request("capture-options");
    const adapters = options.adapters ?? [];
    const adapter = adapters.find((candidate) => /nvidia/i.test(candidate.name ?? "") && candidate.hardwareH264?.length) ??
      adapters.find((candidate) => candidate.hardwareH264?.length) ?? adapters[0];
    const sources = (await this.request("list-sources")).sources ?? [];
    const source = sources.find((candidate) => candidate.kind === kind && (kind === "display" || candidate.title?.includes(title)));
    if (!adapter || !source) throw new Error("capture selection unavailable: " + JSON.stringify({ adapters, sources: sources.map((s) => s.title) }));
    return { adapter, source, encoder: adapter.hardwareH264?.[0]?.index ?? 0 };
  },
  async start({ codec, kind, title, profile, edges, audio }) {
    const { adapter, source, encoder } = await this.selection(kind, title);
    this.shareId = "matrix_share_" + Date.now();
    const started = Date.now();
    const response = await this.request("start-share", {
      shareId: this.shareId, codec, source, audio: !!audio, adapterIndex: adapter.index,
      encoderIndex: encoder, edgeCapacity: Math.max(1, edges), profile: { ...profile, screenAudioQuality: profile.screenAudioQuality ?? "music" },
    }, 30000);
    if (response.type !== "share-started") return { ok: false, response, ms: Date.now() - started };
    this.source = source; this.adapter = adapter; this.encoder = encoder;
    for (let edge = 0; edge < edges; edge++) await this.addEdge();
    return { ok: true, response, ms: Date.now() - started, adapter: adapter.name, source: source.title };
  },
  async addEdge() {
    const connectionId = "matrix_edge_" + String(++this.edgeSequence).padStart(6, "0");
    const peer = new RTCPeerConnection();
    const video = document.createElement("video");
    video.autoplay = true; video.muted = true; video.playsInline = true;
    video.style.width = "320px";
    document.body.append(video);
    const stream = new MediaStream();
    video.srcObject = stream;
    peer.ondatachannel = (event) => {
      if (event.channel.label === "piik-lossless-audio") edge.losslessChannel = event.channel;
    };
    const edge = { connectionId, peer, video, stream, described: false, queued: [], state: "new",
      presented: 0, previous: null };
    const frameCallback = () => { edge.presented++; video.requestVideoFrameCallback(frameCallback); };
    video.requestVideoFrameCallback(frameCallback);
    this.edges.set(connectionId, edge);
    peer.ontrack = (event) => { if (!stream.getTrackById(event.track.id)) stream.addTrack(event.track); video.play().catch(() => {}); };
    peer.onicecandidate = (event) => {
      this.request("edge-candidate", { shareId: this.shareId, connectionId, candidate: event.candidate?.toJSON() ?? null }).catch(() => {});
    };
    const offer = await this.request("prepare-edge", { shareId: this.shareId, connectionId, iceServers: [] });
    if (offer.type !== "edge-offer" && !offer.sdp) throw new Error("prepare-edge failed: " + JSON.stringify(offer));
    await peer.setRemoteDescription({ type: "offer", sdp: offer.sdp });
    edge.described = true;
    for (const candidate of edge.queued.splice(0)) await peer.addIceCandidate(candidate).catch(() => {});
    const answer = await peer.createAnswer();
    await peer.setLocalDescription(answer);
    await this.request("edge-answer", { shareId: this.shareId, connectionId, sdp: answer.sdp });
    return connectionId;
  },
  async audioStats(seconds) {
    const edge = [...this.edges.values()][0];
    const read = async () => {
      const report = await edge.peer.getStats();
      let inbound = null;
      const codecs = new Map();
      report.forEach((stat) => {
        if (stat.type === "inbound-rtp" && stat.kind === "audio") inbound = stat;
        if (stat.type === "codec") codecs.set(stat.id, stat);
      });
      return { bytes: inbound?.bytesReceived ?? 0, codec: inbound ? codecs.get(inbound.codecId) : null };
    };
    const first = await read();
    await new Promise((resolve) => setTimeout(resolve, seconds * 1000));
    const last = await read();
    return { kbps: ((last.bytes - first.bytes) * 8) / seconds / 1000,
      mimeType: last.codec?.mimeType ?? null, fmtp: last.codec?.sdpFmtpLine ?? null };
  },
  async losslessProbe(seconds) {
    const edge = [...this.edges.values()][0];
    const channel = edge?.losslessChannel;
    if (!channel) return { error: "no lossless channel in the edge offer" };
    if (channel.readyState !== "open") {
      await new Promise((resolve) => channel.addEventListener("open", resolve, { once: true }));
    }
    const result = { header: null, frames: 0, decoded: 0, decodedFrames: 0, errors: [], gaps: 0, bytes: 0,
      sampleRate: null, channels: null, rms: 0, kbps: 0, supported: null };
    let decoder = null;
    let lastIndex = null;
    let sumSquares = 0;
    let count = 0;
    channel.binaryType = "arraybuffer";
    channel.onmessage = (event) => {
      if (typeof event.data === "string") {
        const message = JSON.parse(event.data);
        if (message.type !== "flac") { result.errors.push("message " + event.data); return; }
        result.header = { sampleRate: message.sampleRate, channels: message.channels, frameSamples: message.frameSamples };
        const raw = atob(message.description);
        const description = new Uint8Array(raw.length);
        for (let index = 0; index < raw.length; index++) description[index] = raw.charCodeAt(index);
        const config = { codec: "flac", sampleRate: 48000, numberOfChannels: 2, description };
        AudioDecoder.isConfigSupported(config).then((support) => { result.supported = support.supported; });
        decoder = new AudioDecoder({
          output: (data) => {
            result.decoded++;
            result.decodedFrames += data.numberOfFrames;
            result.sampleRate = data.sampleRate;
            result.channels = data.numberOfChannels;
            const left = new Float32Array(data.numberOfFrames);
            data.copyTo(left, { planeIndex: 0, format: "f32-planar" });
            for (const value of left) { sumSquares += value * value; count++; }
            data.close();
          },
          error: (error) => result.errors.push(String(error)),
        });
        decoder.configure(config);
        return;
      }
      const index = Number(new DataView(event.data).getBigUint64(0));
      if (lastIndex !== null && index !== lastIndex + 960) result.gaps++;
      lastIndex = index;
      result.frames++;
      result.bytes += event.data.byteLength;
      decoder?.decode(new EncodedAudioChunk({ type: "key", timestamp: (index * 1e6) / 48000,
        data: new Uint8Array(event.data, 8) }));
    };
    channel.send(JSON.stringify({ type: "start" }));
    await new Promise((resolve) => setTimeout(resolve, seconds * 1000));
    channel.send(JSON.stringify({ type: "stop" }));
    await decoder?.flush().catch((error) => result.errors.push(String(error)));
    result.rms = count ? Math.sqrt(sumSquares / count) : 0;
    result.kbps = (result.bytes * 8) / seconds / 1000;
    return result;
  },
  async closeEdge(connectionId) {
    const edge = this.edges.get(connectionId);
    if (!edge) return;
    this.edges.delete(connectionId);
    edge.peer.close(); edge.video.remove();
    await this.request("close-edge", { shareId: this.shareId, connectionId }).catch(() => {});
  },
  async reconnectFirstEdge() {
    const first = [...this.edges.keys()][0];
    if (first) await this.closeEdge(first);
    return this.addEdge();
  },
  async update(profile) {
    const started = Date.now();
    const response = await this.request("update-share", { shareId: this.shareId, profile: { ...profile, screenAudioQuality: "music" } }, 30000);
    return { ok: response.type === "share-updated", response, ms: Date.now() - started };
  },
  async replace(kind, title) {
    const { adapter, source, encoder } = await this.selection(kind, title);
    const started = Date.now();
    const response = await this.request("replace-share-source", {
      shareId: this.shareId, source, audio: false, adapterIndex: adapter.index, encoderIndex: encoder }, 30000);
    return { ok: response.type === "share-source-replaced", response, ms: Date.now() - started, source: source.title };
  },
  async pause(paused) {
    const response = await this.request("pause-share", { shareId: this.shareId, paused });
    return { ok: response.type !== "request-failed", response };
  },
  async stop() {
    for (const id of [...this.edges.keys()]) { const edge = this.edges.get(id); edge.peer.close(); edge.video.remove(); this.edges.delete(id); }
    if (!this.shareId) return { ok: true };
    const response = await this.request("stop-share", { shareId: this.shareId }).catch((error) => ({ type: "error", error: String(error) }));
    this.shareId = null;
    return { ok: response.type === "share-stopped", response };
  },
  psnr(edge) {
    const video = edge.video, pattern = this.pattern;
    if (!pattern || video.videoWidth !== pattern.width || video.videoHeight !== pattern.height) return null;
    const width = video.videoWidth, height = video.videoHeight;
    this.frameCanvas ??= document.createElement("canvas");
    this.referenceCanvas ??= document.createElement("canvas");
    for (const canvas of [this.frameCanvas, this.referenceCanvas]) {
      if (canvas.width !== width || canvas.height !== height) { canvas.width = width; canvas.height = height; }
    }
    const frame = this.frameCanvas.getContext("2d", { willReadFrequently: true });
    frame.drawImage(video, 0, 0);
    const barcode = readBarcode(frame);
    if (barcode === null) return { index: null, psnr: null };
    const { index, white, black } = barcode;
    const gain = (white - black) / 255;
    const reference = this.referenceCanvas.getContext("2d", { willReadFrequently: true });
    drawPattern(reference, width, height, index);
    const cropWidth = Math.min(1024, width), cropHeight = Math.min(768, height - 64);
    const x = Math.floor((width - cropWidth) / 2), y = Math.max(64, Math.floor((height - cropHeight) / 2));
    const a = frame.getImageData(x, y, cropWidth, cropHeight).data;
    const b = reference.getImageData(x, y, cropWidth, cropHeight).data;
    let error = 0;
    for (let at = 0; at < a.length; at += 4) {
      const difference = (0.2126 * a[at] + 0.7152 * a[at + 1] + 0.0722 * a[at + 2]) -
        (black + gain * (0.2126 * b[at] + 0.7152 * b[at + 1] + 0.0722 * b[at + 2]));
      error += difference * difference;
    }
    const mse = error / (a.length / 4);
    if (this.wantSnapshot) {
      this.wantSnapshot = false;
      const crop = (source) => {
        const canvas = document.createElement("canvas");
        canvas.width = cropWidth; canvas.height = cropHeight;
        canvas.getContext("2d").drawImage(source, x, y, cropWidth, cropHeight, 0, 0, cropWidth, cropHeight);
        return canvas.toDataURL("image/png");
      };
      this.snapshot = { index, frame: crop(this.frameCanvas), reference: crop(this.referenceCanvas) };
    }
    // Peak is the captured white level, so a dimmed composition is not scored as noise.
    const peak = Math.max(1, white - black);
    return { index, gain, psnr: mse === 0 ? 99 : 10 * Math.log10(peak * peak / mse) };
  },
  async sample(withPsnr) {
    const now = performance.now();
    const edges = [];
    for (const edge of this.edges.values()) {
      const current = { now, presented: edge.presented };
      for (const [, report] of await edge.peer.getStats()) {
        if (report.type === "inbound-rtp" && report.kind === "video") {
          Object.assign(current, {
            framesDecoded: report.framesDecoded ?? 0, framesDropped: report.framesDropped ?? 0,
            framesReceived: report.framesReceived ?? 0, bytes: report.bytesReceived ?? 0,
            packetsLost: report.packetsLost ?? 0, nack: report.nackCount ?? 0, pli: report.pliCount ?? 0,
            freezes: report.freezeCount ?? 0, freezeSeconds: report.totalFreezesDuration ?? 0,
            decodeSeconds: report.totalDecodeTime ?? 0, jitterDelay: report.jitterBufferDelay ?? 0,
            jitterEmitted: report.jitterBufferEmittedCount ?? 0, width: report.frameWidth ?? 0,
            height: report.frameHeight ?? 0, codecId: report.codecId, decoder: report.decoderImplementation,
            powerEfficientDecoder: report.powerEfficientDecoder,
          });
        }
      }
      if (current.codecId) {
        const stats = await edge.peer.getStats();
        current.codec = stats.get(current.codecId)?.mimeType ?? null;
      }
      const previous = edge.previous;
      edge.previous = current;
      if (!previous || current.framesDecoded === undefined) {
        edges.push({ connectionId: edge.connectionId, state: edge.state, warming: true });
        continue;
      }
      const seconds = (now - previous.now) / 1000;
      const decoded = current.framesDecoded - (previous.framesDecoded ?? 0);
      edges.push({
        connectionId: edge.connectionId, state: edge.state, codec: current.codec, decoder: current.decoder,
        powerEfficientDecoder: current.powerEfficientDecoder,
        width: current.width, height: current.height,
        decodedFps: decoded / seconds,
        presentedFps: (current.presented - previous.presented) / seconds,
        droppedFrames: current.framesDropped - (previous.framesDropped ?? 0),
        mbps: (current.bytes - (previous.bytes ?? 0)) * 8 / seconds / 1e6,
        packetsLost: current.packetsLost - (previous.packetsLost ?? 0),
        nack: current.nack - (previous.nack ?? 0), pli: current.pli - (previous.pli ?? 0),
        freezes: current.freezes - (previous.freezes ?? 0),
        freezeSeconds: current.freezeSeconds - (previous.freezeSeconds ?? 0),
        decodeMs: decoded > 0 ? (current.decodeSeconds - previous.decodeSeconds) * 1000 / decoded : null,
        jitterBufferMs: current.jitterEmitted > (previous.jitterEmitted ?? 0)
          ? (current.jitterDelay - previous.jitterDelay) * 1000 / (current.jitterEmitted - previous.jitterEmitted) : null,
        totalDecoded: current.framesDecoded,
        quality: withPsnr ? this.psnr(edge) : undefined,
      });
    }
    // Events of a share this page already stopped must not fail the next one.
    // Only the live share's still-open edges count: an edge closed on purpose
    // (viewer reconnect) reports "closed" after it has left this.edges.
    const events = this.events.splice(0).filter((event) => (!event.shareId || event.shareId === this.shareId) &&
      (event.type !== "edge-state" || this.edges.has(event.connectionId)));
    return { edges, events, quality: this.lastQuality ?? null, socketOpen: this.socket?.readyState === 1 };
  },
};
// Browser method: Chrome captures the screen and encodes it itself, sending to a
// loopback receiver whose stats and PSNR use the same sampler as native edges.
matrix.startBrowser = async function ({ codec, fps, bitrate }) {
  await this.stopBrowser();
  const display = await navigator.mediaDevices.getDisplayMedia({
    video: { width: 2560, height: 1600, frameRate: { ideal: fps, max: fps } }, audio: false });
  const track = display.getVideoTracks()[0];
  track.contentHint = "detail";
  const sender = new RTCPeerConnection(), receiver = new RTCPeerConnection();
  sender.onicecandidate = (event) => event.candidate && receiver.addIceCandidate(event.candidate);
  receiver.onicecandidate = (event) => event.candidate && sender.addIceCandidate(event.candidate);
  const transceiver = sender.addTransceiver(track, { direction: "sendonly" });
  const codecs = RTCRtpSender.getCapabilities("video").codecs.filter((c) => c.mimeType.toLowerCase() === "video/" + codec);
  if (!codecs.length) throw new Error("browser cannot encode " + codec);
  transceiver.setCodecPreferences(codecs);
  const video = document.createElement("video");
  video.autoplay = true; video.muted = true; video.style.width = "320px"; document.body.append(video);
  const stream = new MediaStream(); video.srcObject = stream;
  receiver.ontrack = (event) => { stream.addTrack(event.track); video.play().catch(() => {}); };
  const edge = { connectionId: "browser_" + codec, peer: receiver, video, stream, described: true, queued: [],
    state: "connected", presented: 0, previous: null, sender, display };
  const frameCallback = () => { edge.presented++; video.requestVideoFrameCallback(frameCallback); };
  video.requestVideoFrameCallback(frameCallback);
  const offer = await sender.createOffer();
  await sender.setLocalDescription(offer);
  await receiver.setRemoteDescription(offer);
  const answer = await receiver.createAnswer();
  // Raise the receiver's bandwidth ceiling so BWE does not cap the comparison.
  answer.sdp = answer.sdp.replace(/(m=video[^\r\n]*\r\n)/, "$1b=AS:" + Math.round(bitrate / 1000 * 1.2) + "\r\n");
  await receiver.setLocalDescription(answer);
  await sender.setRemoteDescription(answer);
  const parameters = transceiver.sender.getParameters();
  parameters.encodings[0].maxBitrate = bitrate;
  parameters.encodings[0].maxFramerate = fps;
  parameters.degradationPreference = "maintain-resolution";
  await transceiver.sender.setParameters(parameters);
  this.browser = edge;
  this.edges.set(edge.connectionId, edge);
  this.shareId = null;
  return { ok: true, ms: 0 };
};
matrix.stopBrowser = async function () {
  const edge = this.browser;
  if (!edge) return;
  this.browser = null;
  this.edges.delete(edge.connectionId);
  edge.display.getTracks().forEach((track) => track.stop());
  edge.sender.close(); edge.peer.close(); edge.video.remove();
};
matrix.browserSender = async function () {
  const edge = this.browser;
  if (!edge) return null;
  for (const [, report] of await edge.sender.getStats()) {
    if (report.type === "outbound-rtp" && report.kind === "video") {
      return { encoder: report.encoderImplementation, powerEfficient: report.powerEfficientEncoder,
        limitation: report.qualityLimitationReason, fps: report.framesPerSecond,
        width: report.frameWidth, height: report.frameHeight };
    }
  }
  return null;
};

// Reads the pattern's white barcode block and two tiles from three capture
// paths, so a brightness change can be attributed to one stage.
matrix.colorCheck = async function (kind) {
  const read = async (source, width, height) => {
    const canvas = document.createElement("canvas");
    canvas.width = width; canvas.height = height;
    const context = canvas.getContext("2d", { willReadFrequently: true });
    context.drawImage(source, 0, 0, width, height);
    // Previews letterbox the 2560x1600 source; map source pixels into the image.
    const scale = Math.min(width / 2560, height / 1600);
    const left = (width - 2560 * scale) / 2, top = (height - 1600 * scale) / 2;
    const pixel = (x, y) => Array.from(context.getImageData(Math.floor(left + x * scale), Math.floor(top + y * scale), 1, 1).data.slice(0, 3));
    return { white: pixel(16, 16), tileA: pixel(300, 300), tileB: pixel(600, 400) };
  };
  const result = {};
  const display = await navigator.mediaDevices.getDisplayMedia({ video: { width: 2560, height: 1600 } });
  const video = document.createElement("video");
  video.muted = true; video.srcObject = display; await video.play();
  await new Promise((resolve) => setTimeout(resolve, 1500));
  result.chromeCapture = await read(video, video.videoWidth, video.videoHeight);
  display.getTracks().forEach((track) => track.stop());
  const { source } = await this.selection(kind, "");
  const preview = await this.request("source-preview", { source });
  if (preview.data) {
    const bytes = Uint8Array.from(atob(preview.data), (character) => character.charCodeAt(0));
    const bitmap = await createImageBitmap(new Blob([bytes], { type: "image/bmp" }));
    result.helperPreview = await read(bitmap, bitmap.width, bitmap.height);
  } else result.helperPreview = preview;
  return result;
};
globalThis.matrix = matrix;
`;

const CONTROL_HTML = `<!doctype html><title>Piik Matrix Viewer</title>
<style>body{margin:0;background:#111;color:#ccc;font:12px sans-serif}</style>
<script>${PATTERN_JS}${CONTROL_JS}</script>`;

function startPageServer(port: number) {
  const server = createServer((request, response) => {
    const body = request.url?.startsWith("/pattern") ? PATTERN_HTML : CONTROL_HTML;
    response.writeHead(200, { "Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store" });
    response.end(body);
  });
  return new Promise<{ close(): Promise<void> }>((resolveServer, rejectServer) => {
    server.once("error", rejectServer);
    server.listen(port, "127.0.0.1", () => resolveServer({
      close: () => new Promise<void>((resolveClose) => server.close(() => resolveClose())),
    }));
  });
}

// Encoder statistics from the capture process reach the App's debug log.
class AppLog {
  private offset = 0;
  private partial = "";
  readonly encoder = new Map<string, Record<string, string>>();
  readonly failures: string[] = [];
  constructor(private readonly path: string) {}

  async poll(): Promise<void> {
    let handle;
    try {
      handle = await open(this.path, "r");
    } catch {
      return;
    }
    try {
      const { size } = await handle.stat();
      if (size < this.offset) this.offset = 0;
      if (size === this.offset) return;
      const buffer = Buffer.alloc(size - this.offset);
      await handle.read(buffer, 0, buffer.length, this.offset);
      this.offset = size;
      const text = this.partial + buffer.toString("utf8");
      const lines = text.split("\n");
      this.partial = lines.pop() ?? "";
      for (const line of lines) this.consume(line);
    } finally {
      await handle.close();
    }
  }

  private consume(line: string): void {
    if (line.includes("event=encoder-stats")) {
      const match = /"line":"([^"]*)"/.exec(line);
      if (!match) return;
      const fields = Object.fromEntries(
        match[1]!.split(" ").map((pair) => pair.split("=", 2) as [string, string]),
      );
      this.encoder.set(fields.output ?? "?", fields);
      return;
    }
    if (/capture-process-failed|"share-ended","share":"[^"]*","failed":true|native-operation-failed|stopped unexpectedly/.test(line)) {
      this.failures.push(line.slice(0, 600));
    }
  }
}

// Process CPU, memory and handles every second, plus NVIDIA engine utilisation.
class SystemSampler {
  private processes: ChildProcessWithoutNullStreams | null = null;
  private gpu: ChildProcessWithoutNullStreams | null = null;
  private previous = new Map<string, { at: number; cpu: number }>();
  latest: Record<string, unknown> = {};
  gpuLatest: Record<string, number> = {};

  start(): void {
    const script = [
      "$ErrorActionPreference='SilentlyContinue'",
      "while ($true) {",
      "  $rows = Get-Process piik-app,piik-capture,piik-capture-bin,chrome | ForEach-Object { [pscustomobject]@{ n=$_.ProcessName; c=$_.TotalProcessorTime.TotalMilliseconds; w=$_.WorkingSet64; h=$_.HandleCount } }",
      "  $groups = $rows | Group-Object n | ForEach-Object { [pscustomobject]@{ n=$_.Name; c=($_.Group | Measure-Object c -Sum).Sum; w=($_.Group | Measure-Object w -Sum).Sum; h=($_.Group | Measure-Object h -Sum).Sum; k=$_.Count } }",
      "  [pscustomobject]@{ t=[DateTimeOffset]::Now.ToUnixTimeMilliseconds(); p=@($groups) } | ConvertTo-Json -Compress -Depth 4",
      "  Start-Sleep -Milliseconds 1000",
      "}",
    ].join("\n");
    this.processes = spawn("powershell.exe", ["-NoProfile", "-NonInteractive", "-Command", script], {
      stdio: "pipe", windowsHide: true,
    });
    let buffered = "";
    this.processes.stdout.on("data", (chunk: Buffer) => {
      buffered += chunk.toString();
      const lines = buffered.split("\n");
      buffered = lines.pop() ?? "";
      for (const line of lines) this.consumeProcesses(line.trim());
    });
    this.processes.stderr.resume();
    this.gpu = spawn("nvidia-smi", ["dmon", "-s", "u", "-d", "1"], { stdio: "pipe", windowsHide: true });
    this.gpu.on("error", () => {});
    let gpuBuffered = "";
    this.gpu.stdout.on("data", (chunk: Buffer) => {
      gpuBuffered += chunk.toString();
      const lines = gpuBuffered.split("\n");
      gpuBuffered = lines.pop() ?? "";
      for (const line of lines) {
        if (line.startsWith("#") || !line.trim()) continue;
        const [, sm, mem, enc, dec] = line.trim().split(/\s+/).map(Number);
        this.gpuLatest = { sm: sm!, mem: mem!, enc: enc!, dec: dec! };
      }
    });
    this.gpu.stderr.resume();
  }

  private consumeProcesses(line: string): void {
    if (!line.startsWith("{")) return;
    const value = JSON.parse(line) as { t: number; p: Array<{ n: string; c: number; w: number; h: number; k: number }> };
    const next: Record<string, unknown> = {};
    for (const row of value.p) {
      const before = this.previous.get(row.n);
      this.previous.set(row.n, { at: value.t, cpu: row.c });
      next[row.n] = {
        cores: before ? Math.max(0, (row.c - before.cpu) / (value.t - before.at)) : null,
        workingSetMB: Math.round(row.w / 1048576),
        handles: row.h,
        processes: row.k,
      };
    }
    this.latest = next;
  }

  stop(): void {
    this.processes?.kill();
    this.gpu?.kill();
  }
}

interface Harness {
  cdp: CdpConnection;
  control: PageHandle;
  pattern: PageHandle;
  log: AppLog;
  system: SystemSampler;
  runDir: string;
  startApp(): Promise<void>;
  stopApp(): Promise<void>;
}

async function call<T>(harness: Harness, expression: string, timeoutMs = 60_000): Promise<T> {
  return evaluate<T>(harness.cdp, harness.control, expression, Date.now() + timeoutMs);
}

const delay = (ms: number) => new Promise((resolveDelay) => setTimeout(resolveDelay, ms));

interface Sample {
  at: number;
  edges: Array<Record<string, any>>;
  events: Array<Record<string, any>>;
  quality: Record<string, any> | null;
  socketOpen: boolean;
  encoder: Record<string, string> | null;
  encoderLow: Record<string, string> | null;
  system: Record<string, unknown>;
  gpu: Record<string, number>;
}

async function sample(harness: Harness, withPsnr: boolean): Promise<Sample> {
  await harness.log.poll();
  const page = await call<Omit<Sample, "at" | "encoder" | "encoderLow" | "system" | "gpu">>(
    harness, `matrix.sample(${withPsnr})`, 15_000);
  const result: Sample = {
    at: Date.now(),
    ...page,
    encoder: harness.log.encoder.get("1") ?? harness.log.encoder.get("0") ?? null,
    encoderLow: harness.log.encoder.get("0") ?? null,
    system: harness.system.latest,
    gpu: harness.system.gpuLatest,
  };
  await appendFile(join(harness.runDir, "samples.jsonl"), JSON.stringify(result) + "\n");
  return result;
}

function failureEvent(sampleValue: Sample): string | null {
  for (const event of sampleValue.events) {
    if (event.type === "share-ended") return "share-ended" + (event.failed ? " (failed)" : "");
    if (event.type === "control-closed") return "control socket closed";
    if (event.type === "edge-state" && (event.state === "failed" || event.state === "closed")) {
      return "edge " + event.state;
    }
  }
  return sampleValue.socketOpen ? null : "control socket closed";
}

// Frames must keep reaching every edge after an operation.
async function verifyFlow(harness: Harness, timeoutMs = 15_000): Promise<{ ok: boolean; ms: number; detail: string }> {
  const started = Date.now();
  let baseline: number[] | null = null;
  while (Date.now() - started < timeoutMs) {
    const value = await sample(harness, false);
    const failure = failureEvent(value);
    if (failure) return { ok: false, ms: Date.now() - started, detail: failure };
    const totals = value.edges.map((edge) => edge.totalDecoded ?? 0);
    if (totals.length === 0) return { ok: false, ms: Date.now() - started, detail: "no edges" };
    baseline ??= totals;
    if (totals.every((total, index) => total >= baseline![index]! + 30)) {
      return { ok: true, ms: Date.now() - started, detail: "" };
    }
    await delay(500);
  }
  return { ok: false, ms: timeoutMs, detail: "frames did not advance" };
}

function average(values: number[]): number | null {
  const finite = values.filter((value) => Number.isFinite(value));
  return finite.length ? finite.reduce((sum, value) => sum + value, 0) / finite.length : null;
}

function percentile(values: number[], fraction: number): number | null {
  const finite = values.filter((value) => Number.isFinite(value)).sort((a, b) => a - b);
  if (!finite.length) return null;
  return finite[Math.min(finite.length - 1, Math.floor(fraction * finite.length))]!;
}

interface CellResult {
  codec: string;
  profile: Profile;
  applied: boolean;
  failure: string | null;
  setupMs: number;
  outputs: string | null;
  codecReceived: string | null;
  decoder: string | null;
  width: number | null;
  height: number | null;
  decodedFps: number | null;
  decodedFpsP5: number | null;
  presentedFps: number | null;
  receivedMbps: number | null;
  dropped: number;
  freezes: number;
  packetsLost: number;
  psnr: number | null;
  psnrMin: number | null;
  encodeMs: number | null;
  encodeInputFps: number | null;
  encodeFps: number | null;
  encUtil: number | null;
  appCores: number | null;
  captureCores: number | null;
}

async function measure(harness: Harness, seconds: number): Promise<{ samples: Sample[]; failure: string | null }> {
  const samples: Sample[] = [];
  for (let second = 0; second < seconds; second++) {
    const started = Date.now();
    const value = await sample(harness, second % 2 === 1);
    samples.push(value);
    const failure = failureEvent(value);
    if (failure) return { samples, failure };
    await delay(Math.max(0, 1000 - (Date.now() - started)));
  }
  return { samples, failure: null };
}

function summarize(codec: string, target: Profile, setupMs: number, applied: boolean, samples: Sample[], failure: string | null): CellResult {
  const edges = samples.flatMap((value) => value.edges.filter((edge) => !edge.warming));
  const psnr = edges.map((edge) => edge.quality?.psnr).filter((value): value is number => typeof value === "number");
  const encoder = samples.map((value) => value.encoder).filter((value): value is Record<string, string> => !!value);
  const last = encoder.at(-1);
  const cores = (name: string) => average(samples.map((value) =>
    Number((value.system[name] as { cores?: number } | undefined)?.cores ?? NaN)));
  return {
    codec,
    profile: target,
    applied,
    failure,
    setupMs,
    outputs: last ? `${last.outputWidth}x${last.outputHeight}@${last.ceilingFps}` : null,
    codecReceived: edges.at(-1)?.codec ?? null,
    decoder: edges.at(-1)?.decoder ?? null,
    width: edges.at(-1)?.width ?? null,
    height: edges.at(-1)?.height ?? null,
    decodedFps: average(edges.map((edge) => edge.decodedFps)),
    decodedFpsP5: percentile(edges.map((edge) => edge.decodedFps), 0.05),
    presentedFps: average(edges.map((edge) => edge.presentedFps)),
    receivedMbps: average(edges.map((edge) => edge.mbps)),
    dropped: edges.reduce((sum, edge) => sum + (edge.droppedFrames ?? 0), 0),
    freezes: edges.reduce((sum, edge) => sum + (edge.freezes ?? 0), 0),
    packetsLost: edges.reduce((sum, edge) => sum + (edge.packetsLost ?? 0), 0),
    psnr: average(psnr),
    psnrMin: psnr.length ? Math.min(...psnr) : null,
    encodeMs: average(encoder.map((value) => Number(value.averageEncodeTimeMs))),
    encodeInputFps: average(encoder.map((value) => Number(value.inputFps))),
    encodeFps: average(encoder.map((value) => Number(value.encodeFps))),
    encUtil: average(samples.map((value) => value.gpu.enc ?? NaN)),
    appCores: cores("piik-app"),
    captureCores: cores("piik-capture-bin") ?? cores("piik-capture"),
  };
}

function fixed(value: number | null, digits = 1): string {
  return value === null ? "-" : value.toFixed(digits);
}

async function writeCell(harness: Harness, cell: CellResult): Promise<void> {
  await appendFile(join(harness.runDir, "cells.jsonl"), JSON.stringify(cell) + "\n");
  process.stdout.write(
    `${cell.codec} ${cell.profile.resolution}@${cell.profile.maxFramerate}fps/${cell.profile.maxBitrate / 1e6}M ` +
    `-> ${cell.failure ? "FAIL " + cell.failure : "ok"} out=${cell.outputs} rx=${cell.width}x${cell.height} ` +
    `fps=${fixed(cell.decodedFps)} p5=${fixed(cell.decodedFpsP5)} shown=${fixed(cell.presentedFps)} ` +
    `rx=${fixed(cell.receivedMbps)}Mbps psnr=${fixed(cell.psnr, 2)} min=${fixed(cell.psnrMin, 2)} ` +
    `enc=${fixed(cell.encodeMs, 2)}ms nvenc=${fixed(cell.encUtil, 0)}% drop=${cell.dropped} frz=${cell.freezes}\n`,
  );
}

// Saves the top-left 1024x640 of a received frame beside its reference.
async function saveSnapshot(harness: Harness, name: string): Promise<void> {
  await call(harness, "matrix.wantSnapshot = true; matrix.snapshot = null; matrix.sample(true)");
  const snapshot = await call<{ index: number; frame: string; reference: string } | null>(harness, "matrix.snapshot");
  if (!snapshot) return;
  for (const [kind, url] of [["frame", snapshot.frame], ["reference", snapshot.reference]] as const) {
    await writeFile(join(harness.runDir, `${name}-${snapshot.index}-${kind}.png`), Buffer.from(url.split(",")[1]!, "base64"));
  }
}

async function startShare(harness: Harness, codec: string, target: Profile): Promise<{ ok: boolean; ms: number; detail: string }> {
  const kind = option("source", "display");
  const result = await call<{ ok: boolean; ms: number; response: unknown }>(harness,
    `matrix.start(${JSON.stringify({ codec, kind, title: PATTERN_TITLE, profile: target, edges: Number(option("edges", "1")), audio: options.has("audio") })})`);
  await appendFile(join(harness.runDir, "responses.jsonl"), JSON.stringify(result.response) + "\n");
  return { ok: result.ok, ms: result.ms, detail: result.ok ? "" : JSON.stringify(result.response) };
}

async function stopShare(harness: Harness): Promise<void> {
  await call(harness, "matrix.stop()").catch(() => {});
}

// Browser capture and encode at Source, the same cells as the native matrix.
async function runBrowser(harness: Harness): Promise<void> {
  const seconds = Number(option("seconds", "15"));
  const warmup = Number(option("warmup", "5"));
  for (const codec of list("codecs", "av1,h264,vp8")) {
    for (const fps of numbers("fps", "60,90,120,240")) {
      for (const mbps of numbers("bitrates", "20,51,100,160")) {
        const target = profile("source", fps, mbps);
        let failure: string | null = null;
        let samples: Sample[] = [];
        const started = await call<{ ok: boolean }>(harness,
          `matrix.startBrowser(${JSON.stringify({ codec, fps, bitrate: target.maxBitrate })})`)
          .catch((error: Error) => ({ ok: false, error: error.message }));
        if (!started.ok) failure = "start: " + JSON.stringify(started);
        if (!failure) {
          const warm = await measure(harness, warmup);
          failure = warm.failure;
          if (!failure) {
            const measured = await measure(harness, seconds);
            samples = measured.samples;
            failure = measured.failure;
          }
        }
        const sender = await call<Record<string, unknown> | null>(harness, "matrix.browserSender()").catch(() => null);
        const cell = { ...summarize("browser-" + codec, target, 0, !failure, samples, failure), browserSender: sender };
        await writeCell(harness, cell);
        process.stdout.write(`  sender: ${JSON.stringify(sender)}\n`);
        await call(harness, "matrix.stopBrowser()").catch(() => {});
      }
    }
  }
}

async function runMatrix(harness: Harness): Promise<void> {
  const codecs = list("codecs", "h264");
  const seconds = Number(option("seconds", "15"));
  const warmup = Number(option("warmup", "5"));
  // --cells=1080p@240@160,720p@120@51 replays one exact sequence of live updates.
  const cells: Profile[] = options.has("cells") ? list("cells", "").map(parseProfile) : [];
  for (const resolution of options.has("cells") ? [] : list("resolutions", "source")) {
    for (const fps of numbers("fps", "60,90,120,240")) {
      for (const mbps of numbers("bitrates", "20,51,100,160")) cells.push(profile(resolution, fps, mbps));
    }
  }
  for (const codec of codecs) {
    let active = false;
    for (const target of cells) {
      let setupMs = 0;
      let applied = false;
      let failure: string | null = null;
      if (!active) {
        const started = await startShare(harness, codec, target);
        setupMs = started.ms;
        applied = started.ok;
        failure = started.ok ? null : "start: " + started.detail;
      } else {
        const updated = await call<{ ok: boolean; ms: number; response: unknown }>(harness,
          `matrix.update(${JSON.stringify(target)})`).catch((error: Error) => ({ ok: false, ms: 0, response: error.message }));
        await appendFile(join(harness.runDir, "responses.jsonl"), JSON.stringify(updated.response) + "\n");
        setupMs = updated.ms;
        applied = updated.ok;
        failure = updated.ok ? null : "update: " + JSON.stringify(updated.response);
      }
      let samples: Sample[] = [];
      if (!failure) {
        const warm = await measure(harness, warmup);
        failure = warm.failure;
        if (!failure) {
          const measured = await measure(harness, seconds);
          samples = measured.samples;
          failure = measured.failure;
        }
      }
      const cell = summarize(codec, target, setupMs, applied, samples, failure ?? (harness.log.failures.at(-1) && !applied ? harness.log.failures.at(-1)! : null));
      await writeCell(harness, cell);
      if (options.has("snapshot") && !failure) await saveSnapshot(harness, `${codec}-${target.resolution}-${target.maxFramerate}`);
      active = !failure;
      if (failure) await stopShare(harness);
    }
    await stopShare(harness);
    active = false;
  }
}

interface StepResult { step: string; iteration: number; ok: boolean; ms: number; detail: string }

async function runReliability(harness: Harness): Promise<void> {
  const repeat = Number(option("repeat", "10"));
  const codec = list("codecs", "h264")[0]!;
  const base = parseProfile(option("profile", "source@120@51"));
  const results: StepResult[] = [];
  const record = async (step: string, iteration: number, outcome: { ok: boolean; ms: number; detail: string }) => {
    const value = { step, iteration, ...outcome };
    results.push(value);
    await appendFile(join(harness.runDir, "steps.jsonl"), JSON.stringify(value) + "\n");
    process.stdout.write(`${step} #${iteration}: ${value.ok ? "ok" : "FAIL " + value.detail} (${value.ms} ms)\n`);
  };
  const ensure = async () => {
    const started = await startShare(harness, codec, base);
    if (!started.ok) return started;
    const flowing = await verifyFlow(harness);
    return { ok: flowing.ok, ms: started.ms + flowing.ms, detail: flowing.detail };
  };
  const otherKind = option("source", "display") === "display" ? "window" : "display";

  await record("initial-start", 0, await ensure());
  for (let iteration = 1; iteration <= repeat; iteration++) {
    await stopShare(harness);
    await record("stop-restart", iteration, await ensure());
  }
  for (let iteration = 1; iteration <= repeat; iteration++) {
    await stopShare(harness);
    const kind = iteration % 2 ? otherKind : option("source", "display");
    const started = await call<{ ok: boolean; ms: number; response: unknown }>(harness,
      `matrix.start(${JSON.stringify({ codec, kind, title: PATTERN_TITLE, profile: base, edges: Number(option("edges", "1")) })})`);
    const flowing = started.ok ? await verifyFlow(harness) : { ok: false, ms: 0, detail: JSON.stringify(started.response) };
    await record("reselect-" + kind, iteration, { ok: flowing.ok, ms: started.ms + flowing.ms, detail: flowing.detail });
  }
  await stopShare(harness);
  await record("restart-for-live", 0, await ensure());
  for (let iteration = 1; iteration <= repeat; iteration++) {
    const kind = iteration % 2 ? otherKind : option("source", "display");
    const replaced = await call<{ ok: boolean; ms: number; response: unknown }>(harness,
      `matrix.replace(${JSON.stringify(kind)}, ${JSON.stringify(PATTERN_TITLE)})`)
      .catch((error: Error) => ({ ok: false, ms: 0, response: error.message }));
    const flowing = replaced.ok ? await verifyFlow(harness) : { ok: false, ms: 0, detail: JSON.stringify(replaced.response) };
    await record("switch-live-" + kind, iteration, { ok: flowing.ok, ms: replaced.ms + flowing.ms, detail: flowing.detail });
  }
  const live = [
    profile("1080p", 60, 20), profile("source", 60, 100), profile("720p", 120, 10),
    profile("1440p", 120, 100), profile("source", 120, 51), profile("480p", 30, 2),
  ];
  for (let iteration = 1; iteration <= repeat; iteration++) {
    const target = live[(iteration - 1) % live.length]!;
    const updated = await call<{ ok: boolean; ms: number; response: unknown }>(harness, `matrix.update(${JSON.stringify(target)})`)
      .catch((error: Error) => ({ ok: false, ms: 0, response: error.message }));
    const flowing = updated.ok ? await verifyFlow(harness) : { ok: false, ms: 0, detail: JSON.stringify(updated.response) };
    await record(`update-live-${target.resolution}@${target.maxFramerate}/${target.maxBitrate / 1e6}M`, iteration,
      { ok: flowing.ok, ms: updated.ms + flowing.ms, detail: flowing.detail });
  }
  for (let iteration = 1; iteration <= repeat; iteration++) {
    await call(harness, "matrix.pause(true)");
    await delay(1000);
    await call(harness, "matrix.pause(false)");
    await record("pause-resume", iteration, await verifyFlow(harness));
  }
  for (let iteration = 1; iteration <= repeat; iteration++) {
    const started = Date.now();
    await call(harness, "matrix.reconnectFirstEdge()");
    const flowing = await verifyFlow(harness);
    await record("viewer-reconnect", iteration, { ...flowing, ms: Date.now() - started });
  }
  const restarts = Math.max(1, Math.floor(repeat / 3));
  for (let iteration = 1; iteration <= restarts; iteration++) {
    const started = Date.now();
    await harness.stopApp();
    await harness.startApp();
    const outcome = await ensure();
    await record("app-restart", iteration, { ...outcome, ms: Date.now() - started });
  }
  await stopShare(harness);
  const failed = results.filter((value) => !value.ok);
  await writeFile(join(harness.runDir, "reliability.json"), JSON.stringify({ results, failed: failed.length }, null, 2));
  process.stdout.write(`reliability: ${results.length - failed.length}/${results.length} steps passed\n`);
}

// Opus tiers measured at the receiver, then the lossless channel decoded by
// Chrome's own WebCodecs FLAC decoder while a test signal plays.
async function runAudio(harness: Harness): Promise<void> {
  const state = await evaluate<string>(harness.cdp, harness.pattern, "startTone()", Date.now() + 10_000);
  process.stdout.write(`test signal: ${state}\n`);
  const base = parseProfile(option("profile", "1080p@30@8"));
  try {
    for (const quality of list("audio-qualities", "music,very-high,high,max")) {
      const started = await call<{ ok: boolean; response: unknown }>(harness,
        `matrix.start(${JSON.stringify({ codec: list("codecs", "h264")[0], kind: "display", title: PATTERN_TITLE,
          profile: { ...base, screenAudioQuality: quality }, edges: 1, audio: true })})`, 40_000);
      if (!started.ok) {
        process.stdout.write(`${quality}: start failed ${JSON.stringify(started.response)}\n`);
        continue;
      }
      await delay(3_000);
      const stats = await call<{ kbps: number; mimeType: string | null; fmtp: string | null }>(harness,
        "matrix.audioStats(6)", 20_000);
      process.stdout.write(`${quality}: received ${stats.kbps.toFixed(0)} kbps ${stats.mimeType} [${stats.fmtp}]\n`);
      if (quality === list("audio-qualities", "music,very-high,high,max").at(-1)) {
        const lossless = await call<Record<string, unknown>>(harness, "matrix.losslessProbe(6)", 30_000);
        process.stdout.write(`lossless: ${JSON.stringify(lossless)}\n`);
      }
      await stopShare(harness);
    }
  } finally {
    await evaluate(harness.cdp, harness.pattern, "globalThis.stopTone?.()", Date.now() + 5_000).catch(() => undefined);
  }
}

async function runEndurance(harness: Harness): Promise<void> {
  const codec = list("codecs", "h264")[0]!;
  const target = parseProfile(option("profile", "source@120@51"));
  const duration = Number(option("duration", "3600"));
  const started = await startShare(harness, codec, target);
  if (!started.ok) throw new Error("endurance start failed: " + started.detail);
  const begin = Date.now();
  let minute: Sample[] = [];
  let failure: string | null = null;
  while (Date.now() - begin < duration * 1000) {
    const tick = Date.now();
    const value = await sample(harness, (minute.length % 5) === 4);
    minute.push(value);
    failure = failureEvent(value);
    if (failure) break;
    if (minute.length >= 60) {
      const cell = summarize(codec, target, 0, true, minute, null);
      const system = value.system as Record<string, { workingSetMB?: number; handles?: number }>;
      const line = {
        minute: Math.round((Date.now() - begin) / 60000), ...cell,
        appWorkingSetMB: system["piik-app"]?.workingSetMB, appHandles: system["piik-app"]?.handles,
        captureWorkingSetMB: system["piik-capture-bin"]?.workingSetMB, captureHandles: system["piik-capture-bin"]?.handles,
      };
      await appendFile(join(harness.runDir, "endurance.jsonl"), JSON.stringify(line) + "\n");
      process.stdout.write(`minute ${line.minute}: fps=${fixed(cell.decodedFps)} p5=${fixed(cell.decodedFpsP5)} ` +
        `psnr=${fixed(cell.psnr, 2)} frz=${cell.freezes} drop=${cell.dropped} appMB=${line.appWorkingSetMB} ` +
        `appHandles=${line.appHandles} capMB=${line.captureWorkingSetMB} capHandles=${line.captureHandles}\n`);
      minute = [];
    }
    await delay(Math.max(0, 1000 - (Date.now() - tick)));
  }
  await stopShare(harness);
  process.stdout.write(`endurance ${failure ? "FAILED: " + failure : "completed"} after ${Math.round((Date.now() - begin) / 1000)} s\n`);
}

async function main(): Promise<void> {
  const chromePath = process.env.CHROME_PATH?.trim() || "C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe";
  const appExecutable = resolve(option("app", join(ROOT, "build", "go-check", "piik-app.exe")));
  const captureExecutable = resolve(option("capture", join(ROOT, "runtime", "native", "piik-capture.exe")));
  const stamp = new Date().toISOString().replace(/[:.]/g, "-");
  const runDir = join(ROOT, "build", "media-matrix", `${scenario}-${stamp}`);
  await mkdir(join(runDir, "logs"), { recursive: true });
  const profileDir = await mkdtemp(join(tmpdir(), "piik-client-media-"));
  const pagePort = await reservePort();
  const debugPort = await reservePort();
  const server = await startPageServer(pagePort);
  const chrome = await launchChrome(chromePath, debugPort, profileDir, [
    "--no-first-run", "--no-default-browser-check", "--disable-extensions",
    "--disable-background-timer-throttling", "--disable-backgrounding-occluded-windows",
    "--disable-renderer-backgrounding", "--autoplay-policy=no-user-gesture-required",
    ...(options.has("color-profile") ? [`--force-color-profile=${option("color-profile", "srgb")}`] : []),
    "--disable-features=CalculateNativeWinOcclusion",
    "--auto-select-desktop-capture-source=Entire screen", "--use-fake-ui-for-media-stream",
  ]);
  chrome.stdout.resume();
  chrome.stderr.resume();
  let app: ChildProcessWithoutNullStreams | null = null;
  let endpoint: AppEndpoint | null = null;
  const system = new SystemSampler();
  let cdp: CdpConnection | null = null;
  try {
    const version = await waitForVersion(debugPort, chrome);
    cdp = await CdpConnection.connect(version.webSocketDebuggerUrl, Date.now() + 10_000);
    const control = await createPage(cdp, `http://127.0.0.1:${pagePort}/control`, undefined, true);
    const pattern = await createPage(cdp, `http://127.0.0.1:${pagePort}/pattern${options.has("noise") ? "?noise" : ""}`, undefined, true);
    const { targetInfos } = await cdp.call<{ targetInfos: Array<{ targetId: string; url: string }> }>(
      "Target.getTargets", {}, undefined, Date.now() + 5_000);
    const patternTarget = targetInfos.find((target) => target.url.includes("/pattern"));
    if (option("source", "display") === "display" && patternTarget) {
      const { windowId } = await cdp.call<{ windowId: number }>("Browser.getWindowForTarget",
        { targetId: patternTarget.targetId }, undefined, Date.now() + 5_000);
      await cdp.call("Browser.setWindowBounds", { windowId, bounds: { windowState: "fullscreen" } }, undefined, Date.now() + 5_000);
    }
    await delay(1500);
    const patternState = await evaluate<{ width: number; height: number }>(cdp, pattern, "patternState()", Date.now() + 5_000);
    process.stdout.write(`pattern ${patternState.width}x${patternState.height}; run ${runDir}\n`);

    const harness: Harness = {
      cdp, control, pattern, system, runDir,
      log: new AppLog(join(runDir, "logs", "client.log")),
      async startApp() {
        app = spawn(appExecutable, [
          "--site", `http://127.0.0.1:${pagePort}`,
          "--config", join(profileDir, "client.json"),
          "--capture-process", captureExecutable,
          "--debug", "--log-dir", join(runDir, "logs"),
        ], { stdio: "pipe", windowsHide: true, env: { ...process.env, PIIK_CLIENT_GATE_NO_BROWSER: "true" } });
        app.stderr.resume();
        endpoint = await readAppEndpoint(app, { timeoutMs: 20_000, ignoreNonEndpointLines: true });
        app.stdout.resume();
        await evaluate(cdp!, control, `matrix.connect(${JSON.stringify(endpoint)}, ${JSON.stringify(NATIVE_CLIENT_SUBPROTOCOL)})`, Date.now() + 15_000);
        await evaluate(cdp!, control, `matrix.pattern = ${JSON.stringify(patternState)}`, Date.now() + 5_000);
      },
      async stopApp() {
        if (!app) return;
        const child: ChildProcessWithoutNullStreams = app;
        app = null;
        await new Promise<void>((resolveExit) => {
          child.once("exit", () => resolveExit());
          spawn("taskkill.exe", ["/PID", String(child.pid), "/T", "/F"], { windowsHide: true });
          setTimeout(resolveExit, 5_000);
        });
        await delay(1000);
      },
    };
    system.start();
    await harness.startApp();
    await writeFile(join(runDir, "run.json"), JSON.stringify({
      scenario, options: Object.fromEntries(options), browser: version.Browser,
      cpu: cpus()[0]?.model, cores: cpus().length, pattern: patternState,
    }, null, 2));
    if (scenario === "matrix") await runMatrix(harness);
    else if (scenario === "reliability") await runReliability(harness);
    else if (scenario === "endurance") await runEndurance(harness);
    else if (scenario === "browser") await runBrowser(harness);
    else if (scenario === "audio") await runAudio(harness);
    else if (scenario === "colorcheck") {
      process.stdout.write(JSON.stringify(await call(harness, `matrix.colorCheck("display")`)) + "\n");
      const gdi = spawn("powershell.exe", ["-NoProfile", "-Command",
        "Add-Type -AssemblyName System.Drawing,System.Windows.Forms; $b=[System.Windows.Forms.Screen]::PrimaryScreen.Bounds; " +
        "$s=New-Object System.Drawing.Bitmap $b.Width,$b.Height; $g=[System.Drawing.Graphics]::FromImage($s); $g.CopyFromScreen(0,0,0,0,$b.Size); " +
        "$k=$b.Width/2560; foreach($p in @(@(16,16),@(300,300),@(600,400))){ $c=$s.GetPixel([int]($p[0]*$k),[int]($p[1]*$k)); '{0},{1}: {2},{3},{4}' -f $p[0],$p[1],$c.R,$c.G,$c.B }"],
        { stdio: "pipe", windowsHide: true });
      let gdiText = "";
      gdi.stdout.on("data", (chunk: Buffer) => { gdiText += chunk.toString(); });
      await new Promise((resolveExit) => gdi.once("exit", resolveExit));
      process.stdout.write("gdi " + gdiText.replace(/\r?\n/g, " | ") + "\n");
      const started = await startShare(harness, list("codecs", "h264")[0]!, parseProfile(option("profile", "source@30@20")));
      if (!started.ok) throw new Error(started.detail);
      await measure(harness, 4);
      await saveSnapshot(harness, "colorcheck");
      process.stdout.write(JSON.stringify(await call(harness, `(() => { const v = [...matrix.edges.values()][0].video; const c = document.createElement("canvas"); c.width = v.videoWidth; c.height = v.videoHeight; const x = c.getContext("2d"); x.drawImage(v, 0, 0); return { nativeStream: { white: Array.from(x.getImageData(16, 16, 1, 1).data.slice(0, 3)), tileA: Array.from(x.getImageData(300, 300, 1, 1).data.slice(0, 3)) } }; })()`)) + "\n");
      await stopShare(harness);
    }
    else throw new Error("unknown scenario " + scenario);
    if (harness.log.failures.length) {
      await writeFile(join(runDir, "failures.log"), harness.log.failures.join("\n"));
    }
  } finally {
    system.stop();
    const appChild = app as ChildProcessWithoutNullStreams | null;
    await cleanupRun({
      cdp, native: appChild, chrome, server, profile: profileDir,
      ports: [pagePort, debugPort, ...(endpoint ? [(endpoint as AppEndpoint).port] : [])],
    });
  }
}

await main();
