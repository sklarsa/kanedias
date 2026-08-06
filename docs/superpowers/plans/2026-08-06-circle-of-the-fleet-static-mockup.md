# Circle of the Fleet Static Mockup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the placeholder Kanedias server page with a fully embedded, static Circle of the Fleet desktop mockup built on vendored Terminal.css.

**Architecture:** Keep the current `html/template`, Chi, Datastar, and `go:embed web/*` boundaries. Add one embedded Terminal.css asset route, then replace only the project-authored HTML/CSS with a fixed fleet orrery, nested-subagent aperture, paused question, transcript, metrics, and disabled operator controls; retain every existing server endpoint and add no live behavior.

**Tech Stack:** Go 1.26.5, Chi v5.3.1, `html/template`, `embed`, Datastar browser bundle v1.0.2, datastar-go v1.2.2, Terminal.css at commit `63551f0de711f2f634a0c2da7bab1d3bae216fef`, plain HTML5, and project-owned CSS.

## Global Constraints

- The approved design is `docs/superpowers/specs/2026-08-06-circle-of-the-fleet-static-mockup-design.md` at or after commit `63fcefa`; stop rather than widening its product scope.
- Execution begins in an isolated worktree created through the `superpowers:using-git-worktrees` skill. Do not create a manual worktree before invoking that skill.
- Use red/green TDD for handler, asset, HTML-contract, and static-behavior changes.
- Vendor Terminal.css unchanged from commit `63551f0de711f2f634a0c2da7bab1d3bae216fef`; its exact SHA-256 is `54382cfc04c064df22f6179453bb3eb85c50fd9cf855f7b57adfbe8c8f75b0f8`.
- Keep all browser resources as flat files under `internal/server/web` and embedded by the existing `//go:embed web/*` directive.
- Keep `GET /`, `/healthz`, `/ui/status`, `/assets/app.css`, and `/assets/datastar.js` unchanged; add only `GET /assets/terminal.css`.
- Terminal.css loads before `app.css`; the existing local Datastar module loads after both stylesheets.
- The new page contains no Datastar action/request attributes, automatic requests, inline fetch, form, editable prompt, API binding, shell, RPC, watcher, database, or browser application logic.
- Every preview control is a native disabled button. Status uses a symbol and readable text, never color alone.
- The theme is dark, cobalt/amber, system-monospace, and static: no flashing, pulsing, rotating, glitch, or continuous animation.
- Mobile reflow, mobile navigation, and touch-specific behavior are out of scope; preserve desktop reachability through horizontal overflow without mobile acceptance work.
- Do not add external fonts, images, icons, stylesheets, scripts, CDNs, analytics, telemetry, npm, lockfiles, Sass, Tailwind, CSS generation, or a frontend build step.
- Do not modify CLI behavior, listen-address validation, server lifecycle, Incus behavior, Go dependencies, the existing Datastar bundle, or the architecture note.
- Finish with an independent review, complete verification, a fast-forward merge to local `main`, and a non-force push to `origin/main` as the user requested.

---

## File Map

- Modify `internal/server/handler.go` only to construct and register the Terminal.css handler.
- Modify `internal/server/handler_test.go` for the sixth route, provenance integrity, static mockup contract, asset order, and project-style contract.
- Replace `internal/server/web/index.html` with semantic fixed mockup markup.
- Replace `internal/server/web/app.css` with the Kanedias visual layer; this is the only customized stylesheet.
- Create `internal/server/web/terminal.css` as unchanged upstream bytes.
- Create `internal/server/web/terminal.LICENSE` as unchanged upstream MIT text.
- Create `internal/server/web/terminal.PROVENANCE` with immutable source and digest data.
- Do not modify `internal/server/server.go`, `cmd/**`, `main.go`, `go.mod`, `go.sum`, `datastar.js`, `datastar.LICENSE`, or `datastar.PROVENANCE`.

### Task 1: Vendor and Serve Terminal.css

**Files:**
- Create: `internal/server/web/terminal.css`
- Create: `internal/server/web/terminal.LICENSE`
- Create: `internal/server/web/terminal.PROVENANCE`
- Modify: `internal/server/handler.go:51-60`
- Modify: `internal/server/handler_test.go:3-15,18-123,188-208`

**Interfaces:**
- Consumes: existing `serveEmbeddedAsset(logger *slog.Logger, name, contentType string) http.HandlerFunc` and `//go:embed web/*`.
- Produces: `GET /assets/terminal.css` with `Content-Type: text/css; charset=utf-8`; unchanged vendored bytes and verifiable provenance for Task 2’s HTML asset reference.

- [ ] **Step 1: Establish the isolated execution baseline**

After invoking `superpowers:using-git-worktrees`, record the worktree and branch, then run:

```bash
git branch --show-current
git rev-parse --show-toplevel
git status --short --untracked-files=all
go mod download
go test ./... -count=1
```

Expected: the worktree is isolated from the user’s `main` checkout, status is empty, module download exits `0`, and the baseline test suite passes. If the baseline fails, stop before source edits and report the exact failure.

- [ ] **Step 2: Add failing route and provenance tests**

In `internal/server/handler_test.go`, add `crypto/sha256` and `fmt` imports. Extend `TestHandlerRoutes` with this case between the existing app stylesheet and JavaScript cases:

```go
{
    name:        "terminal stylesheet",
    path:        "/assets/terminal.css",
    status:      http.StatusOK,
    contentType: "text/css; charset=utf-8",
},
```

Replace the asset-body condition with:

```go
if strings.HasPrefix(tt.path, "/assets/") && response.Body.Len() == 0 {
    t.Fatal("asset body is empty")
}
```

Add the framework route to both route lists:

```go
paths := []string{
    "/",
    "/healthz",
    "/ui/status",
    "/assets/terminal.css",
    "/assets/app.css",
    "/assets/datastar.js",
}
```

Add this test below `TestAssetsAreEmbedded`:

```go
func TestTerminalCSSProvenanceMatchesEmbeddedAsset(t *testing.T) {
    stylesheet, err := webFiles.ReadFile("web/terminal.css")
    if err != nil {
        t.Fatalf("read embedded Terminal.css: %v", err)
    }
    license, err := webFiles.ReadFile("web/terminal.LICENSE")
    if err != nil {
        t.Fatalf("read embedded Terminal.css license: %v", err)
    }
    provenance, err := webFiles.ReadFile("web/terminal.PROVENANCE")
    if err != nil {
        t.Fatalf("read Terminal.css provenance: %v", err)
    }

    digest := sha256.Sum256(stylesheet)
    checks := []string{
        "Commit: 63551f0de711f2f634a0c2da7bab1d3bae216fef",
        fmt.Sprintf("SHA-256: %x", digest),
        "License identifier: MIT",
        "Modification: Vendored unchanged for offline embedding.",
    }
    for _, want := range checks {
        if !strings.Contains(string(provenance), want) {
            t.Errorf("Terminal.css provenance does not contain %q", want)
        }
    }
    if !strings.Contains(string(license), "MIT License") || !strings.Contains(string(license), "Copyright (c) 2019 Jonas D.") {
        t.Fatal("embedded Terminal.css license is not the expected upstream MIT license")
    }
}
```

- [ ] **Step 3: Run the focused tests RED**

Run:

```bash
gofmt -w internal/server/handler_test.go
go test ./internal/server -run '^(TestHandlerRoutes|TestHandlerRejectsUnsupportedMethods|TestAssetsAreEmbedded|TestTerminalCSSProvenanceMatchesEmbeddedAsset)$' -count=1
```

Expected: FAIL because `/assets/terminal.css` is not registered and `web/terminal.css`, `web/terminal.LICENSE`, and `web/terminal.PROVENANCE` do not exist.

- [ ] **Step 4: Download and verify immutable upstream bytes**

Run exactly:

```bash
curl --fail --location --proto '=https' --tlsv1.2 \
  https://raw.githubusercontent.com/Gioni06/terminal.css/63551f0de711f2f634a0c2da7bab1d3bae216fef/lib/terminal.css \
  -o internal/server/web/terminal.css
curl --fail --location --proto '=https' --tlsv1.2 \
  https://raw.githubusercontent.com/Gioni06/terminal.css/63551f0de711f2f634a0c2da7bab1d3bae216fef/LICENSE \
  -o internal/server/web/terminal.LICENSE
printf '%s  %s\n' \
  54382cfc04c064df22f6179453bb3eb85c50fd9cf855f7b57adfbe8c8f75b0f8 \
  internal/server/web/terminal.css | sha256sum -c -
```

Expected: `internal/server/web/terminal.css: OK`. Do not format, minify, concatenate, or edit the stylesheet or license.

- [ ] **Step 5: Record Terminal.css provenance**

Create `internal/server/web/terminal.PROVENANCE` with this exact structure, substituting only the command-produced UTC date:

```text
Project: Terminal.css
Official repository: https://github.com/Gioni06/terminal.css
Commit: 63551f0de711f2f634a0c2da7bab1d3bae216fef
Source: https://raw.githubusercontent.com/Gioni06/terminal.css/63551f0de711f2f634a0c2da7bab1d3bae216fef/lib/terminal.css
License source: https://raw.githubusercontent.com/Gioni06/terminal.css/63551f0de711f2f634a0c2da7bab1d3bae216fef/LICENSE
Vendored path: internal/server/web/terminal.css
Retrieval date: 2026-08-06
License identifier: MIT
SHA-256: 54382cfc04c064df22f6179453bb3eb85c50fd9cf855f7b57adfbe8c8f75b0f8
Modification: Vendored unchanged for offline embedding.
```

Use these commands so the date is concrete rather than handwritten:

```bash
RETRIEVAL_DATE=$(date -u +%F)
python - "$RETRIEVAL_DATE" <<'PY'
from pathlib import Path
import sys

date = sys.argv[1]
Path("internal/server/web/terminal.PROVENANCE").write_text(f"""Project: Terminal.css
Official repository: https://github.com/Gioni06/terminal.css
Commit: 63551f0de711f2f634a0c2da7bab1d3bae216fef
Source: https://raw.githubusercontent.com/Gioni06/terminal.css/63551f0de711f2f634a0c2da7bab1d3bae216fef/lib/terminal.css
License source: https://raw.githubusercontent.com/Gioni06/terminal.css/63551f0de711f2f634a0c2da7bab1d3bae216fef/LICENSE
Vendored path: internal/server/web/terminal.css
Retrieval date: {date}
License identifier: MIT
SHA-256: 54382cfc04c064df22f6179453bb3eb85c50fd9cf855f7b57adfbe8c8f75b0f8
Modification: Vendored unchanged for offline embedding.
""")
PY
```

- [ ] **Step 6: Register the embedded framework route**

In `internal/server/handler.go`, replace the asset construction/registration block with:

```go
serveTerminalCSS := serveEmbeddedAsset(logger, "web/terminal.css", "text/css; charset=utf-8")
serveCSS := serveEmbeddedAsset(logger, "web/app.css", "text/css; charset=utf-8")
serveJavaScript := serveEmbeddedAsset(logger, "web/datastar.js", "text/javascript; charset=utf-8")

router := chi.NewRouter()
router.Use(requestLogger(logger), recoverPanics(logger))
router.Get("/", serveIndex)
router.Get("/healthz", serveHealth)
router.Get("/ui/status", serveStatus)
router.Get("/assets/terminal.css", serveTerminalCSS)
router.Get("/assets/app.css", serveCSS)
router.Get("/assets/datastar.js", serveJavaScript)
```

Do not change any existing handler or middleware behavior.

- [ ] **Step 7: Run Task 1 GREEN**

Run:

```bash
gofmt -w internal/server/handler.go internal/server/handler_test.go
go test ./internal/server -run '^(TestHandlerRoutes|TestHandlerRejectsUnsupportedMethods|TestAssetsAreEmbedded|TestTerminalCSSProvenanceMatchesEmbeddedAsset)$' -count=1
go test ./internal/server -count=1
git diff --check
```

Expected: all commands pass.

- [ ] **Step 8: Commit the embedded framework**

Run:

```bash
git add internal/server/handler.go internal/server/handler_test.go \
  internal/server/web/terminal.css \
  internal/server/web/terminal.LICENSE \
  internal/server/web/terminal.PROVENANCE
git diff --cached --check
git commit -m "feat: embed Terminal.css"
git status --short
```

Expected: the commit succeeds and worktree status is empty.

### Task 2: Replace the Placeholder with the Static Circle of the Fleet

**Files:**
- Modify: `internal/server/handler_test.go:18-226`
- Replace: `internal/server/web/index.html`
- Replace: `internal/server/web/app.css`

**Interfaces:**
- Consumes: `/assets/terminal.css` from Task 1, existing `/assets/app.css`, and existing `/assets/datastar.js`.
- Produces: stable static region IDs `fleet-orbit`, `maker-aperture`, `question-alert`, and `command-deck`; seven disabled preview buttons; no runtime request or interaction.

- [ ] **Step 1: Replace obsolete page tests with failing mockup-contract tests**

Delete `TestInitialPageContainsInertPanels` and `TestIndexRequiresClickForStatusRefresh`. Add these tests in their place:

```go
func TestInitialPageContainsCircleOfFleetMockup(t *testing.T) {
    body := indexBody(t)
    required := []string{
        `<html lang="en" data-theme="dark">`,
        `<body class="terminal">`,
        `KANEDIAS // CIRCLE OF THE FLEET`,
        `STATIC DEMONSTRATION`,
        `id="question-alert"`,
        `2 QUESTIONS`,
        `id="fleet-orbit"`,
        `RPC-SPIKE`,
        `WEB-SHELL`,
        `PTY-OWNER`,
        `id="maker-aperture"`,
        `Should shell sessions survive a browser reconnect`,
        `12.8K`,
        `TOKENS`,
        `id="command-deck"`,
        `● ACTIVE`,
        `◇ QUESTION`,
        `○ COMPLETE`,
    }
    for _, want := range required {
        if !strings.Contains(body, want) {
            t.Errorf("initial page does not contain %q", want)
        }
    }

    obsolete := []string{
        "Refresh status",
        "Not refreshed yet.",
        "Dashboard view is not available in this scaffold.",
        "Session view is not available in this scaffold.",
        `id="dashboard-panel"`,
        `id="session-panel"`,
    }
    for _, unwanted := range obsolete {
        if strings.Contains(body, unwanted) {
            t.Errorf("initial page retains obsolete content %q", unwanted)
        }
    }
}

func TestCircleOfFleetMockupIsStatic(t *testing.T) {
    body := indexBody(t)
    lower := strings.ToLower(body)
    forbidden := []string{
        "data-on:",
        "data-init",
        "@get(",
        "/ui/status",
        "fetch(",
        "xmlhttprequest",
        "onclick=",
        "contenteditable",
        "<form",
        "<input",
        "<textarea",
        "<select",
        "<a ",
    }
    for _, unwanted := range forbidden {
        if strings.Contains(lower, unwanted) {
            t.Errorf("static mockup contains active mechanism %q", unwanted)
        }
    }

    buttonRE := regexp.MustCompile(`(?s)<button\b[^>]*>.*?</button>`)
    buttons := buttonRE.FindAllString(body, -1)
    if len(buttons) != 7 {
        t.Fatalf("button count = %d, want 7", len(buttons))
    }
    for _, button := range buttons {
        if !strings.Contains(button, " disabled") {
            t.Errorf("mockup button is not disabled: %s", button)
        }
    }
}
```

Replace `TestRenderedPageHasNoExternalRuntimeAssets` with:

```go
func TestRenderedPageHasOnlyOrderedLocalRuntimeAssets(t *testing.T) {
    body := indexBody(t)
    assetRE := regexp.MustCompile(`(?:src|href)="([^"]+)"`)
    matches := assetRE.FindAllStringSubmatch(body, -1)
    want := []string{
        "/assets/terminal.css",
        "/assets/app.css",
        "/assets/datastar.js",
    }
    if len(matches) != len(want) {
        t.Fatalf("runtime asset count = %d, want %d", len(matches), len(want))
    }
    for index, match := range matches {
        if match[1] != want[index] {
            t.Errorf("runtime asset %d = %q, want %q", index, match[1], want[index])
        }
        asset := strings.ToLower(match[1])
        if strings.HasPrefix(asset, "http://") ||
            strings.HasPrefix(asset, "https://") ||
            strings.HasPrefix(asset, "//") ||
            strings.Contains(asset, "cdn") ||
            strings.Contains(asset, "node_modules") ||
            strings.Contains(asset, "npm") ||
            strings.Contains(asset, "unpkg") {
            t.Errorf("external runtime asset %q", match[1])
        }
    }
}

func TestProjectStylesDefineStaticCircleVisualSystem(t *testing.T) {
    contents, err := webFiles.ReadFile("web/app.css")
    if err != nil {
        t.Fatalf("read embedded project stylesheet: %v", err)
    }
    styles := string(contents)
    required := []string{
        "--page-bg: #05070b",
        "--active: #69a9ed",
        "--question: #d9ae70",
        "min-width: 72rem",
        "#fleet-orbit",
        ".orbit-ring",
        ".run-node",
        ".child-moon",
        "#maker-aperture",
        "#question-alert",
        "#command-deck",
    }
    for _, want := range required {
        if !strings.Contains(styles, want) {
            t.Errorf("project stylesheet does not contain %q", want)
        }
    }

    lower := strings.ToLower(styles)
    for _, unwanted := range []string{"@import", "http://", "https://", "url(", "@keyframes", "animation:"} {
        if strings.Contains(lower, unwanted) {
            t.Errorf("project stylesheet contains disallowed runtime or motion construct %q", unwanted)
        }
    }
}
```

Keep `regexp` imported because the static-controls and asset-order tests use it.

- [ ] **Step 2: Run the page contract RED**

Run:

```bash
gofmt -w internal/server/handler_test.go
go test ./internal/server -run '^(TestInitialPageContainsCircleOfFleetMockup|TestCircleOfFleetMockupIsStatic|TestRenderedPageHasOnlyOrderedLocalRuntimeAssets|TestProjectStylesDefineStaticCircleVisualSystem)$' -count=1
```

Expected: FAIL because the current page still contains the old status/dashboard/session scaffold, loads only two assets, and lacks the Circle visual system.

- [ ] **Step 3: Replace `index.html` with the semantic static mockup**

Write `internal/server/web/index.html` with this complete document:

```html
<!doctype html>
<html lang="en" data-theme="dark">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="dark">
  <title>Kanedias — Circle of the Fleet</title>
  <link rel="stylesheet" href="/assets/terminal.css">
  <link rel="stylesheet" href="/assets/app.css">
  <script type="module" src="/assets/datastar.js"></script>
</head>
<body class="terminal">
  <div class="app-shell container-fluid">
    <header class="command-header">
      <div>
        <p class="kicker">Master Maker’s operator console</p>
        <h1>KANEDIAS // CIRCLE OF THE FLEET</h1>
      </div>
      <div class="session-proof" aria-label="Mock Pi session">
        <span class="session-label">PI SESSION</span>
        <strong>019FD8D2</strong>
        <span class="mock-label">STATIC DEMONSTRATION</span>
      </div>
      <div id="question-alert" class="terminal-alert question-count" role="note">
        <span aria-hidden="true">◇</span>
        <strong>2 QUESTIONS</strong>
        <span>await the Maker</span>
      </div>
    </header>

    <main class="fleet-console" aria-label="Static subagent fleet mockup">
      <section id="fleet-orbit" aria-labelledby="fleet-heading">
        <div class="section-heading">
          <div>
            <p class="kicker">Inner ring: parent runs · Moons: nested subagents</p>
            <h2 id="fleet-heading">Fleet orrery</h2>
          </div>
          <span class="view-label">ALL POSITIONS ARE MOCK DATA</span>
        </div>

        <div class="orrery" aria-label="Four parent runs and four nested subagents">
          <div class="orbit-ring orbit-inner" aria-hidden="true"></div>
          <div class="orbit-ring orbit-middle" aria-hidden="true"></div>
          <div class="orbit-ring orbit-outer" aria-hidden="true"></div>

          <div class="maker-center" aria-label="The Maker at the fleet center">
            <strong>K∴D</strong>
            <span>YOU / MAKER</span>
          </div>

          <article class="run-node run-rpc terminal-card" aria-label="RPC Spike workflow active">
            <header><span>RPC-SPIKE</span><span>◆</span></header>
            <div>
              <p>workflow · 3 children</p>
              <strong>● ACTIVE</strong>
              <span>READ docs/rpc.md · 01:42</span>
            </div>
          </article>

          <article class="run-node run-shell terminal-card question-node" aria-label="Web Shell worker paused with a question">
            <header><span>WEB-SHELL</span><span>◇</span></header>
            <div>
              <p>worker · 2 children</p>
              <strong>◇ QUESTION</strong>
              <span>PAUSED FOR THE MAKER</span>
            </div>
          </article>

          <article class="run-node run-incus terminal-card" aria-label="Incus Image worker active">
            <header><span>INCUS-IMAGE</span><span>◆</span></header>
            <div>
              <p>worker · 1 child</p>
              <strong>● ACTIVE</strong>
              <span>TEST ./internal/image · 00:44</span>
            </div>
          </article>

          <article class="run-node run-review terminal-card" aria-label="Final Review parallel run partially complete">
            <header><span>FINAL-REVIEW</span><span>◆</span></header>
            <div>
              <p>parallel · 3 children</p>
              <strong>○ COMPLETE 1/3</strong>
              <span>8.4K TOKENS OBSERVED</span>
            </div>
          </article>

          <div class="child-moon moon-researcher">
            <span class="moon-mark" aria-hidden="true">○</span>
            <span><strong>RESEARCHER</strong><small>COMPLETE</small></span>
          </div>
          <div class="child-moon moon-pty question-moon">
            <span class="moon-mark" aria-hidden="true">◇</span>
            <span><strong>PTY-OWNER</strong><small>QUESTION</small></span>
          </div>
          <div class="child-moon moon-tests">
            <span class="moon-mark" aria-hidden="true">●</span>
            <span><strong>TEST-RUNNER</strong><small>ACTIVE</small></span>
          </div>
          <div class="child-moon moon-correctness">
            <span class="moon-mark" aria-hidden="true">●</span>
            <span><strong>CORRECTNESS</strong><small>ACTIVE</small></span>
          </div>
        </div>

        <ul class="fleet-legend" aria-label="Fleet state legend">
          <li><span aria-hidden="true">●</span> ACTIVE</li>
          <li><span aria-hidden="true">◇</span> QUESTION</li>
          <li><span aria-hidden="true">○</span> COMPLETE</li>
          <li>Shape and text repeat every color-coded state.</li>
        </ul>
      </section>

      <aside id="maker-aperture" aria-labelledby="aperture-heading">
        <header class="aperture-header">
          <div>
            <p class="kicker">Selected nested child</p>
            <h2 id="aperture-heading">MAKER’S APERTURE</h2>
          </div>
          <span>NESTED DEPTH 1</span>
        </header>

        <p class="selection-path">WEB-SHELL <span>›</span> PTY-OWNER</p>

        <ul class="aperture-tabs" aria-label="Static detail views">
          <li aria-current="page">QUESTION</li>
          <li>TRANSCRIPT</li>
          <li>TOOLS</li>
          <li>ARTIFACTS</li>
        </ul>

        <section class="terminal-alert full-question" aria-labelledby="question-heading">
          <p class="kicker">The child has left its orbit</p>
          <h3 id="question-heading">Should shell sessions survive a browser reconnect, or can v1 terminate them when the tab closes?</h3>
          <div class="answer-preview" aria-label="Disabled answer previews">
            <button class="btn btn-ghost" type="button" disabled>PRESERVE + REPLAY</button>
            <button class="btn btn-ghost" type="button" disabled>TAB-BOUND V1</button>
            <button class="btn btn-ghost" type="button" disabled>WRITE ANSWER…</button>
          </div>
        </section>

        <section class="run-tree-panel" aria-labelledby="tree-heading">
          <h3 id="tree-heading">Run lineage</h3>
          <ul class="run-tree">
            <li><span aria-hidden="true">◆</span> WEB-SHELL <small>worker · paused</small></li>
            <li class="tree-child tree-question"><span aria-hidden="true">◇</span> PTY-OWNER <small>asks you now</small></li>
            <li class="tree-child"><span aria-hidden="true">├</span> RPC-SCOUT <small>complete · 4.1K tokens</small></li>
            <li class="tree-grandchild"><span aria-hidden="true">└</span> DOCS-FETCH <small>complete</small></li>
          </ul>
        </section>

        <section class="transcript-panel" aria-labelledby="transcript-heading">
          <div class="panel-title">
            <h3 id="transcript-heading">Recent transmission</h3>
            <span>STATIC TAIL</span>
          </div>
          <div class="transcript-line">
            <time>20:41:11</time>
            <p><strong>ASSISTANT</strong>The server-owned PTY enables replay after reconnect.</p>
          </div>
          <div class="transcript-line tool-line">
            <time>20:41:15</time>
            <p><strong>TOOL / READ</strong>docs/rpc.md · 281 lines</p>
          </div>
          <div class="transcript-line">
            <time>20:41:18</time>
            <p><strong>ASSISTANT</strong>That creates one product decision I should not guess.</p>
          </div>
        </section>

        <dl class="run-metrics" aria-label="Mock run metrics">
          <div><dt>TOKENS</dt><dd>12.8K</dd></div>
          <div><dt>TOOLS</dt><dd>09</dd></div>
          <div><dt>TURNS</dt><dd>12</dd></div>
          <div><dt>ELAPSED</dt><dd>06:14</dd></div>
        </dl>
      </aside>
    </main>

    <footer id="command-deck">
      <div class="command-copy">
        <span class="command-mark" aria-hidden="true">CENTER&gt;</span>
        <span>answer or steer selected child…</span>
        <span class="static-cursor" aria-hidden="true"></span>
      </div>
      <div class="command-controls" aria-label="Disabled future controls">
        <button class="btn btn-primary" type="button" disabled>STEER</button>
        <button class="btn btn-ghost question-control" type="button" disabled>INTERRUPT</button>
        <button class="btn btn-ghost stop-control" type="button" disabled>STOP RUN</button>
        <button class="btn btn-ghost" type="button" disabled>＋ FORGE ORBIT</button>
      </div>
      <span class="deck-state">STATIC MOCKUP</span>
    </footer>
  </div>
</body>
</html>
```

- [ ] **Step 4: Replace `app.css` with the Kanedias visual layer**

Write `internal/server/web/app.css` with this complete stylesheet:

```css
:root {
  color-scheme: dark;
  --global-font-size: 14px;
  --global-line-height: 1.4em;
  --global-space: 10px;
  --font-stack: Menlo, Monaco, "Lucida Console", "Liberation Mono", "DejaVu Sans Mono", Consolas, monospace;
  --mono-font-stack: var(--font-stack);
  --background-color: #05070b;
  --page-width: none;
  --font-color: #d7e3ef;
  --invert-font-color: #05070b;
  --primary-color: #69a9ed;
  --secondary-color: #71849a;
  --error-color: #e98d68;
  --progress-bar-background: #17283b;
  --progress-bar-fill: #69a9ed;
  --code-bg-color: #0b1420;
  --input-style: solid;
  --display-h1-decoration: none;

  --page-bg: #05070b;
  --surface: #080e17;
  --surface-raised: #0b1522;
  --surface-question: #17130d;
  --border: #2d4d6e;
  --border-strong: #3c6087;
  --text: #d7e3ef;
  --muted: #71849a;
  --active: #69a9ed;
  --active-soft: #9ec4e5;
  --question: #d9ae70;
  --stop: #e98d68;
  --shadow: #020407;
}

html {
  min-width: 72rem;
  min-height: 100%;
  background: var(--page-bg);
}

body {
  min-width: 72rem;
  min-height: 100vh;
  margin: 0;
  overflow-x: auto;
  background:
    radial-gradient(circle at 31% 46%, #0c1726 0, var(--page-bg) 35rem),
    var(--page-bg);
  color: var(--text);
}

body::before {
  position: fixed;
  z-index: 20;
  inset: 0;
  background: repeating-linear-gradient(
    0deg,
    transparent 0,
    transparent 3px,
    rgba(158, 196, 229, 0.018) 3px,
    rgba(158, 196, 229, 0.018) 4px
  );
  content: "";
  pointer-events: none;
}

button,
h1,
h2,
h3,
p,
ul,
dl,
dd {
  margin: 0;
}

button:disabled {
  cursor: not-allowed;
  opacity: 0.72;
}

.app-shell {
  width: 100%;
  min-width: 72rem;
  max-width: 106rem;
  margin: 0 auto;
  padding: 1rem;
}

.kicker,
.view-label,
.session-label,
.mock-label,
.deck-state {
  color: var(--muted);
  font-size: 0.68rem;
  letter-spacing: 0.13em;
  text-transform: uppercase;
}

.command-header {
  display: grid;
  grid-template-columns: minmax(28rem, 1fr) auto auto;
  gap: 1rem;
  align-items: center;
  padding: 0.8rem 0.9rem;
  border: 1px solid var(--border);
  background: rgba(6, 11, 18, 0.94);
}

.command-header h1 {
  display: block;
  padding: 0.2rem 0 0;
  color: var(--active);
  font-size: 1.15rem;
  font-weight: 800;
  letter-spacing: 0.16em;
  line-height: 1.2;
}

.session-proof {
  display: grid;
  grid-template-columns: auto auto;
  gap: 0.15rem 0.55rem;
  align-items: baseline;
  color: var(--active-soft);
}

.session-proof strong {
  color: var(--active-soft);
  letter-spacing: 0.08em;
}

.session-proof .mock-label {
  grid-column: 1 / -1;
  text-align: right;
}

#question-alert {
  display: grid;
  grid-template-columns: auto auto;
  gap: 0.1rem 0.45rem;
  min-width: 11.5rem;
  margin: 0;
  padding: 0.55rem 0.7rem;
  border-color: var(--question);
  background: var(--surface-question);
  color: var(--question);
}

#question-alert > span:last-child {
  grid-column: 2;
  font-size: 0.7rem;
}

.fleet-console {
  display: grid;
  grid-template-columns: minmax(43rem, 1.35fr) minmax(29rem, 0.75fr);
  min-height: 42rem;
  border-right: 1px solid var(--border);
  border-left: 1px solid var(--border);
}

#fleet-orbit,
#maker-aperture {
  min-width: 0;
  background: rgba(5, 8, 13, 0.82);
}

#fleet-orbit {
  border-right: 1px solid var(--border);
}

.section-heading,
.aperture-header {
  display: flex;
  justify-content: space-between;
  align-items: end;
  min-height: 4.1rem;
  padding: 0.7rem 0.8rem;
  border-bottom: 1px solid var(--border);
}

.section-heading h2,
.aperture-header h2 {
  padding-top: 0.2rem;
  color: var(--active-soft);
  font-size: 0.92rem;
  letter-spacing: 0.12em;
}

.aperture-header > span {
  color: var(--muted);
  font-size: 0.68rem;
  letter-spacing: 0.1em;
}

.orrery {
  position: relative;
  min-height: 34rem;
  overflow: hidden;
}

.orbit-ring {
  position: absolute;
  top: 49%;
  left: 46%;
  border: 1px solid var(--border);
  border-radius: 50%;
  transform: translate(-50%, -50%) rotate(-12deg);
  pointer-events: none;
}

.orbit-inner {
  width: 15rem;
  height: 10rem;
}

.orbit-middle {
  width: 27rem;
  height: 19rem;
  transform: translate(-50%, -50%) rotate(17deg);
}

.orbit-outer {
  width: 39rem;
  height: 29rem;
  border-style: dashed;
  transform: translate(-50%, -50%) rotate(-7deg);
}

.maker-center {
  position: absolute;
  z-index: 3;
  top: 49%;
  left: 46%;
  display: flex;
  width: 7rem;
  height: 7rem;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  transform: translate(-50%, -50%);
  border: 1px solid var(--active);
  border-radius: 50%;
  background: #07111d;
  box-shadow: inset 0 0 1.5rem rgba(74, 144, 226, 0.13), 0 0 1rem rgba(74, 144, 226, 0.12);
}

.maker-center strong {
  color: var(--active-soft);
  font-size: 1.2rem;
}

.maker-center span {
  padding-top: 0.2rem;
  color: var(--muted);
  font-size: 0.6rem;
}

.run-node {
  position: absolute;
  z-index: 4;
  width: 12.5rem;
  border-color: var(--border-strong);
  background: var(--surface-raised);
  box-shadow: 0.25rem 0.25rem 0 var(--shadow);
}

.run-node > header {
  display: flex;
  justify-content: space-between;
  padding: 0.42rem 0.55rem;
  border-bottom: 1px solid var(--border-strong);
  background: #0c1a29;
  color: var(--text);
  text-align: left;
}

.run-node > div:first-of-type {
  display: grid;
  gap: 0.25rem;
  padding: 0.55rem;
}

.run-node p,
.run-node span {
  color: var(--muted);
  font-size: 0.7rem;
}

.run-node strong {
  color: var(--active);
  font-size: 0.72rem;
}

.question-node {
  border-color: var(--question);
  background: var(--surface-question);
  box-shadow: 0.25rem 0.25rem 0 #24190c;
}

.question-node > header {
  border-color: var(--question);
  background: #1a160e;
  color: var(--question);
}

.question-node strong,
.question-node > div:first-of-type > span {
  color: var(--question);
}

.run-rpc {
  top: 6.1rem;
  left: 2rem;
}

.run-shell {
  top: 4.7rem;
  right: 1.5rem;
}

.run-incus {
  bottom: 4.5rem;
  left: 1rem;
}

.run-review {
  right: 1.8rem;
  bottom: 4rem;
}

.child-moon {
  position: absolute;
  z-index: 5;
  display: flex;
  gap: 0.4rem;
  align-items: center;
  color: var(--active-soft);
  font-size: 0.62rem;
}

.child-moon .moon-mark {
  display: grid;
  width: 1.25rem;
  height: 1.25rem;
  place-items: center;
  border: 1px solid var(--active);
  border-radius: 50%;
  background: var(--page-bg);
}

.child-moon strong,
.child-moon small {
  display: block;
}

.child-moon small {
  color: var(--muted);
  font-size: 0.54rem;
}

.question-moon,
.question-moon small {
  color: var(--question);
}

.question-moon .moon-mark {
  border-color: var(--question);
  border-radius: 0;
  transform: rotate(45deg);
}

.question-moon .moon-mark + span {
  transform: none;
}

.moon-researcher {
  top: 4.2rem;
  left: 11rem;
}

.moon-pty {
  top: 9.7rem;
  right: 0.6rem;
}

.moon-tests {
  bottom: 2.2rem;
  left: 12.5rem;
}

.moon-correctness {
  right: 10.8rem;
  bottom: 1.6rem;
}

.fleet-legend {
  display: flex;
  gap: 1rem;
  align-items: center;
  min-height: 3rem;
  margin: 0;
  padding: 0.55rem 0.8rem;
  border-top: 1px solid var(--border);
  color: var(--muted);
  font-size: 0.64rem;
}

.fleet-legend li {
  display: inline-flex;
  gap: 0.32rem;
  padding: 0;
}

.fleet-legend li::after {
  content: none;
}

.fleet-legend li:nth-child(1) span {
  color: var(--active);
}

.fleet-legend li:nth-child(2) span {
  color: var(--question);
}

.selection-path {
  padding: 0.65rem 0.8rem;
  border-bottom: 1px solid var(--border);
  color: var(--question);
  font-weight: 700;
  letter-spacing: 0.08em;
}

.selection-path span {
  color: var(--muted);
}

.aperture-tabs {
  display: flex;
  margin: 0;
  border-bottom: 1px solid var(--border);
}

.aperture-tabs li {
  padding: 0.45rem 0.62rem;
  border-right: 1px solid var(--border);
  color: var(--muted);
  font-size: 0.63rem;
}

.aperture-tabs li::after {
  content: none;
}

.aperture-tabs [aria-current="page"] {
  background: var(--surface-question);
  color: var(--question);
}

.full-question {
  margin: 0.65rem;
  padding: 0.7rem;
  border-color: var(--question);
  background: var(--surface-question);
  color: var(--question);
}

.full-question h3 {
  margin: 0.35rem 0 0.75rem;
  color: var(--question);
  font-size: 0.82rem;
  line-height: 1.45;
}

.answer-preview,
.command-controls {
  display: flex;
  flex-wrap: wrap;
  gap: 0.4rem;
}

.answer-preview .btn,
.command-controls .btn {
  padding: 0.45rem 0.6rem;
  font-size: 0.62rem;
}

.run-tree-panel,
.transcript-panel {
  margin: 0 0.65rem 0.65rem;
  border: 1px solid var(--border);
  background: rgba(8, 14, 23, 0.9);
}

.run-tree-panel > h3,
.panel-title {
  padding: 0.45rem 0.55rem;
  border-bottom: 1px solid var(--border);
  color: var(--active-soft);
  font-size: 0.7rem;
  letter-spacing: 0.08em;
}

.run-tree {
  margin: 0;
  padding: 0.55rem;
}

.run-tree li {
  display: block;
  margin-bottom: 0.35rem;
  padding: 0;
  color: var(--text);
  font-size: 0.68rem;
}

.run-tree li::after {
  content: none;
}

.run-tree li > span {
  display: inline-block;
  width: 1.1rem;
  color: var(--active);
}

.run-tree small {
  color: var(--muted);
}

.run-tree .tree-child {
  padding-left: 1rem;
}

.run-tree .tree-grandchild {
  padding-left: 2rem;
}

.run-tree .tree-question,
.run-tree .tree-question > span,
.run-tree .tree-question small {
  color: var(--question);
}

.panel-title {
  display: flex;
  justify-content: space-between;
}

.panel-title h3 {
  color: inherit;
  font-size: inherit;
}

.panel-title span {
  color: var(--muted);
  font-size: 0.6rem;
}

.transcript-line {
  display: grid;
  grid-template-columns: 4.1rem 1fr;
  gap: 0.45rem;
  padding: 0.45rem 0.55rem;
  border-bottom: 1px dotted #1f354d;
}

.transcript-line:last-child {
  border-bottom: 0;
}

.transcript-line time {
  color: var(--muted);
  font-size: 0.63rem;
}

.transcript-line p {
  color: var(--text);
  font-size: 0.67rem;
}

.transcript-line strong {
  display: block;
  margin-bottom: 0.14rem;
  color: var(--active);
  font-size: 0.63rem;
}

.tool-line strong {
  color: var(--question);
}

.run-metrics {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 1px;
  margin: 0 0.65rem 0.65rem;
  background: var(--border);
}

.run-metrics > div {
  display: flex;
  flex-direction: column-reverse;
  align-items: center;
  padding: 0.42rem;
  background: var(--surface);
}

.run-metrics dt {
  color: var(--muted);
  font-size: 0.55rem;
  letter-spacing: 0.08em;
}

.run-metrics dd {
  color: var(--active-soft);
  font-size: 0.78rem;
  font-weight: 700;
}

#command-deck {
  display: grid;
  grid-template-columns: minmax(22rem, 1fr) auto auto;
  gap: 0.75rem;
  align-items: center;
  padding: 0.65rem 0.8rem;
  border: 1px solid var(--border);
  background: #060b12;
}

.command-copy {
  display: flex;
  gap: 0.55rem;
  align-items: center;
  color: var(--text);
}

.command-mark {
  color: var(--active);
  font-weight: 700;
}

.static-cursor {
  display: inline-block;
  width: 0.45rem;
  height: 0.9rem;
  background: var(--active);
}

.question-control {
  border-color: var(--question) !important;
  color: var(--question) !important;
}

.stop-control {
  border-color: var(--stop) !important;
  color: var(--stop) !important;
}

.deck-state {
  color: var(--question);
  text-align: right;
}
```

This CSS intentionally has no responsive media query. Do not add one in this increment.

- [ ] **Step 5: Run the focused mockup tests GREEN**

Run:

```bash
gofmt -w internal/server/handler_test.go
go test ./internal/server -run '^(TestInitialPageContainsCircleOfFleetMockup|TestCircleOfFleetMockupIsStatic|TestRenderedPageHasOnlyOrderedLocalRuntimeAssets|TestProjectStylesDefineStaticCircleVisualSystem)$' -count=1
```

Expected: PASS.

- [ ] **Step 6: Run the full server suite and static source scans**

Run:

```bash
go test ./internal/server -count=1
if grep -nE 'https?://|//[^[:space:]]+|@import|url\(' \
  internal/server/web/index.html internal/server/web/app.css; then
  exit 1
fi
if grep -niE 'data-on:|data-init|@get\(|fetch\(|xmlhttprequest|/ui/status|onclick=|contenteditable|<form|<input|<textarea|<select|<a ' \
  internal/server/web/index.html; then
  exit 1
fi
if grep -nE '@keyframes|animation:' internal/server/web/app.css; then
  exit 1
fi
test "$(grep -c '<button ' internal/server/web/index.html)" -eq 7
test "$(grep -c '<button .* disabled>' internal/server/web/index.html)" -eq 7
git diff --check
```

Expected: tests pass; scans find no remote/runtime/motion constructs; all seven buttons are disabled.

- [ ] **Step 7: Commit the static mockup**

Run:

```bash
git add internal/server/handler_test.go \
  internal/server/web/index.html \
  internal/server/web/app.css
git diff --cached --check
git commit -m "feat: add Circle of the Fleet mockup"
git status --short
```

Expected: commit succeeds and worktree status is empty.

### Task 3: Verify, Review, Integrate, and Push

**Files:**
- Review: all files changed since the approved design commit.
- Modify only if an accepted review finding requires a focused fix.

**Interfaces:**
- Consumes: Task 1 framework route and Task 2 static page.
- Produces: verified commits fast-forwarded into local `main` and pushed to `origin/main` without force.

- [ ] **Step 1: Run complete automated verification**

Run:

```bash
gofmt -w internal/server/handler.go internal/server/handler_test.go
git diff --exit-code
go mod download
go test ./internal/server -count=1
go test ./... -count=1
go test -race ./internal/server ./cmd -count=1
go vet ./...
mkdir -p .tmp
go build -trimpath -o .tmp/kanedias .
git diff --check
```

Expected: formatting produces no diff, every check exits `0`, and `.tmp/kanedias` exists.

- [ ] **Step 2: Reverify immutable assets and exact file scope**

Run:

```bash
printf '%s  %s\n' \
  54382cfc04c064df22f6179453bb3eb85c50fd9cf855f7b57adfbe8c8f75b0f8 \
  internal/server/web/terminal.css | sha256sum -c -
RECORDED_SHA256=$(sed -n 's/^SHA-256: //p' internal/server/web/terminal.PROVENANCE)
test "$RECORDED_SHA256" = "54382cfc04c064df22f6179453bb3eb85c50fd9cf855f7b57adfbe8c8f75b0f8"
IMPLEMENTATION_BASE=$(git log -1 --format=%H -- \
  docs/superpowers/plans/2026-08-06-circle-of-the-fleet-static-mockup.md)
git diff --name-only "$IMPLEMENTATION_BASE"..HEAD
```

Expected checksum output: `internal/server/web/terminal.css: OK`.

Expected changed implementation paths:

```text
internal/server/handler.go
internal/server/handler_test.go
internal/server/web/app.css
internal/server/web/index.html
internal/server/web/terminal.LICENSE
internal/server/web/terminal.PROVENANCE
internal/server/web/terminal.css
```

The implementation range must not include `go.mod`, `go.sum`, `main.go`, `cmd/**`, `internal/server/server.go`, or existing Datastar assets.

- [ ] **Step 3: Run the loopback smoke test**

Run:

```bash
rm -f .tmp/server.stdout .tmp/server.stderr .tmp/index.html .tmp/status.sse
.tmp/kanedias server --listen 127.0.0.1:18080 \
  >.tmp/server.stdout 2>.tmp/server.stderr &
SERVER_PID=$!
trap 'kill "$SERVER_PID" 2>/dev/null || true' EXIT
READY=0
for attempt in $(seq 1 50); do
  if curl --fail --silent http://127.0.0.1:18080/healthz >/dev/null; then
    READY=1
    break
  fi
  sleep 0.1
done
test "$READY" -eq 1
test "$(curl --fail --silent http://127.0.0.1:18080/healthz)" = "ok"
curl --fail --silent http://127.0.0.1:18080/ >.tmp/index.html
grep -F 'KANEDIAS // CIRCLE OF THE FLEET' .tmp/index.html
grep -F 'id="fleet-orbit"' .tmp/index.html
grep -F 'id="maker-aperture"' .tmp/index.html
grep -F 'id="question-alert"' .tmp/index.html
grep -F 'id="command-deck"' .tmp/index.html
grep -F 'PTY-OWNER' .tmp/index.html
grep -F 'STATIC MOCKUP' .tmp/index.html
curl --fail --silent http://127.0.0.1:18080/assets/terminal.css >/dev/null
curl --fail --silent http://127.0.0.1:18080/assets/app.css >/dev/null
curl --fail --silent http://127.0.0.1:18080/assets/datastar.js >/dev/null
curl --fail --silent --no-buffer http://127.0.0.1:18080/ui/status >.tmp/status.sse
grep -F 'event: datastar-patch-elements' .tmp/status.sse
grep -F 'server-status' .tmp/status.sse
for method in POST PUT PATCH DELETE; do
  test "$(curl --silent --output /dev/null --write-out '%{http_code}' \
    --request "$method" http://127.0.0.1:18080/assets/terminal.css)" = "405"
done
test "$(curl --silent --output /dev/null --write-out '%{http_code}' \
  http://127.0.0.1:18080/not-found)" = "404"
kill -TERM "$SERVER_PID"
wait "$SERVER_PID"
trap - EXIT
grep -E 'server.*(start|listen)' .tmp/server.stderr
grep -E 'server.*(stop|shutdown)' .tmp/server.stderr
rm -rf .tmp
git status --short --untracked-files=all
```

Expected: every route and marker check passes, the existing status SSE remains functional, unsupported methods are `405`, unknown paths are `404`, shutdown exits `0`, and status is empty.

- [ ] **Step 4: Request one independent final review**

Use the `superpowers:requesting-code-review` skill. Give the reviewer:

- `docs/superpowers/specs/2026-08-06-circle-of-the-fleet-static-mockup-design.md`;
- this plan;
- the exact implementation range from the plan-document commit through `HEAD` (compute the base with `git log -1 --format=%H -- docs/superpowers/plans/2026-08-06-circle-of-the-fleet-static-mockup.md`);
- automated verification and smoke evidence.

Require findings only for correctness/regressions, static-scope violations, embedding/provenance, accessibility, color-only state, disabled-control semantics, test adequacy, or divergence from the approved Circle composition. The reviewer must not propose mobile behavior, live bridge/RPC work, or unrelated refactoring.

- [ ] **Step 5: Disposition findings and commit focused fixes if required**

For each actionable finding:

1. reproduce it or identify the exact violated requirement;
2. add the narrowest failing test for behavioral changes;
3. run the focused test RED;
4. apply the smallest fix;
5. run the focused test GREEN.

If fixes were needed, run:

```bash
git add internal/server/handler.go internal/server/handler_test.go internal/server/web
git diff --cached --check
git commit -m "fix: address Circle mockup review"
```

If the review has no actionable findings, create no empty commit. Do not broaden scope to optional polish.

- [ ] **Step 6: Re-run final verification after review**

Run whether or not fixes were required:

```bash
gofmt -w internal/server/handler.go internal/server/handler_test.go
git diff --exit-code
go test ./internal/server -count=1
go test ./... -count=1
go test -race ./internal/server ./cmd -count=1
go vet ./...
go build -trimpath -o /tmp/kanedias-circle-verified .
printf '%s  %s\n' \
  54382cfc04c064df22f6179453bb3eb85c50fd9cf855f7b57adfbe8c8f75b0f8 \
  internal/server/web/terminal.css | sha256sum -c -
git diff --check
git status --short --untracked-files=all
```

Expected: all commands pass, checksum is `OK`, and status is empty.

- [ ] **Step 7: Integrate into local `main` and push without force**

Record the feature branch and final commit in the isolated worktree:

```bash
FEATURE_BRANCH=$(git branch --show-current)
FINAL_COMMIT=$(git rev-parse HEAD)
printf 'feature branch: %s\nfinal commit: %s\n' "$FEATURE_BRANCH" "$FINAL_COMMIT"
```

In the original checkout where `main` is active:

```bash
git status --short --branch
git fetch origin
git merge --ff-only "$FEATURE_BRANCH"
git push origin main
git status --short --branch
git log -4 --oneline --decorate
```

Expected: the original checkout is clean before merge, `main` fast-forwards to the reviewed feature commit, the non-force push succeeds, and final status reports `main...origin/main` with no ahead/behind count. If `origin/main` advanced and prevents a fast-forward push, return to the isolated feature branch, rebase it onto the fetched `origin/main`, rerun Step 6 in full, then retry the fast-forward merge and push. Never force-push.

## Final Acceptance Checklist

- [ ] Terminal.css is unchanged, MIT-attributed, provenance-recorded, checksum-verified, embedded, and served locally.
- [ ] Terminal.css loads before project CSS and both load before the existing local Datastar module.
- [ ] The old status/dashboard/session placeholder page is gone.
- [ ] The fixed Circle of the Fleet shows four parent runs, four nested moons, question state, lineage, transcript, metrics, and command deck.
- [ ] Stable IDs are exactly `fleet-orbit`, `maker-aperture`, `question-alert`, and `command-deck`.
- [ ] Every state uses symbol plus text; every action preview is a disabled native button.
- [ ] No live request, form, editable field, external asset, animation, bridge, RPC, shell, or mobile implementation was added.
- [ ] Existing health and one-shot Datastar status behavior still pass unit and curl verification.
- [ ] Focused, full, race, vet, build, checksum, source scans, and smoke tests pass after review.
- [ ] Reviewed commits are fast-forwarded to local `main` and pushed to `origin/main` without force.
