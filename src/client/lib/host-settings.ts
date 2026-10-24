import { z } from "zod";
import {
  DEFAULT_QUALITY_SETTINGS,
  qualitySettingsSchema,
  type QualitySettings,
} from "../../shared/protocol";
import type { BrowserVideoCodecMode } from "../webrtc/video-codec";

// The Host's remembered settings: the last combination used and named presets.
// The App stores this object in a file beside client.json; a Browser-only Host
// keeps it in localStorage. Fields are validated one by one, so a damaged or
// older file keeps whatever is still valid and never blocks starting.

export interface HostSettings {
  quality: QualitySettings;
  codec: BrowserVideoCodecMode;
  topologyOptimization: boolean;
  natPrediction: boolean;
  captureBorder: boolean;
}

export interface HostSettingsPreset {
  name: string;
  settings: Partial<HostSettings>;
}

export interface HostSettingsFile {
  version: 1;
  last: Partial<HostSettings> | null;
  presets: HostSettingsPreset[];
}

export const MAX_HOST_PRESETS = 20;
export const MAX_HOST_PRESET_NAME = 40;
const STORAGE_KEY = "piik.hostSettings";

export const DEFAULT_HOST_SETTINGS: HostSettings = {
  quality: DEFAULT_QUALITY_SETTINGS,
  codec: "auto",
  topologyOptimization: true,
  natPrediction: false,
  captureBorder: false,
};

const codecSchema = z.enum(["vp8", "auto", "h264", "h265", "av1"]);

export function parseHostSettings(value: unknown): Partial<HostSettings> {
  if (!value || typeof value !== "object" || Array.isArray(value)) return {};
  const raw = value as Record<string, unknown>;
  const settings: Partial<HostSettings> = {};
  const quality = qualitySettingsSchema.safeParse(raw.quality);
  if (quality.success) settings.quality = quality.data;
  const codec = codecSchema.safeParse(raw.codec);
  if (codec.success) settings.codec = codec.data;
  for (const key of ["topologyOptimization", "natPrediction", "captureBorder"] as const) {
    if (typeof raw[key] === "boolean") settings[key] = raw[key];
  }
  return settings;
}

export function presetName(value: string): string | null {
  const name = value.trim().replace(/\s+/g, " ");
  return name.length >= 1 && name.length <= MAX_HOST_PRESET_NAME ? name : null;
}

export function parseHostSettingsFile(value: unknown): HostSettingsFile {
  const file: HostSettingsFile = { version: 1, last: null, presets: [] };
  if (!value || typeof value !== "object" || Array.isArray(value)) return file;
  const raw = value as Record<string, unknown>;
  const last = parseHostSettings(raw.last);
  file.last = Object.keys(last).length > 0 ? last : null;
  if (Array.isArray(raw.presets)) {
    for (const entry of raw.presets) {
      if (file.presets.length === MAX_HOST_PRESETS) break;
      const preset = entry as Record<string, unknown> | null;
      const name = typeof preset?.name === "string" ? presetName(preset.name) : null;
      if (!name || file.presets.some((existing) => existing.name === name)) continue;
      file.presets.push({ name, settings: parseHostSettings(preset?.settings) });
    }
  }
  return file;
}

/**
 * Defaults for this Host. An App with a hardware HEVC encoder starts in H.265;
 * anything else keeps automatic selection so a share never asks for an encoder
 * the machine does not have.
 */
export function defaultHostSettings(hardwareHEVC: boolean): HostSettings {
  return hardwareHEVC ? { ...DEFAULT_HOST_SETTINGS, codec: "h265" } : DEFAULT_HOST_SETTINGS;
}

/** A remembered H.265 choice this page cannot decode falls back to automatic selection. */
export function usableHostSettings(settings: HostSettings, canReceiveH265: boolean): HostSettings {
  return settings.codec === "h265" && !canReceiveH265 ? { ...settings, codec: "auto" } : settings;
}

/** Fills every missing field from the defaults. */
export function completeHostSettings(
  settings: Partial<HostSettings>,
  defaults: HostSettings = DEFAULT_HOST_SETTINGS,
): HostSettings {
  return { ...defaults, ...settings };
}

export function savePreset(
  file: HostSettingsFile,
  name: string,
  settings: HostSettings,
): HostSettingsFile {
  const presets = file.presets.filter((preset) => preset.name !== name);
  if (presets.length >= MAX_HOST_PRESETS) presets.shift();
  return { ...file, presets: [...presets, { name, settings }] };
}

export function deletePreset(file: HostSettingsFile, name: string): HostSettingsFile {
  return { ...file, presets: file.presets.filter((preset) => preset.name !== name) };
}

export function loadLocalHostSettings(): HostSettingsFile {
  try {
    const stored = window.localStorage.getItem(STORAGE_KEY);
    return parseHostSettingsFile(stored ? JSON.parse(stored) : null);
  } catch {
    return parseHostSettingsFile(null);
  }
}

export function saveLocalHostSettings(file: HostSettingsFile): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(file));
  } catch {
    // Private windows may refuse storage; the App file remains the record.
  }
}
