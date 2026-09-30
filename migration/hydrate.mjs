import { execFileSync } from "node:child_process";
import { existsSync } from "node:fs";
import { mkdir, readFile, cp } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

export const material = path.dirname(fileURLToPath(import.meta.url));
export const project = path.dirname(material);
export const executor = path.resolve(project, "../portsmith");
export const pins = JSON.parse(await readFile(path.join(material, "pins.json"), "utf8"));
export const source = path.join(project, ".cache/portsmith", pins.portsmith.revision);

export async function hydrate() {
  if (!existsSync(source)) {
    // Extract a known Git tree, never credentials, untracked files or node_modules.
    const archive = execFileSync("git", ["-C", executor, "archive", pins.portsmith.revision], {maxBuffer: 64 * 1024 * 1024});
    await mkdir(source, {recursive: true});
    execFileSync("tar", ["-xf", "-", "-C", source], {input: archive});
  }
  for (const name of pins.referenceFiles) {
    const target = path.join(source, "dependency-reference", name);
    if (!existsSync(target)) {
      await mkdir(path.dirname(target), {recursive: true});
      await cp(path.join(material, name), target);
    }
  }
  const api = path.join(source, "dependency-reference/API.go.txt");
  if (!existsSync(api)) await cp(path.join(material, "API.go.txt"), api);
  return source;
}

export function toolEnvironment() {
  // The executor freezes GOTOOLCHAIN=local for tests. Put the reviewed toolchain
  // on PATH so it does not accidentally pick macOS's older global Go install.
  const goroot = execFileSync("go", ["env", "GOROOT"], {
    cwd: project, encoding: "utf8", env: {...process.env, GOTOOLCHAIN: "go1.25.0"},
  }).trim();
  return {...process.env, PATH: path.join(goroot, "bin") + path.delimiter + process.env.PATH,
    GOMAXPROCS: process.env.GOMAXPROCS ?? "4"};
}
