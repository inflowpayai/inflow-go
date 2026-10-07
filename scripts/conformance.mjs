import { execFileSync } from "node:child_process";
import { mkdtemp, open, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { parseArgs } from "node:util";
import { runtimeCases } from "../conformance/runtime-cases.mjs";

const root = fileURLToPath(new URL("..", import.meta.url));
const command = (program, args, cwd = root) =>
  execFileSync(program, args, {
    cwd,
    encoding: "utf8",
    timeout: 180000,
    maxBuffer: 8 * 1024 * 1024,
  }).trim();

export function checkContract(contractRoot, revision) {
  if (!/^[0-9a-f]{40}$/.test(revision))
    throw new Error("Contract revision must be a full commit SHA");
  if (
    command("git", ["rev-parse", "HEAD"], contractRoot) !== revision ||
    command("git", ["status", "--porcelain"], contractRoot)
  )
    throw new Error(`Use a clean contract checkout at ${revision}`);
}

export function metadata(binary) {
  const info = JSON.parse(command("go", ["version", "-m", "-json", binary]));
  const dependencies = {};
  for (const line of command("go", [
    "list",
    "-m",
    "-f",
    "{{.Path}} {{.Version}}{{if .Replace}} => {{.Replace.Path}} {{.Replace.Version}}{{end}}",
    "all",
  ]).split("\n")) {
    const [name, ...version] = line.split(" ");
    if (name !== info.Main.Path) dependencies[name] = version.join(" ");
  }
  return {
    name: "inflow-go",
    runtime: info.GoVersion,
    packages: { [info.Main.Path]: info.Main.Version },
    dependencies,
  };
}

async function main() {
  const { values } = parseArgs({
    options: {
      "contract-root": { type: "string" },
      "contract-revision": { type: "string" },
      "output-dir": { type: "string" },
    },
  });
  if (!values["contract-root"] || !values["output-dir"])
    throw new Error(
      "Usage: node scripts/conformance.mjs --contract-root PATH --output-dir EXISTING_DIRECTORY",
    );
  const contractRoot = resolve(values["contract-root"]);
  const pin = JSON.parse(
    await readFile(
      new URL("../conformance/inflow-specs.lock.json", import.meta.url),
      "utf8",
    ),
  );
  checkContract(contractRoot, values["contract-revision"] ?? pin.revision);
  const directory = await mkdtemp(join(tmpdir(), "inflow-go-conformance-"));
  const binary = join(directory, "adapter.test");
  const controller = new AbortController();
  const abort = () => controller.abort();
  process.once("SIGINT", abort);
  process.once("SIGTERM", abort);
  try {
    command("go", ["test", "-c", "-race", "-o", binary, "./conformance"]);
    const implementation = metadata(binary);
    const { run } = await import(
      pathToFileURL(join(contractRoot, "runner/run.mjs"))
    );
    for (const suite of ["runtime", "mpp", "x402", "tap", "payment-status", "stripe", "card"]) {
      if (controller.signal.aborted) throw new Error("Conformance interrupted");
      const fixtures = await import(
        pathToFileURL(join(contractRoot, `fixtures/${suite}.mjs`))
      );
      const index =
        suite === "runtime"
          ? runtimeCases(fixtures.runtimeScenarios)
          : suite === "payment-status"
            ? fixtures.paymentStatusCases
            : fixtures[`${suite}Cases`];
      const output = await open(
        join(resolve(values["output-dir"]), `${suite}.json`),
        "wx",
        0o600,
      );
      try {
        const report = await run({
          index,
          capabilities: {
            suites:
              suite === "runtime"
                ? ["runtime"]
                : suite === "payment-status"
                  ? ["mpp-buyer", "x402-buyer"]
                  : suite === "stripe"
                    ? ["mpp-seller"]
                    : suite === "card"
                      ? ["mpp-buyer", "mpp-seller"]
                      : suite === "tap"
                        ? ["tap-seller"]
                        : [`${suite}-core`, `${suite}-buyer`, `${suite}-seller`],
            supported_features:
              suite === "mpp" ? ["mpp-seller-subscriptions"] : [],
            unsupported_features: [],
          },
          implementation,
          command: [binary, "--adapter"],
          contractRoot,
          sdkRoot: root,
          signal: controller.signal,
        });
        await output.writeFile(`${JSON.stringify(report, null, 2)}\n`);
        console.log(
          `${suite}: ${report.results.filter((result) => result.status === "passed").length}/${report.results.length} passed`,
        );
        if (!report.passed) {
          process.exitCode = 1;
          console.error(
            report.runner_error ??
              report.results.filter((result) => result.status !== "passed"),
          );
        }
      } finally {
        await output.close();
      }
    }
  } finally {
    process.removeListener("SIGINT", abort);
    process.removeListener("SIGTERM", abort);
    await rm(directory, { recursive: true, force: true });
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) await main();
