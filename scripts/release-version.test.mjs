import assert from "node:assert/strict";
import { test } from "node:test";
import { releaseTag } from "./release-version.mjs";

test("accepts stable versions compatible with the module path", () => {
  for (const version of ["0.1.0", "0.10.12", "1.0.0", "1.23.456"]) {
    assert.equal(
      releaseTag(version, "refs/heads/main", "false"),
      `v${version}`,
    );
  }
});

test("rejects malformed versions, prereleases and incompatible module majors", () => {
  for (const version of [
    undefined,
    "",
    "v0.1.0",
    "01.0.0",
    "0.01.0",
    "0.1.00",
    "2.0.0",
    "0.1.0-rc.1",
    "0.1.0+build",
    "0.1.0\n",
    "$(id)",
  ]) {
    assert.throws(() => releaseTag(version, "refs/heads/main", "false"));
  }
});

test("branch and pull-request runs cannot publish", () => {
  for (const ref of [
    undefined,
    "refs/heads/release",
    "refs/tags/v0.1.0",
    "refs/pull/1/merge",
  ]) {
    assert.throws(() => releaseTag("0.1.0", ref, "false"));
    assert.equal(releaseTag("0.1.0", ref, "true"), "v0.1.0");
  }
});

test("does not interpret missing or unrecognized dry-run values as permission to publish", () => {
  for (const value of [undefined, "", "False", "0", "yes"]) {
    assert.throws(() => releaseTag("0.1.0", "refs/heads/main", value));
  }
});
