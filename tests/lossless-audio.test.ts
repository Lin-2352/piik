import { describe, expect, it } from "vitest";
import { FLAC_DESCRIPTION } from "../src/client/media/lossless-audio";

describe("lossless audio", () => {
  it("probes FLAC support with the App's exact stream header", () => {
    // internal/app/nativeaudio TestFLACStreamHeaderIsTheSharedDescription pins the same bytes.
    expect(Buffer.from(FLAC_DESCRIPTION).toString("hex")).toBe(
      "664c61438000002203c003c00000000000000bb802f00000000000000000000000000000000000000000",
    );
  });
});
