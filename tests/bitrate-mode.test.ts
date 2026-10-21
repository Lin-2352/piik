import { describe, expect, it } from "vitest";
import { DEFAULT_QUALITY_SETTINGS, qualitySettingsSchema } from "../src/shared/protocol";
import { resolveBitrateMode, videoQualitySettingsEqual } from "../src/client/media/quality";
import { parseHostSettingsFile } from "../src/client/lib/host-settings";

describe("bitrate mode", () => {
  it("is optional and defaults to variable, so earlier settings stay valid", () => {
    expect(qualitySettingsSchema.safeParse(DEFAULT_QUALITY_SETTINGS).success).toBe(true);
    expect(resolveBitrateMode(undefined)).toBe("vbr");
    expect(qualitySettingsSchema.safeParse({ ...DEFAULT_QUALITY_SETTINGS, bitrateMode: "cbr" }).success).toBe(true);
    expect(qualitySettingsSchema.safeParse({ ...DEFAULT_QUALITY_SETTINGS, bitrateMode: "abr" }).success).toBe(false);
  });

  it("counts as a video change, but an absent field equals variable", () => {
    expect(videoQualitySettingsEqual(DEFAULT_QUALITY_SETTINGS, { ...DEFAULT_QUALITY_SETTINGS, bitrateMode: "vbr" })).toBe(true);
    expect(videoQualitySettingsEqual(DEFAULT_QUALITY_SETTINGS, { ...DEFAULT_QUALITY_SETTINGS, bitrateMode: "cbr" })).toBe(false);
  });

  it("keeps a saved preset that predates the field and one that has it", () => {
    const file = parseHostSettingsFile({
      version: 1,
      last: { quality: { ...DEFAULT_QUALITY_SETTINGS, bitrateMode: "cbr" } },
      presets: [{ name: "Old", settings: { quality: DEFAULT_QUALITY_SETTINGS } }],
    });
    expect(file.last?.quality?.bitrateMode).toBe("cbr");
    expect(file.presets[0]?.settings.quality).toEqual(DEFAULT_QUALITY_SETTINGS);
  });
});
