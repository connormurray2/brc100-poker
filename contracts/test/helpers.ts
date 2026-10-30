import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { RunarArtifact } from 'runar-ir-schema';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');

/** Absolute path of the contracts workspace, for tests that shell out to compile.mjs. */
export const contractsRoot = root;

/** Reads contracts/artifacts/<name>.json, reversing compile.mjs's "<n>n" BigInt encoding. */
export function loadArtifact(name: string): RunarArtifact {
  return JSON.parse(readFileSync(join(root, 'artifacts', `${name}.json`), 'utf8'), (_k, v) =>
    typeof v === 'string' && /^-?\d+n$/.test(v) ? BigInt(v.slice(0, -1)) : v,
  ) as RunarArtifact;
}
