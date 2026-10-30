# Rúnar provenance

Built from `https://github.com/icellan/runar` branch `main` at commit
`d207ee8ec4c2b16e5d3b181da1e177fe1fd26c50` (2026-09-18, packages `1.0.0-rc.1`), which was still the head of
`main` on 2026-10-29.

Rebuild: clone, check out that commit, `pnpm install --frozen-lockfile && pnpm build`, then `pnpm pack` in
`packages/runar-{lang,compiler,ir-schema,sdk,testing}` and copy the tarballs here (`contracts/vendor/runar/`). Never install `runar-*`
from npm (0.4.6 has a forgeable stateless `checkPreimage`).

The root `package.json` lists the tarballs as dependencies and pins every `runar-*` name to them with `$name`
overrides, so every workspace resolves the vendored build. Use npm: pnpm overrides do not honour tarballs.
After rebuilding, update the filenames there and run `npm install`.
