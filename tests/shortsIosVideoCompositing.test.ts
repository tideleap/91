import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const shortsCss = readFileSync(
  new URL("../src/styles/shorts.css", import.meta.url),
  "utf8"
);

// iOS Safari/WebKit does not composite an inline <video> nested inside a
// `position: fixed` ancestor — the video decodes and plays but never paints
// (black screen on iOS only). The shorts page wrapper must therefore not be
// position:fixed; it locks the viewport via html/body overflow + 100svh height.
test("shorts page wrapper is not position:fixed (breaks iOS <video> compositing)", () => {
  const pageRule = /\.shorts-page \{[\s\S]*?\}/.exec(shortsCss);
  assert.ok(pageRule, ".shorts-page rule should exist");
  assert.doesNotMatch(pageRule[0], /position:\s*fixed/);
  assert.match(pageRule[0], /position:\s*relative/);
  assert.match(pageRule[0], /height:\s*100svh/);
});
