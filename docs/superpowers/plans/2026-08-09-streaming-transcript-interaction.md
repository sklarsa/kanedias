# Streaming Transcript Interaction Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Preserve individually opened tool cards and text selection in completed transcript content while selected-session activity continues streaming.

**Architecture:** Keep the existing whole-panel Datastar SSE flow, but make activity item identity and ownership explicit. The manager marks terminal activity immutable; the template gives every item a stable sequence-derived ID, preserves the browser-owned `open` attribute, and tells Datastar not to morph completed rendered text/code subtrees.

**Tech Stack:** Go 1.26, `html/template`, Datastar v1.0.2 preservation attributes, Node's built-in test runner.

## Global Constraints

- Work only in `/home/steven/source/github/kanedias/.worktrees/fix-streaming-transcript-state` on `fix/streaming-transcript-state`.
- Follow test-driven development: add each regression first, run it, and observe the expected failure before changing production code.
- Do not redesign the SSE protocol or add JavaScript state restoration.
- Preserve live morphing for the current assistant message and running tool output.
- Preserve existing `html/template` escaping and manager display bounds; DOM IDs may derive only from numeric activity sequence values.
- Keep tool cards server-rendered without `open`; `open` remains browser-owned.
- Do not modify vendored Datastar, Marked, or Highlight.js assets.

---

### Task 1: Project terminal activity state

**Files:**
- Modify: `internal/manager/types.go:59-91`
- Modify: `internal/manager/projection.go:48-181,426-481`
- Test: `internal/manager/projection_test.go:39-167`

**Interfaces:**
- Consumes: ordered `supervisor.EventEnvelope` events and the existing `activityProjector` item/tool maps.
- Produces: `manager.ActivityItem.Complete bool`, false for changing content and true after its terminal event.

- [ ] **Step 1: Write the failing assistant-completion test**

Add this test after `TestProjectActivitySurfacesOnlyContentInTurn`:

```go
func TestProjectActivityMarksAssistantCompleteWithoutChangingIdentity(t *testing.T) {
	projector := newActivityProjector()
	projector.Apply(piEvent(7, "s", "message_update", map[string]any{
		"assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "hello"},
	}))

	streaming := projector.Items()
	if len(streaming) != 1 || streaming[0].Seq != 7 || streaming[0].Complete {
		t.Fatalf("streaming item = %#v", streaming)
	}

	projector.Apply(piEvent(8, "s", "message_end", map[string]any{
		"message": map[string]any{
			"role": "assistant", "stopReason": "stop",
			"content": []any{map[string]any{"type": "text", "text": "hello"}},
		},
	}))
	completed := projector.Items()
	if len(completed) != 1 || completed[0].Seq != 7 || !completed[0].Complete {
		t.Fatalf("completed item = %#v", completed)
	}
}
```

Also extend `TestProjectActivityToolLifecycle` with:

```go
if !items[0].Complete {
	t.Fatal("completed tool should be immutable")
}
```

Extend `TestProjectActivityShowsPromptAndCoalescesRepeatedProviderError`, `TestProjectActivityUnknownEventBecomesGeneric`, and `TestProjectActivityExtensionError` to require their one-shot user/error/generic items to have `Complete == true`.

- [ ] **Step 2: Run focused tests and verify RED**

Run:

```bash
go test ./internal/manager -run 'TestProjectActivity(MarksAssistantCompleteWithoutChangingIdentity|ToolLifecycle|ShowsPromptAndCoalescesRepeatedProviderError|UnknownEventBecomesGeneric|ExtensionError)$' -count=1
```

Expected: FAIL to compile because `ActivityItem.Complete` does not exist.

- [ ] **Step 3: Add the completion field and minimal projector transitions**

Add to `ActivityItem` in `internal/manager/types.go`:

```go
// Complete reports that later events cannot change this item's displayed
// source content. The server uses it only to protect browser-rendered DOM.
Complete bool
```

In `activityProjector`, add a helper that marks only the currently open assistant text item complete and then clears its tracking state:

```go
func (p *activityProjector) completeOpenText() {
	if !p.textOpen {
		return
	}
	for i := len(p.items) - 1; i >= 0; i-- {
		if p.items[i].Kind == "message_update" && p.items[i].Seq == p.textSeq {
			p.items[i].Complete = true
			break
		}
	}
	p.textOpen = false
	p.textSeq = 0
}
```

Move assistant completion in `applyMessageEnd` until after successful payload unmarshal, then call `p.completeOpenText()` before projecting the user/error message. This preserves the design rule that a malformed `message_end` cannot freeze content.

Set `Complete: true` when appending:

- `extension_error` items;
- unknown generic `event` items;
- `user_message` items;
- `model_error` items.

Set `p.items[idx].Complete = true` in `applyToolEnd`. Leave new assistant/tool items and tool updates at the bool zero value (`false`).

- [ ] **Step 4: Format and verify GREEN**

Run:

```bash
gofmt -w internal/manager/types.go internal/manager/projection.go internal/manager/projection_test.go
go test ./internal/manager -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit Task 1**

```bash
git add internal/manager/types.go internal/manager/projection.go internal/manager/projection_test.go
git commit -m "fix(manager): track completed transcript activity"
```

---

### Task 2: Preserve browser-owned transcript state during morphs

**Files:**
- Modify: `internal/server/view.go:103-138,350-382`
- Modify: `internal/server/web/activity.html`
- Test: `internal/server/handler_test.go:448-480,1360-1456`

**Interfaces:**
- Consumes: `manager.ActivityItem.Seq` and `manager.ActivityItem.Complete` from Task 1.
- Produces: `activityItemView.Complete bool` and template contracts `id="activity-item-<seq>"`, `data-preserve-attr="open"`, and conditional `data-ignore-morph`.

- [ ] **Step 1: Write the failing rendering-contract test**

Add this test immediately before `TestToolCardTemplateEscapesAndCollapses`:

```go
func TestActivityTemplatePreservesStableInteractiveState(t *testing.T) {
	templates, err := parseTemplates(webFiles)
	if err != nil {
		t.Fatal(err)
	}
	view := activityView{Items: []activityItemView{
		{Seq: 11, Kind: "message_update", Label: "Message", Text: "completed message", IsMarkdown: true, Complete: true},
		{Seq: 12, Kind: "message_update", Label: "Message", Text: "streaming message", IsMarkdown: true},
		{Seq: 13, Kind: "tool_execution_start", Label: "Tool: bash", IsTool: true,
			ToolArgs: "RUNNING_ARGS", ToolOutput: "RUNNING_RESULT", ToolCardClass: "tool-running", StatusLabel: "running"},
		{Seq: 14, Kind: "tool_execution_start", Label: "Tool: bash", IsTool: true, Complete: true,
			ToolArgs: "DONE_ARGS", ToolOutput: "DONE_RESULT", ToolCardClass: "tool-done", StatusLabel: "done"},
	}}
	html, err := renderTemplate(templates, templateActivity, view)
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"11", "12", "13", "14"} {
		if !strings.Contains(html, `id="activity-item-`+id+`"`) {
			t.Errorf("missing stable activity ID %s:\n%s", id, html)
		}
	}
	if got := strings.Count(html, `data-preserve-attr="open"`); got != 2 {
		t.Errorf("preserved tool open attributes = %d, want 2\n%s", got, html)
	}
	if !strings.Contains(html, `<div class="t-body" data-markdown data-ignore-morph>completed message</div>`) {
		t.Errorf("completed message is morphable:\n%s", html)
	}
	if !strings.Contains(html, `<div class="t-body" data-markdown>streaming message</div>`) {
		t.Errorf("streaming message was frozen:\n%s", html)
	}
	for _, args := range []string{"RUNNING_ARGS", "DONE_ARGS"} {
		if !strings.Contains(html, `data-language="json" data-ignore-morph>`+args+`</code>`) {
			t.Errorf("tool arguments %q are morphable:\n%s", args, html)
		}
	}
	if !strings.Contains(html, `data-language="">RUNNING_RESULT</code>`) {
		t.Errorf("running tool result was frozen:\n%s", html)
	}
	if !strings.Contains(html, `data-language="" data-ignore-morph>DONE_RESULT</code>`) {
		t.Errorf("completed tool result is morphable:\n%s", html)
	}
	if detailsTagOpen(html) {
		t.Fatalf("tool defaulted open: %s", html)
	}
}
```

Extend `TestActivityMarksOnlyConversationTextAsMarkdown` so its manager fixtures include stable sequences and completion states, then assert `newActivityView` carries `Complete` through unchanged.

- [ ] **Step 2: Run focused server tests and verify RED**

Run:

```bash
go test ./internal/server -run 'Test(ActivityTemplatePreservesStableInteractiveState|ActivityMarksOnlyConversationTextAsMarkdown|ToolCardTemplateEscapesAndCollapses)$' -count=1
```

Expected: FAIL to compile because `activityItemView.Complete` does not exist; after adding only that field to the test fixture, the template assertions still fail because preservation attributes are absent.

- [ ] **Step 3: Map completion into the server view**

Add this field to `activityItemView`:

```go
// Complete allows the template to leave finalized browser-rendered content
// untouched while the surrounding activity panel continues to morph.
Complete bool
```

Set it in the base `activityItemView` literal inside `newActivityView`:

```go
Complete: a.Complete,
```

No new trusted HTML type or raw payload field is introduced.

- [ ] **Step 4: Add stable IDs and Datastar preservation attributes**

Change the tool wrapper in `activity.html` to:

```html
<details id="activity-item-{{.Seq}}" class="tool-card {{.ToolCardClass}}" data-tool-card data-preserve-attr="open">
```

Change immutable tool arguments to:

```html
<code class="tool-code hljs" data-tool-code data-language="json" data-ignore-morph>{{.ToolArgs}}</code>
```

Change tool results to keep running output morphable and completed output immutable:

```html
<code class="tool-code hljs" data-tool-code data-language="{{.ToolLanguage}}"{{if .Complete}} data-ignore-morph{{end}}>{{.ToolOutput}}</code>
```

Change the non-tool wrapper and body to:

```html
<div id="activity-item-{{.Seq}}" class="t-turn {{if .IsError}}t-error{{end}}">
  <span class="t-role {{.Kind}}">{{.Label}}</span>
  {{if .Text}}<div class="t-body"{{if .IsMarkdown}} data-markdown{{end}}{{if .Complete}} data-ignore-morph{{end}}>{{.Text}}</div>{{end}}
</div>
```

Do not place `data-ignore-morph` on the whole tool card; its status and result must still update while running.

- [ ] **Step 5: Format and verify GREEN**

Run:

```bash
gofmt -w internal/server/view.go internal/server/handler_test.go
go test ./internal/server -count=1
node --test internal/server/web/*.test.js
```

Expected: all tests PASS, including the existing Ctrl-O and Markdown/highlighting tests.

- [ ] **Step 6: Commit Task 2**

```bash
git add internal/server/view.go internal/server/web/activity.html internal/server/handler_test.go
git commit -m "fix(ui): preserve transcript interaction during streaming"
```

---

### Task 3: Validate the integrated behavior

**Files:**
- Verify only: all files changed by Tasks 1 and 2.

**Interfaces:**
- Consumes: the completed manager projection and server template changes.
- Produces: repository-wide test evidence and a clean reviewable branch.

- [ ] **Step 1: Run the complete hermetic suite without cache**

```bash
go test ./... -count=1
node --test internal/server/web/*.test.js
```

Expected: PASS with no failures.

- [ ] **Step 2: Run the repository test target**

```bash
make test
```

Expected: PASS for all Go packages and all browser-unit tests.

- [ ] **Step 3: Run static diff checks**

```bash
git diff --check main...HEAD
git status --short
git diff --stat main...HEAD
git diff main...HEAD -- internal/manager internal/server docs/superpowers
```

Expected: no whitespace errors, a clean worktree, and only the approved spec, plan, manager projection, server view/template, and regression tests changed.

- [ ] **Step 4: Exercise the user flow against a streaming session**

Build and start a branch binary on port 18080 without stopping the user's existing server:

```bash
go build -o /tmp/kanedias-streaming-transcript .
/tmp/kanedias-streaming-transcript --config "$PWD/config.toml" server \
  --listen 127.0.0.1:18080 \
  >/tmp/kanedias-streaming-transcript.log 2>&1 &
validation_pid=$!
trap 'kill "$validation_pid" 2>/dev/null || true' EXIT
```

Open the bootstrap URL printed in `/tmp/kanedias-streaming-transcript.log`, select a live session, then verify:

1. An individually opened completed tool card remains open through later activity patches.
2. A selection in a completed message/tool result remains active through later activity patches.
3. The current assistant message and running tool output continue changing.
4. Ctrl-O still toggles every card and its global mode applies to cards inserted later.

Stop only the validation server with `kill "$validation_pid"`; do not stop or mutate unrelated sessions. Record the port, browser/session used, and observed result in the implementation handoff. If no live session is safely available, report that limitation explicitly and rely on focused template contracts plus the full suite.

- [ ] **Step 5: Confirm commit history**

```bash
git log --oneline main..HEAD
```

Expected commit subjects, newest first:

```text
fix(ui): preserve transcript interaction during streaming
fix(manager): track completed transcript activity
docs: plan stable streaming transcript interaction
docs: design stable streaming transcript interaction
```
