// Preparation helper only. The delivered Go binary does not use this script.
import { spawn, execFileSync } from "node:child_process";
import path from "node:path";
import { hydrate, project, executor, material, toolEnvironment } from "./hydrate.mjs";

await hydrate();
const args = process.argv.slice(2);
if (args.some(a => a === "--plan" || a.startsWith("--plan="))) throw Error("This launcher always uses the reviewed migration/ plan");
if (!args.includes("--check") && !args.includes("--commit")) throw Error("Use --check for preparation checks or --commit to execute the complete migration");
const environment = toolEnvironment();
// Candidates are verified offline; prefetch only the operator-reviewed graph.
execFileSync("go", ["mod", "download"], {cwd: project, env: environment, stdio: "inherit"});
const child = spawn(process.execPath, [path.join(executor, "dist/cli.js"), "migrate", "--plan", material, ...args], {
  cwd: project, stdio: "inherit", env: {...environment,
    PORTSMITH_MODEL: process.env.PORTSMITH_MODEL ?? "deepseek-flash",
    PORTSMITH_BASE_URL: process.env.PORTSMITH_BASE_URL ?? "https://api.deepseek.com/v1"},
});
// The executor handles cancellation and preserves the current candidate/session.
for (const signal of ["SIGINT", "SIGTERM"]) process.on(signal, () => child.kill(signal));
child.on("error", error => { console.error(error.message); process.exitCode = 1; });
child.on("exit", (code, signal) => { process.exitCode = code ?? (signal ? 130 : 1); });
