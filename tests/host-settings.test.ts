import { describe, expect, it } from "vitest";
import {
  DEFAULT_HOST_SETTINGS,
  MAX_HOST_PRESETS,
  completeHostSettings,
  deletePreset,
  parseHostSettingsFile,
  presetName,
  savePreset,
} from "../src/client/lib/host-settings";

const gaming = {
  quality: { resolution: "source", maxFramerate: 120, maxBitrate: 40_000_000,
    degradationPreference: "maintain-framerate", screenAudioQuality: "max" },
  codec: "h265", topologyOptimization: true, natPrediction: false, captureBorder: true,
} as const;

describe("remembered Host settings", () => {
  it("round-trips the last settings and presets", () => {
    const file = parseHostSettingsFile(JSON.parse(JSON.stringify({
      version: 1, last: gaming, presets: [{ name: "Gaming", settings: gaming }],
    })));
    expect(file.last).toEqual(gaming);
    expect(file.presets).toEqual([{ name: "Gaming", settings: gaming }]);
  });

  it("keeps valid fields and drops invalid or outdated ones", () => {
    const file = parseHostSettingsFile({
      last: { quality: { ...gaming.quality, maxFramerate: 240 }, codec: "vp9", captureBorder: true, extra: 1 },
      presets: [{ name: "  ", settings: {} }, { name: "A", settings: gaming }, { name: "A", settings: {} }, "x"],
    });
    // 240 fps exceeds the current ceiling, so the whole quality block falls back.
    expect(file.last).toEqual({ captureBorder: true });
    expect(file.presets.map((preset) => preset.name)).toEqual(["A"]);
    expect(completeHostSettings(file.last ?? {})).toEqual({ ...DEFAULT_HOST_SETTINGS, captureBorder: true });
  });

  it("never throws on damaged input", () => {
    for (const value of [null, "text", 3, [], { last: "x", presets: {} }]) {
      expect(parseHostSettingsFile(value)).toEqual({ version: 1, last: null, presets: [] });
    }
  });

  it("saves, replaces, bounds and deletes presets by name", () => {
    let file = parseHostSettingsFile(null);
    file = savePreset(file, "A", completeHostSettings(gaming));
    file = savePreset(file, "A", DEFAULT_HOST_SETTINGS);
    expect(file.presets).toEqual([{ name: "A", settings: DEFAULT_HOST_SETTINGS }]);
    for (let index = 0; index < MAX_HOST_PRESETS + 3; index++) {
      file = savePreset(file, `P${index}`, DEFAULT_HOST_SETTINGS);
    }
    expect(file.presets).toHaveLength(MAX_HOST_PRESETS);
    expect(deletePreset(file, `P${MAX_HOST_PRESETS + 2}`).presets).toHaveLength(MAX_HOST_PRESETS - 1);
    expect(presetName("  Movie   night ")).toBe("Movie night");
    expect(presetName("x".repeat(41))).toBeNull();
  });
});

describe("default encoder", () => {
  it("is H.265 when the App has a hardware HEVC encoder, automatic otherwise", async () => {
    const { defaultHostSettings } = await import("../src/client/lib/host-settings");
    expect(defaultHostSettings(true).codec).toBe("h265");
    expect(defaultHostSettings(false)).toEqual(DEFAULT_HOST_SETTINGS);
    expect(DEFAULT_HOST_SETTINGS.codec).toBe("auto");
    // A remembered codec wins; a record without one takes this Host's default.
    expect(completeHostSettings({ codec: "h264" }, defaultHostSettings(true)).codec).toBe("h264");
    expect(completeHostSettings({ captureBorder: true }, defaultHostSettings(true)).codec).toBe("h265");
  });
});

describe("H.265 on a page that cannot receive it", () => {
  it("falls back to automatic selection instead of a stream it cannot preview", async () => {
    const { usableHostSettings } = await import("../src/client/lib/host-settings");
    const settings = completeHostSettings({ codec: "h265" });
    expect(usableHostSettings(settings, true).codec).toBe("h265");
    expect(usableHostSettings(settings, false).codec).toBe("auto");
    expect(usableHostSettings({ ...settings, codec: "h264" }, false).codec).toBe("h264");
  });
});
