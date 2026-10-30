// node contracts/compile.mjs <Name>...
// Reads contracts/src/<Name>.runar.ts, writes contracts/artifacts/<Name>.json.
// BigInts are serialised as "<n>n" strings; contracts/test/helpers.ts reverses that.
import { compile } from 'runar-compiler';
import { readFileSync, writeFileSync, mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = dirname(fileURLToPath(import.meta.url));
const names = process.argv.slice(2);
if (names.length === 0) {
  console.error('usage: node contracts/compile.mjs <Name>...');
  process.exit(2);
}
mkdirSync(join(root, 'artifacts'), { recursive: true });

let failed = false;
for (const name of names) {
  const src = readFileSync(join(root, 'src', `${name}.runar.ts`), 'utf8');
  const t = Date.now();
  const r = compile(src, { fileName: `${name}.runar.ts` });
  const errs = (r.diagnostics || []).filter((d) => d.severity === 'error');
  if (!r.artifact || errs.length) {
    console.error(name, 'FAILED', JSON.stringify(errs.map((e) => e.message), null, 1));
    failed = true;
    continue;
  }
  writeFileSync(
    join(root, 'artifacts', `${name}.json`),
    JSON.stringify(r.artifact, (_k, v) => (typeof v === 'bigint' ? v.toString() + 'n' : v)),
  );
  console.log(
    name, 'script bytes', r.artifact.script.length / 2, 'compile ms', Date.now() - t,
    'methods', r.artifact.abi.methods.map((m) => m.name + (m.sigHashType ? '@0x' + m.sigHashType.toString(16) : '')).join(','),
  );
  for (const d of r.diagnostics || []) if (d.severity === 'warning') console.log('  warn:', d.message);
}
process.exit(failed ? 1 : 0);
