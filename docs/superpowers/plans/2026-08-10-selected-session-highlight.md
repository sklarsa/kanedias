# Selected Session Highlight Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Highlight the root or worker row whose session is currently selected in the main detail view.

**Architecture:** Keep `selectedSessionId` as the browser-side source of truth. Bind each fleet row’s existing `sel` CSS class declaratively through Datastar so selection remains correct after both clicks and server-rendered fleet patches.

**Tech Stack:** Go `html/template`, Datastar attributes, existing Astrolabe CSS, Go `testing`

## Global Constraints

- “Selected” means the session identified by the browser’s `selectedSessionId` signal, not lifecycle `active`.
- Reuse the existing `.row.sel` styling.
- Do not add backend state, JavaScript controllers, CSS changes, APIs, or unrelated refactoring.
- Cover all three row variants: root, parent worker, and leaf worker.

---

## File Structure

- `internal/server/web/fleet.html` — owns the server-rendered root, parent-worker, and leaf-worker row markup and will add the reactive class binding.
- `internal/server/handler_test.go` — owns embedded-template rendering contracts and will verify every row variant carries the binding.

### Task 1: Bind Fleet Rows to the Selected Session Signal

**Files:**
- Modify: `internal/server/handler_test.go:748-806`
- Modify: `internal/server/web/fleet.html:39-103`
- Test: `internal/server/handler_test.go`

**Interfaces:**
- Consumes: browser signal `$selectedSessionId` and each row’s existing `el.dataset.sessionId` value.
- Produces: Datastar attribute `data-class:sel="$selectedSessionId === el.dataset.sessionId"` on every fleet row; when true, Datastar applies the existing `sel` class.

- [ ] **Step 1: Write the failing fleet-template contract test**

Add this focused test after `TestAstrolabeGroupsNestedSubagentsUnderParents` in `internal/server/handler_test.go`:

```go
func TestFleetRowsBindSelectedSessionClass(t *testing.T) {
	templates, err := parseTemplates(webFiles)
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}

	snap := manager.FleetSnapshot{
		Roots: []manager.RootState{
			{
				RootSessionID: "root-1",
				Tree: supervisor.NodeSnapshot{
					SessionID: "root-1",
					Lifecycle: "active",
					Children: []supervisor.NodeSnapshot{
						{
							SessionID: "parent-1",
							WorkerType: "worker",
							Lifecycle: "active",
							Children: []supervisor.NodeSnapshot{
								{SessionID: "leaf-1", WorkerType: "reviewer", Lifecycle: "completed"},
							},
						},
					},
				},
			},
		},
	}
	rendered, err := renderTemplate(templates, templateFleet, newFleetView(snap))
	if err != nil {
		t.Fatalf("render fleet.html: %v", err)
	}

	const binding = `data-class:sel="$selectedSessionId === el.dataset.sessionId"`
	if got := strings.Count(rendered, binding); got != 3 {
		t.Fatalf("selected-session class bindings = %d, want 3 (root, parent worker, leaf worker)\n%s", got, rendered)
	}
}
```

- [ ] **Step 2: Run the focused test and verify RED**

Run:

```bash
go test ./internal/server -run '^TestFleetRowsBindSelectedSessionClass$' -count=1
```

Expected: FAIL with `selected-session class bindings = 0, want 3` because no fleet row currently binds the existing `sel` class.

- [ ] **Step 3: Add the minimal declarative binding to all row variants**

In `internal/server/web/fleet.html`, insert this attribute immediately after `data-lifecycle` on the root row, parent-worker row, and leaf-worker row:

```html
data-class:sel="$selectedSessionId === el.dataset.sessionId"
```

For example, each row’s attributes must follow this pattern without changing its existing click behavior:

```html
<div class="row st-{{.Lifecycle}}"
     data-session-id="{{.RootSessionID}}"
     data-lifecycle="{{.Lifecycle}}"
     data-class:sel="$selectedSessionId === el.dataset.sessionId"
     data-on:click="$selectedSessionId = el.dataset.sessionId; @get('/ui/session', {payload:{selectedSessionId:el.dataset.sessionId}, requestCancellation:'auto'})"
>
```

Use the corresponding existing `{{.SessionID}}` value for both worker row variants; the reactive expression remains identical for all three variants.

- [ ] **Step 4: Run the focused test and verify GREEN**

Run:

```bash
go test ./internal/server -run '^TestFleetRowsBindSelectedSessionClass$' -count=1
```

Expected: PASS.

- [ ] **Step 5: Run formatting and regression verification**

Run:

```bash
gofmt -w internal/server/handler_test.go
git diff --check
make test
```

Expected: `git diff --check` exits successfully; all Go tests pass; all 38 JavaScript tests pass.

- [ ] **Step 6: Commit the implementation**

```bash
git add internal/server/handler_test.go internal/server/web/fleet.html
git commit -m "fix(ui): highlight selected session row"
```
