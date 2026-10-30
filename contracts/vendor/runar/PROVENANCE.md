# Rúnar provenance

Built from `https://github.com/icellan/runar`, packages `1.0.0-rc.1`, as the merge of two unmerged fix branches
on top of `main` at commit `d207ee8ec4c2b16e5d3b181da1e177fe1fd26c50` (2026-09-18), which was still the head of
`origin/main` on 2026-10-30 (checked with `git fetch origin`; neither fix was on `main`):

| Piece | Commit | What it fixes |
|---|---|---|
| base: `main` | `d207ee8ec4c2b16e5d3b181da1e177fe1fd26c50` | |
| `fix/if-arm-dead-local-desync` ([icellan/runar#179](https://github.com/icellan/runar/pull/179)) | `9fb51ea5124d13c3c5e2637e7c7bb0ded030cc95` | #178: a branch arm reordered the slots it inherits, so a later read took its neighbour (a silent miscompile that made the gate (e) evaluator reject kicker hands) |
| `fix/temp-name-collision` ([icellan/runar#177](https://github.com/icellan/runar/pull/177)) | `b96bc8fc9770091acb5a9c5237cfd1ec2386e5b5` | #175: `t<digits>` identifiers aliased ANF temporaries; they are now a compile error |
| **merge** (local, never pushed) | `cb34fd262b8dfe17e022f89cc0dd6bf7c46fe487` | `git merge --no-ff fork/fix/temp-name-collision` onto the #179 branch. The only conflict was `CHANGELOG.md`; both entries were kept |

Both branches are from the fork `connormurray2/runar`. When #177 and #179 land on `main`, re-vendor from `main`
and replace this table.

Of the five tarballs, only `runar-compiler` and `runar-testing` differ from the previous vendoring
(`d207ee8e` alone). `runar-lang`, `runar-sdk` and `runar-ir-schema` are byte-identical: both fixes are in
the compiler.

## Rebuild

1. `git clone https://github.com/icellan/runar && cd runar && git remote add fork https://github.com/connormurray2/runar && git fetch fork`
2. `git worktree add ../runar-gates fork/fix/if-arm-dead-local-desync -b gates/combined && cd ../runar-gates`
3. `git merge --no-ff fork/fix/temp-name-collision`, resolve `CHANGELOG.md` by keeping both entries, commit.
4. `pnpm install --frozen-lockfile && pnpm build`
5. `pnpm pack --pack-destination <dir>` in each of `packages/runar-{lang,compiler,ir-schema,sdk,testing}`, then copy
   the tarballs into `contracts/vendor/runar/`.
6. Refresh the lock (below) and run `npx vitest run --root contracts`.

Never install `runar-*` from npm (0.4.6 has a forgeable stateless `checkPreimage`, G-25).

## Refreshing the lock

The root `package.json` lists the tarballs as dependencies and pins every `runar-*` name to them with `$name`
overrides, so every workspace resolves the vendored build. Use npm: pnpm overrides do not honour tarballs.

**A plain `npm install` does not pick up a tarball replaced under the same version.** It keeps the old
`integrity` hash and the old `node_modules/runar-*`, and says nothing. Delete the five `integrity` lines of
`node_modules/runar-*` from `package-lock.json` and `node_modules/runar-*`, run `npm install`, and then set each
`integrity` to `sha512-` plus the base64 SHA-512 of its tarball (npm does not write one for a `file:` tarball
on its own). `contracts/test/toolchain.test.ts` checks both the `file:contracts/vendor/runar/` source and that
each hash matches the tarball on disk.

## Tarball SHA-256

```
e5a16fe3f774c38a3bbbc7e97c1954dd8362184da0969b9a175d4140b400fba2  runar-compiler-1.0.0-rc.1.tgz
2af7413cc7deef4a4f424a33c702d8bd04033e7fa796e44be8fa2e67416d11d8  runar-ir-schema-1.0.0-rc.1.tgz
50d061007b3a537353117ef45c0480793921326cb7f8ac5845842796064970a0  runar-lang-1.0.0-rc.1.tgz
63e0281cfdbd255a9f16261cf7bb1ffb5b09c7f1b0e81b209212ca8f5e868dab  runar-sdk-1.0.0-rc.1.tgz
35edcdc3f202a8d7392a15d049f65a45eff9e1d46b3a5f88e0fd24e2342d47b2  runar-testing-1.0.0-rc.1.tgz
```
