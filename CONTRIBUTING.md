# Contributing

Thanks for helping! The most common contribution is adding an app.

## Adding an app ([catalog/catalog.json](catalog/catalog.json))
```json
{"key":"vivaldi","name":"Vivaldi","category":"Browsers","winget":"Vivaldi.Vivaldi","brew":"vivaldi"}
```
Rules (enforced by `go test ./catalog` and reviewers):
- Package ids come from the **official** winget / Homebrew / Debian-Ubuntu repositories only. No URLs, no third-party sources, no installer downloads.
- Verify the id yourself: `winget show --id <id> -e`, `brew info <name>`, `apt show <name>`.
- Key: lowercase, unique. Name: the vendor's product name.
- Detection: the app is matched by package id **and** by the display name Windows lists it under. If the name differs from `name` (e.g. "Mozilla Firefox"), add `"match":["mozilla firefox"]`. Check with `winget list` on a machine where it's installed.
- Icons: add the app's Simple Icons slug to `tools/fetch_icons.py` and run it; apps without a brand icon get a letter tile automatically.
- Starter packs live in `catalog/packs.json`; keep them small and unsurprising.

## Code
- `go vet ./... && go test ./... && python tests/test_e2e.py && python tests/test_go_agent.py` must pass.
- Matching rules exist twice (Go: `catalog/catalog.go`, Python: `server/server.py`); change both and their tests.
- Keep dependencies at zero (Go standard library, Python standard library). Fewer moving parts is a feature.
- The UI is one file, `web/index.html`, with no build step and no network dependencies. UI icons (Lucide, ISC) are embedded by `tools/fetch_ui_icons.py`; app icons (Simple Icons, CC0) by `tools/fetch_icons.py`. Text must go through the `h()` helper (never `innerHTML`) so user-provided names can't inject markup.
- **Demo backend.** `OPENITE_DEMO=1` swaps the package manager for a simulated one: installs are short child processes that print realistic, slowly arriving output (including progress bars) and change nothing on your machine. `OPENITE_DEMO_FAIL=chrome` makes packages whose id contains that text fail with an MSI-style error. Use it to work on live progress, streaming and failure hints: `OPENITE_DEMO=1 openite ui` or `OPENITE_DEMO=1 openite run`.
- Failure hints live in `cmd/openite/explain.go` (one table of patterns, with tests). Add a rule there, not in the UI.
- Anything that executes commands must go through `run()`/backends with argv lists, never a shell string.
