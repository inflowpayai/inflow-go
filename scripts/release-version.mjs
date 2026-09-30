import { pathToFileURL } from "node:url";

export function releaseTag(version, ref, dryRun) {
  if (!/^(0|1)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/.test(version)) {
    throw new Error(
      "Use a stable version without v, with major 0 or 1. Major 2 requires a /v2 module path.",
    );
  }
  if (dryRun !== "true" && dryRun !== "false") {
    throw new Error("dry_run must be true or false");
  }
  if (dryRun === "false" && ref !== "refs/heads/main") {
    throw new Error("Publishing must run from main");
  }
  return `v${version}`;
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(process.argv[1]).href
) {
  console.log(
    releaseTag(
      process.env.RELEASE_VERSION,
      process.env.GITHUB_REF,
      process.env.DRY_RUN,
    ),
  );
}
