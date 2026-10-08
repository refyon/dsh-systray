<!-- Keep this short: what changed, why, and how you verified it. -->

## What changed

<!-- One or two sentences. If the diff is large, say which files carry the real change. -->

## Why

<!-- The problem being solved. Link the issue: Fixes #123 -->

## Verification

<!-- Which checks did you run? Paste the commands, not the whole output. -->

- [ ] `cd src && go test ./...`
- [ ] `node scripts/check-frontend-i18n.mjs`
- [ ] `node scripts/check-frontend-files-card.mjs`
- [ ] `node scripts/check-frontend-lang.mjs` (needed for any UI or wording change)
- [ ] Manually verified on a packaged build, if the change only shows up there

Platforms you tested on:

## Checklist

- [ ] One concern per pull request (no unrelated refactors)
- [ ] UI changes follow [DESIGN.md](../blob/main/docs/DESIGN.md) (tokens, spacing, focus states, dialogs come to the front)
- [ ] New user-facing strings go through the i18n layer (both `zh` and `en`), not inline
- [ ] Behaviour that is fixed or added is covered by a test
- [ ] No machine-specific absolute paths, credentials or personal data in the diff
- [ ] `CONTRIBUTING.md` checks understood; anything not verified is called out above
