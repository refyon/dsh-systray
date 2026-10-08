# Contributing to dsh-systray

Thanks for taking the time to help. This is a small, single-maintainer project, so the fastest
path to a merged change is a focused diff plus evidence that you ran the checks below.

## Before you start

- **Bugs and feature ideas** → open an issue (templates are provided). For anything larger than a
  small fix, please open an issue first so we can agree on the approach before you write code.
- **Security problems** → do **not** open a public issue; follow [SECURITY.md](SECURITY.md).
- **Translations / wording** → always welcome. The UI is bilingual (`src/i18n.go` plus `data-i18n`
  keys in the frontend), and the checker below will tell you which key you missed.

## Development setup

Prerequisites: **Go 1.21+**, the **Wails CLI v2**
(`go install github.com/wailsapp/wails/v2/cmd/wails@v2.15.0`), and Node.js for the frontend checks.

```bash
cd src
wails dev          # hot-reloading development build
```

Useful environment variables while developing: `DSH_SYSTRAY_PORT`, `DSH_SYSTRAY_HARNESS_DIR`,
`DSH_SYSTRAY_STARTUP_TIMEOUT`, `DSH_SYSTRAY_LOG_DIR`, `DSH_SYSTRAY_LANG`, `DSH_SYSTRAY_PROXY`.

## Checks to run before opening a pull request

```bash
cd src && go test ./...                    # Go regressions: file sync, account, updater, service watchdog
node scripts/check-frontend-i18n.mjs       # translation keys: data-i18n ↔ I18N_EN, literals ↔ I18N_DYN
node scripts/check-frontend-files-card.mjs # file list: size formatting, tree, sorting, navigation
node scripts/check-frontend-lang.mjs       # real browser: 7 pages × zh/en + live language switch
```

For any UI or wording change run at least the last three — the third one renders the same UI twice
(started in the target language vs switched into it) and lists every element whose text did not
follow. It needs a local Edge or Chrome. Please also add a test for behaviour you fix or add: the
Go tests are table-driven and live beside the code they cover (`src/*_test.go`).

`gofmt`-clean and `go vet`-clean are required. Behaviour that only exists in a packaged build
(install location, start-at-login, cancelling system dialogs) still has to be verified on a real
machine — say so in the pull request if you could not do that part.

## Coding conventions

- **Language**: code comments, log messages and commit subjects are written in Chinese in this
  repository; user-facing strings go through the i18n layer, never inline.
- **Design**: UI work must follow [docs/DESIGN.md](docs/DESIGN.md) — colors, spacing and radii come from the
  CSS tokens there, and any new dialog has to come to the foreground (see the Do's & Don'ts).
- **Scope**: keep a pull request to one concern. Unrelated refactors make a change hard to review
  and hard to roll back.
- **Dependencies**: avoid adding a runtime dependency. Anything that ships inside the binary has to
  be justified, because it also has to survive on Windows 10 and unsigned macOS builds.

## Commits

Short imperative subjects, one line, optionally with a `scope:` prefix:

```
fix(plugin): resolve mirror tarball specs to GitHub repos
site: rebuild hero banner
```

Describe what changed and why in the body when the reason is not obvious from the diff. Do not
include machine-specific absolute paths, credentials or personal data in commits — the repository
is public.

## Releases

Maintainer-only: push a `vX.Y.Z` tag and CI builds both platforms, creates the release and picks the
notes up from `docs/RELEASE_NOTES.md`. A plain push to `main` does not build anything; to verify that
`main` still builds, run the `build-release` workflow manually (`workflow_dispatch`).

## License

By contributing you agree that your contribution is licensed under the terms of
[LICENSE](LICENSE).
