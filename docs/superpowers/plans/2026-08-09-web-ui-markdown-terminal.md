# Pi-Like Web Transcript and Terminal Controls Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Render safe Pi-like Markdown, highlighted code, expandable tool details, and live terminal-style controls in the Kanedias web console.

**Architecture:** Keep Go `html/template` as the source-escaping boundary, then render marked transcript nodes with vendored copies of Pi's browser Markdown stack after Datastar patches. Extend the manager's allowlisted activity projection for bounded tool display data, and project action capabilities from current server state so keyboard and button behavior consume live truth rather than click-time lifecycle guesses.

**Tech Stack:** Go 1.26.5, `html/template`, embedded static assets, Datastar browser patches, vanilla JavaScript (UMD modules), Node's built-in test runner, vendored `marked` 18.0.5, and Pi's vendored Highlight.js browser build.

## Global Constraints

- Work only in `/home/steven/source/github/kanedias/.worktrees/web-ui-markdown-terminal` on branch `feat/web-ui-markdown-terminal`.
- Preserve the no-framework, no-bundler web architecture; do not add React, Vue, Svelte, npm dependencies, or a runtime CDN.
- User and assistant text supports GFM, Pi-style line breaks and strict strikethrough, inline code, fenced code, tables, links, lists, blockquotes, and syntax highlighting.
- Raw model/tool HTML is always literal text; URL schemes are limited to `http`, `https`, `mailto`, `tel`, and `ftp`.
- Tool arguments and output are each limited to 64 KiB per activity item and truncated at valid UTF-8 boundaries with a visible marker.
- Tool details are collapsed by default; individual `<details>` cards and global `Ctrl-O` expansion are both supported.
- `Ctrl-A` moves the deck caret to the start; `Ctrl-C` copies an existing selection or clears the deck with no selection; `Escape` interrupts only a currently interruptible session; Enter sends and clears.
- `CanSteer` and `CanInterrupt` require a non-stale route; `CanInterrupt` additionally requires `running`; transient event-stream disconnection alone does not disable actions; stale retained roots remain stoppable.
- Browser disabled state is never authorization; all existing authenticated, same-origin POST and manager validation remains in force.
- `make test` must run both Go and JavaScript tests.
- Commit after every task, request independent review before the PR, and merge only after required GitHub checks are green.

## File Structure

### New files

- `internal/server/web/marked.min.js` — exact vendored Marked 18.0.5 UMD build from Pi's HTML exporter.
- `internal/server/web/highlight.min.js` — exact vendored Highlight.js UMD build from Pi's HTML exporter.
- `internal/server/web/markdown-renderer.js` — isolated Markdown configuration, URL policy, rendering, and code highlighting.
- `internal/server/web/markdown-renderer.test.js` — Node tests for Markdown parity and injection defenses.
- `internal/server/web/terminal-ui.js` — pure keyboard and global tool-toggle decisions shared by browser handlers and tests.
- `internal/server/web/terminal-ui.test.js` — Node tests for Pi-compatible key decisions and tool expansion.

### Modified files

- `Makefile` — include Node browser-unit tests in `make test`.
- `internal/manager/types.go` — add bounded tool display fields to `ActivityItem`.
- `internal/manager/projection.go` — extract, correlate, summarize, format, and bound tool arguments/results.
- `internal/manager/projection_test.go` — test tool display projection and truncation.
- `internal/server/view.go` — mark Markdown items, carry tool display fields, and compute action capabilities.
- `internal/server/web/activity.html` — emit Markdown source markers and collapsed semantic tool cards.
- `internal/server/web/detail.html` — expose inert capability attributes on the stable detail root.
- `internal/server/web/index.html` — load local vendor/helpers, add key hints, and annotate controls.
- `internal/server/web/app.js` — process patched Markdown, copy code/output, synchronize controls, and apply key actions.
- `internal/server/web/app.css` — Pi-like Markdown/code/tool styling and armed Interrupt state.
- `internal/server/handler.go` — serve the four new embedded JavaScript assets.
- `internal/server/handler_test.go` — verify local asset wiring and stable browser shell behavior.
- `internal/server/questions_render_test.go` — verify capability/template safety cases where appropriate.

---

### Task 1: Safe Pi-Compatible Markdown and Highlighted Code

**Files:**
- Create: `internal/server/web/marked.min.js`
- Create: `internal/server/web/highlight.min.js`
- Create: `internal/server/web/markdown-renderer.js`
- Create: `internal/server/web/markdown-renderer.test.js`
- Modify: `Makefile`
- Modify: `internal/server/view.go`
- Modify: `internal/server/web/activity.html`
- Modify: `internal/server/web/index.html`
- Modify: `internal/server/web/app.js`
- Modify: `internal/server/web/app.css`
- Modify: `internal/server/handler.go`
- Modify: `internal/server/handler_test.go`

**Interfaces:**
- Produces browser global/CommonJS export `KanediasMarkdown` with `render(text: string): string`, `sanitizeURL(value: unknown): string | null`, and `renderPending(root: ParentNode): void`.
- Produces `activityItemView.IsMarkdown bool`; only `message_update` and `user_message` are Markdown.
- Produces local routes `/assets/marked.min.js`, `/assets/highlight.min.js`, and `/assets/markdown-renderer.js`.
- Later tasks reuse `KanediasMarkdown.highlight(code, language)` for tool arguments/results.

- [ ] **Step 1: Add failing view/template and asset tests**

In `internal/server/handler_test.go`, add assertions that a rendered user/assistant activity body has a Markdown marker but an error body does not, and that the shell references only the three new local assets:

```go
func TestActivityMarksOnlyConversationTextAsMarkdown(t *testing.T) {
	templates, err := parseTemplates(webFiles)
	if err != nil { t.Fatal(err) }
	view := activityView{Items: []activityItemView{
		{Kind: "user_message", Label: "You", Text: "# prompt", IsMarkdown: true},
		{Kind: "message_update", Label: "Message", Text: "```go\npackage p\n```", IsMarkdown: true},
		{Kind: "model_error", Label: "Model error", Text: "**not markup**", IsError: true},
	}}
	html, err := renderTemplate(templates, templateActivity, view)
	if err != nil { t.Fatal(err) }
	if got := strings.Count(html, `data-markdown`); got != 2 {
		t.Fatalf("markdown markers = %d, want 2\n%s", got, html)
	}
	if strings.Contains(html, `<h1>`) || strings.Contains(html, `<script`) {
		t.Fatalf("server trusted transcript markup:\n%s", html)
	}
}
```

Extend `TestAstrolabeConsoleIsInteractive` to require local `marked.min.js`, `highlight.min.js`, and `markdown-renderer.js` script sources and reject `http://`/`https://` script sources.

- [ ] **Step 2: Run focused Go tests and verify RED**

Run:

```bash
go test ./internal/server -run 'TestActivityMarksOnlyConversationTextAsMarkdown|TestAstrolabeConsoleIsInteractive' -count=1
```

Expected: FAIL because `activityItemView.IsMarkdown` and the local renderer assets do not exist.

- [ ] **Step 3: Add failing Markdown JavaScript tests**

Create `internal/server/web/markdown-renderer.test.js` using `node:test` and `node:assert/strict`:

```js
const test = require("node:test");
const assert = require("node:assert/strict");
const renderer = require("./markdown-renderer.js");

test("renders GFM and highlighted fenced code", () => {
  const html = renderer.render("# Title\n\n| A | B |\n|---|---|\n| 1 | 2 |\n\n```js\nconst n = 1;\n```");
  assert.match(html, /<h1>Title<\/h1>/);
  assert.match(html, /<table>/);
  assert.match(html, /class="hljs"/);
  assert.match(html, /hljs-keyword/);
});

test("renders raw HTML literally and rejects active URLs", () => {
  const html = renderer.render('<img src=x onerror=alert(1)> [bad](javascript:alert(1)) [ok](https://example.com)');
  assert.doesNotMatch(html, /<img\b/i);
  assert.doesNotMatch(html, /href="javascript:/i);
  assert.match(html, /&lt;img src=x onerror=alert/);
  assert.match(html, /href="https:\/\/example.com"/);
  assert.match(html, /rel="noopener noreferrer"/);
  assert.equal(renderer.sanitizeURL("java\u0000script:alert(1)"), null);
});
```

- [ ] **Step 4: Run the JavaScript test and verify RED**

Run:

```bash
node --test internal/server/web/markdown-renderer.test.js
```

Expected: FAIL with `Cannot find module './markdown-renderer.js'`.

- [ ] **Step 5: Vendor Pi's exact browser libraries**

Copy, without modifying minified contents:

```bash
cp /home/steven/.local/lib/node_modules/@earendil-works/pi-coding-agent/dist/core/export-html/vendor/marked.min.js internal/server/web/marked.min.js
cp /home/steven/.local/lib/node_modules/@earendil-works/pi-coding-agent/dist/core/export-html/vendor/highlight.min.js internal/server/web/highlight.min.js
```

Verify the Marked banner reports `v18.0.5` and preserve both upstream license banners.

- [ ] **Step 6: Implement the isolated Markdown renderer**

Create `internal/server/web/markdown-renderer.js` as a UMD module. In CommonJS, require the two vendored files; in the browser, consume `globalThis.marked` and `globalThis.hljs`. Export exactly:

```js
{
  render,
  renderPending,
  sanitizeURL,
  highlight
}
```

Configure Marked once with Pi's `breaks`, `gfm`, strict strikethrough tokenizer, literal HTML tokenizer, URL allow-list, safe link/image renderers, highlighted fenced-code renderer, and escaped inline-code renderer. Code blocks must return:

```html
<div class="code-block"><button type="button" class="copy-btn" data-copy-code>copy</button><pre><code class="hljs">…</code></pre></div>
```

`renderPending(root)` must select `[data-markdown]:not([data-markdown-rendered])`, retain each element's original `textContent`, call `render`, set `innerHTML`, and set `data-markdown-rendered="true"`. On failure, leave `textContent` unchanged and set `data-markdown-error="true"`.

- [ ] **Step 7: Mark only user and assistant activity as Markdown**

Add `IsMarkdown bool` to `activityItemView`. In `newActivityView`, set it with an explicit helper:

```go
func activityUsesMarkdown(kind string) bool {
	return kind == "message_update" || kind == "user_message"
}
```

Change `activity.html` to render:

```html
{{if .Text}}<div class="t-body"{{if .IsMarkdown}} data-markdown{{end}}>{{.Text}}</div>{{end}}
```

Do not use `template.HTML`.

- [ ] **Step 8: Serve and load the local renderer assets**

In `handler.go`, add embedded-asset handlers/routes for all three files with `text/javascript; charset=utf-8`. In `index.html`, load them in this order before `app.js`:

```html
<script src="/assets/marked.min.js"></script>
<script src="/assets/highlight.min.js"></script>
<script src="/assets/markdown-renderer.js"></script>
<script type="module" src="/assets/app.js"></script>
```

No script may reference a remote origin.

- [ ] **Step 9: Render Markdown after every activity patch and add copy behavior**

In `app.js`, add one activity-panel observer callback that calls `window.KanediasMarkdown.renderPending(panel)` before auto-scroll measurement. Add delegated `[data-copy-code]` handling that copies the sibling `code.textContent`, stops propagation, and changes button text to `copied` or `copy failed` for 1.2 seconds. Use `navigator.clipboard.writeText` when available and a hidden textarea/`document.execCommand("copy")` fallback otherwise.

- [ ] **Step 10: Add scoped Markdown/code styling**

In `app.css`, scope every rule under `.transcript .t-body[data-markdown-rendered]` or `.code-block`. Include headings, paragraphs, nested lists, task checkboxes, blockquotes, links, inline code, horizontal rules, horizontally scrollable tables, fenced code, focus-visible copy buttons, selected text, and the Highlight.js token classes already mapped by the Astrolabe palette. Override the global terminal stylesheet's backtick pseudo-content inside rendered Markdown:

```css
.transcript .t-body[data-markdown-rendered] code::before,
.transcript .t-body[data-markdown-rendered] code::after { content:none; }
```

- [ ] **Step 11: Make JavaScript tests part of `make test`**

Modify `Makefile`:

```make
test: ## Run the hermetic test suite (no Incus, no network)
	go test ./...
	node --test internal/server/web/*.test.js
```

- [ ] **Step 12: Run task tests and verify GREEN**

Run:

```bash
node --test internal/server/web/markdown-renderer.test.js
go test ./internal/server -count=1
make test
```

Expected: all commands PASS.

- [ ] **Step 13: Commit Task 1**

```bash
git add Makefile internal/server/handler.go internal/server/handler_test.go internal/server/view.go internal/server/web/activity.html internal/server/web/index.html internal/server/web/app.js internal/server/web/app.css internal/server/web/marked.min.js internal/server/web/highlight.min.js internal/server/web/markdown-renderer.js internal/server/web/markdown-renderer.test.js
git commit -m "feat(ui): render safe Pi-like markdown"
```

---

### Task 2: Bounded Expandable Tool Inputs and Results

**Files:**
- Modify: `internal/manager/types.go`
- Modify: `internal/manager/projection.go`
- Modify: `internal/manager/projection_test.go`
- Modify: `internal/server/view.go`
- Modify: `internal/server/web/activity.html`
- Modify: `internal/server/web/app.js`
- Modify: `internal/server/web/app.css`
- Modify: `internal/server/handler_test.go`

**Interfaces:**
- Extends `manager.ActivityItem` and `activityItemView` with `IsTool bool`, `ToolSummary string`, `ToolArgs string`, `ToolOutput string`, `ToolLanguage string`, and `ToolTruncated bool`.
- Adds manager constant `maxToolDisplayBytes = 64 << 10`.
- Adds pure manager helpers `boundedDisplay(string) (string, bool)`, `formatToolJSON(json.RawMessage) (string, bool)`, `formatToolResult(json.RawMessage) (string, bool)`, `summarizeTool(string, json.RawMessage) string`, and `toolLanguage(string, json.RawMessage) string`.
- Reuses `window.KanediasMarkdown.highlight(code, language)` from Task 1.

- [ ] **Step 1: Write failing manager projection tests**

Add tests covering start, update, final, common summary, custom fallback, and UTF-8 truncation. Use real Pi-shaped payloads:

```go
func TestProjectActivityRetainsBoundedToolDisplay(t *testing.T) {
	events := []supervisor.EventEnvelope{
		piEvent(1, "s", "tool_execution_start", map[string]any{
			"toolCallId": "tc-1", "toolName": "read",
			"args": map[string]any{"path": "internal/server/view.go"},
		}),
		piEvent(2, "s", "tool_execution_update", map[string]any{
			"toolCallId": "tc-1", "toolName": "read",
			"partialResult": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "package server"},
			}},
		}),
		piEvent(3, "s", "tool_execution_end", map[string]any{
			"toolCallId": "tc-1", "toolName": "read", "isError": false,
			"result": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "package server\n"},
			}},
		}),
	}
	items := projectActivity(events, "s")
	if len(items) != 1 { t.Fatalf("items = %#v", items) }
	item := items[0]
	if item.ToolSummary != "read internal/server/view.go" || item.ToolLanguage != "go" {
		t.Fatalf("tool display = %#v", item)
	}
	if !strings.Contains(item.ToolArgs, `"path": "internal/server/view.go"`) || item.ToolOutput != "package server\n" {
		t.Fatalf("tool details = %#v", item)
	}
}
```

Add a truncation test with `strings.Repeat("界", maxToolDisplayBytes)` and assert `utf8.ValidString`, `ToolTruncated`, byte bound, and suffix `\n… display truncated …`.

- [ ] **Step 2: Run focused manager tests and verify RED**

Run:

```bash
go test ./internal/manager -run 'TestProjectActivityRetainsBoundedToolDisplay|TestToolDisplayTruncatesUTF8Safely' -count=1
```

Expected: FAIL because tool display fields/helpers do not exist.

- [ ] **Step 3: Extend the allowlisted activity type**

Add the six specified fields to `manager.ActivityItem`. Do not add raw `any`, `map[string]any`, `template.HTML`, base64 image data, or an unbounded raw event field.

- [ ] **Step 4: Parse and bound Pi tool payloads**

Extend `toolPayload` with:

```go
Args          json.RawMessage `json:"args"`
PartialResult json.RawMessage `json:"partialResult"`
Result        json.RawMessage `json:"result"`
```

Implement `boundedDisplay` by reserving bytes for `\n… display truncated …`, cutting before the limit, backing up while `!utf8.ValidString`, and returning the marker. Empty input stays empty and untruncated.

Implement JSON indentation with `json.Indent`; malformed JSON falls back to the bounded original string. Implement result extraction from `content` blocks with `{type,text,mimeType}`: join text blocks in order, summarize image blocks as `[image: <mime>]`, and use indented result JSON only when no supported content block exists.

- [ ] **Step 5: Add common summaries and language hints**

Decode only the argument keys needed for presentation. Exact summaries:

- `bash`: first command line, prefixed `$ ` and capped at 160 display characters.
- `read`, `write`, `edit`, `ls`: `<tool> <path>` using `path` then `file_path`.
- `grep`, `find`: `<tool> <pattern> in <path-or-dot>`.
- custom: tool name.

Infer Highlight.js languages from common extensions (`go`, `js`, `ts`, `tsx`, `jsx`, `py`, `rb`, `rs`, `java`, `c`, `cpp`, `cs`, `php`, `sh`, `sql`, `html`, `css`, `scss`, `json`, `yaml`, `xml`, `md`, and `dockerfile`). Return `json` for argument blocks and an empty output language when no source-path inference applies.

- [ ] **Step 6: Correlate start/update/end display state**

At start, store formatted arguments, summary, language, running status, and truncation. At update, replace `ToolOutput` with the accumulated `partialResult` projection. At end, replace it with final `result`, set status `done`, merge truncation state, and preserve `IsError`.

- [ ] **Step 7: Add failing tool-card template tests**

In `handler_test.go`, render one tool item containing `</pre><script>alert(1)</script>` in arguments/output. Assert:

```go
if !strings.Contains(html, `<details class="tool-card`) { t.Fatal(html) }
if strings.Contains(html, `<script>alert`) { t.Fatalf("unescaped tool content: %s", html) }
if !strings.Contains(html, `&lt;script&gt;alert`) { t.Fatalf("missing escaped source: %s", html) }
if strings.Contains(html, `<details class="tool-card" open`) { t.Fatalf("tool defaulted open: %s", html) }
```

- [ ] **Step 8: Render semantic collapsed tool cards**

Copy the new fields into `activityItemView`. In `activity.html`, branch on `IsTool` and render a collapsed `<details class="tool-card ..." data-tool-card>` with a `<summary>`, status, summary, truncation indicator, and escaped `<pre><code>` blocks for arguments/output. Add `data-tool-code` and `data-language` attributes to blocks for browser highlighting, plus `data-copy-tool` buttons. Non-tool messages retain Task 1's body.

- [ ] **Step 9: Highlight and copy patched tool blocks**

Extend the existing activity observer in `app.js` to find `[data-tool-code]:not([data-highlighted])`, call `KanediasMarkdown.highlight(textContent, dataset.language)`, replace `innerHTML`, and mark the code node. Add delegated copy handling for `[data-copy-tool]`, reusing Task 1's clipboard helper and stopping propagation so copy never toggles `<details>`.

- [ ] **Step 10: Style compact tool cards**

Add scoped styles for pending/success/error borders, summary focus/hover, status labels, truncation labels, nested argument/result sections, preformatted horizontal scrolling, and copy buttons. Preserve text selection. Keep cards compact when collapsed and constrain expanded output within the transcript width.

- [ ] **Step 11: Run task tests and verify GREEN**

Run:

```bash
go test ./internal/manager -count=1
go test ./internal/server -count=1
node --test internal/server/web/markdown-renderer.test.js
make test
```

Expected: all commands PASS.

- [ ] **Step 12: Commit Task 2**

```bash
git add internal/manager/types.go internal/manager/projection.go internal/manager/projection_test.go internal/server/view.go internal/server/web/activity.html internal/server/web/app.js internal/server/web/app.css internal/server/handler_test.go
git commit -m "feat(ui): show expandable tool details"
```

---

### Task 3: Pi Keyboard Semantics and Live Action Capabilities

**Files:**
- Create: `internal/server/web/terminal-ui.js`
- Create: `internal/server/web/terminal-ui.test.js`
- Modify: `internal/server/view.go`
- Modify: `internal/server/web/detail.html`
- Modify: `internal/server/web/index.html`
- Modify: `internal/server/web/app.js`
- Modify: `internal/server/web/app.css`
- Modify: `internal/server/handler.go`
- Modify: `internal/server/handler_test.go`
- Modify: `internal/server/questions_render_test.go`

**Interfaces:**
- Adds `detailView.CanSteer`, `detailView.CanInterrupt`, and `detailView.CanStop`.
- Adds Go helper `newActionCapabilities(state manager.SessionState) actionCapabilities` where `actionCapabilities` contains the three booleans.
- Produces browser global/CommonJS `KanediasTerminalUI` with `keyAction(event, context): string | null` and `nextToolExpansion(openStates: boolean[]): boolean`.
- Adds local route `/assets/terminal-ui.js`.
- Detail root exposes `data-can-steer`, `data-can-interrupt`, and `data-can-stop` as literal `true`/`false` strings.

- [ ] **Step 1: Write failing capability table tests**

Add a table test in `questions_render_test.go` (or a new focused server test file if clearer):

```go
func TestActionCapabilitiesFollowCurrentSessionState(t *testing.T) {
	cases := []struct {
		name, lifecycle string
		stale, connected bool
		steer, interrupt, stop bool
	}{
		{"ready", "ready", false, true, true, false, true},
		{"running", "running", false, true, true, true, true},
		{"running stream reconnect", "running", false, false, true, true, true},
		{"writer handoff", "awaiting_handoff", false, true, true, false, true},
		{"stale running", "running", true, false, false, false, true},
		{"completed", "completed", false, true, false, false, true},
		{"stopping", "stopping", false, true, false, false, false},
		{"stopped", "stopped", false, true, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := manager.SessionState{RootStale: tc.stale, StreamConnected: tc.connected,
				Node: supervisor.NodeSnapshot{SessionID: "s", Lifecycle: tc.lifecycle}}
			got := newActionCapabilities(state)
			if got.CanSteer != tc.steer || got.CanInterrupt != tc.interrupt || got.CanStop != tc.stop {
				t.Fatalf("capabilities = %#v", got)
			}
		})
	}
}
```

- [ ] **Step 2: Run focused capability tests and verify RED**

Run:

```bash
go test ./internal/server -run TestActionCapabilitiesFollowCurrentSessionState -count=1
```

Expected: FAIL because `newActionCapabilities` does not exist.

- [ ] **Step 3: Implement and expose server capabilities**

Implement the helper with supervisor lifecycle constants. `CanSteer` is true only for non-stale `ready`, `running`, and `awaiting_handoff`. `CanInterrupt` is true only for non-stale `running`. `CanStop` is true for every nonempty lifecycle except `stopping` and `stopped`, including stale states. Do not gate on `StreamConnected`.

Populate the three `detailView` fields and render them on the stable detail root:

```html
<div id="detail-panel"
  data-can-steer="{{.CanSteer}}"
  data-can-interrupt="{{.CanInterrupt}}"
  data-can-stop="{{.CanStop}}">
```

The empty-state root emits all false.

- [ ] **Step 4: Add failing pure keyboard tests**

Create `terminal-ui.test.js`:

```js
const test = require("node:test");
const assert = require("node:assert/strict");
const ui = require("./terminal-ui.js");

const event = (key, extra = {}) => ({ key, ctrlKey: false, altKey: false, metaKey: false, shiftKey: false, isComposing: false, ...extra });

test("matches Pi editor and interrupt keys without stealing copy", () => {
  assert.equal(ui.keyAction(event("a", {ctrlKey:true}), {target:"deck", hasSelection:false, canInterrupt:false}), "line-start");
  assert.equal(ui.keyAction(event("c", {ctrlKey:true}), {target:"deck", hasSelection:true, canInterrupt:false}), null);
  assert.equal(ui.keyAction(event("c", {ctrlKey:true}), {target:"deck", hasSelection:false, canInterrupt:false}), "clear");
  assert.equal(ui.keyAction(event("Escape"), {target:"deck", hasSelection:false, canInterrupt:true}), "interrupt");
  assert.equal(ui.keyAction(event("Escape"), {target:"deck", hasSelection:false, canInterrupt:false}), null);
  assert.equal(ui.keyAction(event("o", {ctrlKey:true}), {target:"body", hasSelection:false, canInterrupt:false}), "toggle-tools");
});

test("global tool toggle expands unless every card is open", () => {
  assert.equal(ui.nextToolExpansion([]), true);
  assert.equal(ui.nextToolExpansion([true, false]), true);
  assert.equal(ui.nextToolExpansion([true, true]), false);
});
```

- [ ] **Step 5: Run keyboard tests and verify RED**

Run:

```bash
node --test internal/server/web/terminal-ui.test.js
```

Expected: FAIL with `Cannot find module './terminal-ui.js'`.

- [ ] **Step 6: Implement the pure terminal decision module**

Create a dependency-free UMD module. `keyAction` returns only `submit`, `line-start`, `clear`, `interrupt`, `toggle-tools`, or `null`. It returns null for composition, Alt/Meta/Shift modified events, unrelated editable controls, and `Ctrl-C` with any selection. Enter submits only for deck target. `Ctrl-A` and clear apply only to deck/body focus according to the spec. `nextToolExpansion` returns `!openStates.every(Boolean)` and therefore true for an empty list.

- [ ] **Step 7: Serve/load terminal decisions and disclose shortcuts**

Serve `/assets/terminal-ui.js` locally and load it before `app.js`. Add `aria-keyshortcuts` and title text to deck controls:

- deck input: `Control+A Control+C Enter`;
- Interrupt: `Escape`;
- a compact deck hint: `^A home · ^C clear/copy · esc abort · ^O tools`.

- [ ] **Step 8: Replace click-time lifecycle guesses with detail synchronization**

Delete the old `setDeckState(sessionID, lifecycle)` lifecycle list. Add `syncDeckState()` that reads the three detail attributes and applies `disabled`, `aria-disabled`, and `.armed` to controls. Observe `#detail-panel` replacement through `#main-stack` and call synchronization after each patch. On row click, disable Steer/Interrupt/Stop immediately until the selected detail patch arrives.

- [ ] **Step 9: Apply Pi key actions and persistent global tool mode**

In the delegated document keydown handler:

1. Compute selection from both `window.getSelection()` and `selectionStart !== selectionEnd` for inputs/textareas.
2. Classify target as `deck`, `body`, or `other-editable`.
3. Call `KanediasTerminalUI.keyAction`.
4. For `line-start`, prevent default, focus deck, and call `setSelectionRange(0, 0)`.
5. For `clear`, prevent default, clear the deck, emit bubbling `input`, and focus it.
6. For `interrupt`, prevent default and click the enabled Interrupt button.
7. For `toggle-tools`, prevent default, choose expansion with `nextToolExpansion`, update every `[data-tool-card]`, and store the boolean in page-local `toolExpansionMode`.
8. For `submit`, preserve existing click-and-clear behavior.

When the activity observer sees fresh cards and `toolExpansionMode !== null`, set each card's `open` property to the stored mode.

- [ ] **Step 10: Fix Interrupt affordance styling**

Add `.dbtn.interrupt.armed:not(:disabled)` with an amber glow, stronger border/background, and visible `ESC` keycap. Ensure the armed state disappears under `:disabled`. Add focus-visible styles and prevent the shortcut hint from squeezing the mobile deck by hiding or wrapping it under the existing responsive breakpoints.

- [ ] **Step 11: Extend shell/template tests**

Assert the terminal helper is local and present, the detail root renders literal capability attributes, `aria-keyshortcuts` are present, and no inline key event handler or remote asset was introduced.

- [ ] **Step 12: Run task tests and verify GREEN**

Run:

```bash
node --test internal/server/web/terminal-ui.test.js
node --test internal/server/web/*.test.js
go test ./internal/server -count=1
make test
```

Expected: all commands PASS.

- [ ] **Step 13: Commit Task 3**

```bash
git add internal/server/view.go internal/server/web/detail.html internal/server/web/index.html internal/server/web/app.js internal/server/web/app.css internal/server/web/terminal-ui.js internal/server/web/terminal-ui.test.js internal/server/handler.go internal/server/handler_test.go internal/server/questions_render_test.go
git commit -m "fix(ui): mirror Pi terminal controls"
```

---

### Task 4: Integration Verification, Independent Review, and PR Delivery

**Files:**
- Modify only files required by verified review findings.
- Update: `docs/superpowers/plans/2026-08-09-web-ui-markdown-terminal.md` checkboxes as tasks complete.

**Interfaces:**
- Consumes all Task 1–3 interfaces.
- Produces a clean, reviewed branch and merged GitHub PR.

- [ ] **Step 1: Run formatting and static checks**

Run:

```bash
gofmt -w internal/manager/types.go internal/manager/projection.go internal/manager/projection_test.go internal/server/view.go internal/server/handler.go internal/server/handler_test.go internal/server/questions_render_test.go
git diff --check
```

Expected: no output from `git diff --check`.

- [ ] **Step 2: Run the complete local verification suite**

Run separately and retain outputs for the final report:

```bash
make test
make build
make lint
```

Expected: all commands exit 0. If `golangci-lint` is unavailable locally, run the exact CI formatter check plus `go vet ./...`, report the missing local binary, and rely on the pinned GitHub lint job before merge.

- [ ] **Step 3: Perform security-focused source checks**

Run:

```bash
rg -n 'template\.HTML|innerHTML|javascript:|data:|on(click|error|load)=' internal/server internal/manager --glob '!web/marked.min.js' --glob '!web/highlight.min.js' --glob '!web/datastar.js'
rg -n '<script[^>]+src="https?://' internal/server/web
```

Expected: every `innerHTML` assignment is confined to the reviewed renderer/highlighter path; no `template.HTML`, inline generated handlers, or remote script sources exist.

- [ ] **Step 4: Request independent code review**

Use the `requesting-code-review` skill with a fresh reviewer. Give it the design, plan, base commit `c539acb`, current HEAD, and require findings ranked by severity with file/line evidence, explicit XSS review, streaming/Datastar behavior review, lifecycle capability review, and missing-test review.

- [ ] **Step 5: Triage review findings rigorously**

Use the `receiving-code-review` skill. Reproduce or verify every finding before changing code. Add a failing regression test for every accepted behavioral/security issue, confirm RED, implement the smallest correction, and confirm GREEN. Reject incorrect findings with code/test evidence.

- [ ] **Step 6: Commit review corrections**

If corrections are required, the isolated worktree contains only this feature, so stage the reviewed delta and commit it:

```bash
git add -A
git commit -m "fix(ui): address transcript review findings"
```

If no corrections are required, do not create an empty commit.

- [ ] **Step 7: Re-run completion verification**

Use the `verification-before-completion` skill, then run fresh:

```bash
git status --short --branch
git diff --check origin/main...HEAD
make test
make build
make lint
git log --oneline origin/main..HEAD
```

Expected: clean worktree, no diff-check errors, all verification commands exit 0, and commits are limited to the feature.

- [ ] **Step 8: Push and create the pull request**

Push:

```bash
git push -u origin feat/web-ui-markdown-terminal
```

Create a PR to `main` with a concise summary of safe Markdown parity, bounded tool cards, Pi keyboard behavior, Interrupt capability repair, and exact test commands. Include the security decisions (literal raw HTML, URL allow-list, bounded tool fields, no external assets).

- [ ] **Step 9: Wait for required GitHub checks without premature merge**

Resolve the current PR number and watch it until every required check completes:

```bash
pr_number=$(gh pr view --json number --jq .number)
gh pr checks --watch "$pr_number"
```

If a check fails, inspect its logs, reproduce locally, add a regression test when applicable, fix, re-run local verification, commit, push, and wait again.

Expected: PR reports mergeable and every required check is green.

- [ ] **Step 10: Merge and verify the remote result**

Merge with the repository's accepted method:

```bash
pr_number=$(gh pr view --json number --jq .number)
gh pr merge "$pr_number" --squash --delete-branch
```

Then verify:

```bash
gh pr view "$pr_number" --json state,mergedAt,mergeCommit,url
```

Expected: state `MERGED`, non-null `mergedAt`, and a merge commit on `main`.

- [ ] **Step 11: Report completion evidence**

Report the PR URL, merge commit, local verification commands, GitHub check result, changed areas, and any residual limitation: the console still displays retained recent activity rather than hydrating an unbounded durable transcript.
