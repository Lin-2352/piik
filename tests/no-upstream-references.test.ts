import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { describe, expect, it } from "vitest";

// This fork does not present the original project's name, website or
// repositories to users or point its tooling at them. LICENSE keeps the
// copyright notice MIT requires, and the Go module path is code, not a surface.
// site/ is the original project's separate marketing website, not part of the app.
const ROOT = join(import.meta.dirname, "..");
const SURFACES = [
  "src",
  "public",
  "index.html",
  "README.md",
  "README.zh-CN.md",
  "CONTRIBUTING.md",
  "cmd",
  "deploy",
  "scripts",
  "internal/app/launcher",
];
const SKIPPED = new Set(["node_modules", "dist", ".vitepress/cache", ".vitepress/dist"]);
const FORBIDDEN = /tntcraft|piik\.tv|gitee\.com\/tntcraft/i;
// The module path appears in every Go import and linker flag.
const MODULE_PATH = /github\.com\/TNTcraftHIM\/Piik\/(internal|cmd)\b/g;

function* files(path: string): Generator<string> {
  const stat = statSync(path);
  if (stat.isFile()) {
    yield path;
    return;
  }
  for (const entry of readdirSync(path)) {
    if (SKIPPED.has(entry)) continue;
    yield* files(join(path, entry));
  }
}

describe("original project references", () => {
  it("are absent from shipped surfaces", () => {
    const found: string[] = [];
    for (const surface of SURFACES) {
      const path = join(ROOT, surface);
      try {
        statSync(path);
      } catch {
        continue;
      }
      for (const file of files(path)) {
        if (/\.(png|jpe?g|webp|ico|woff2?|zip|exe|wasm|mp4|webm)$/i.test(file)) continue;
        if (relative(ROOT, file).endsWith("no-upstream-references.test.ts")) continue;
        const lines = readFileSync(file, "utf8").split(/\r?\n/);
        lines.forEach((line, index) => {
          if (FORBIDDEN.test(line.replace(MODULE_PATH, ""))) {
            found.push(`${relative(ROOT, file)}:${index + 1}: ${line.trim().slice(0, 100)}`);
          }
        });
      }
    }
    expect(found).toEqual([]);
  });
});
