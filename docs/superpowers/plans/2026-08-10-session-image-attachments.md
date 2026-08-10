# Session-Scoped GUI Image Attachments Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let operators stage multiple per-session image drafts in the web GUI and submit them as bounded native Pi image attachments with idle prompts or streaming steers.

**Architecture:** A small shared Go package owns image formats and byte limits. The browser keeps per-session drafts and uploads them once as multipart data; the server validates every part in bounded memory, and the manager checks the active model before constructing native Pi image RPC blocks. Routed RPC limits increase only on the Pi RPC path, while the browser transcript projects attachment counts without replaying image bytes.

**Tech Stack:** Go 1.26.5, `net/http`, `mime/multipart`, embedded `html/template` assets, vanilla JavaScript UMD modules, Node's built-in test runner, Pi JSONL RPC, Datastar SSE.

## Global Constraints

- Accept only PNG, JPEG, GIF, and WebP images; reject SVG and arbitrary binary data.
- Accept at most 4 images, at most 3 MiB per image, and at most 8 MiB of decoded image data per submission.
- Accept at most 64 KiB of directive text.
- Keep drafts in browser memory only; reloading clears every draft. Do not use local storage, IndexedDB, server temporary files, `/workspace`, or any other filesystem.
- Provide drag-and-drop on the selected session's command composer, an **Attach images** picker, and image clipboard paste.
- Keep text and image drafts isolated by immutable Kanedias session ID and clear only after definitive Pi acceptance.
- Use `Please inspect the attached image(s).` for an image-bearing submission whose directive is empty or whitespace-only.
- Validate selected model capability before sending images; do not switch models or silently omit attachments.
- Keep the existing JSON steer endpoint and unrelated endpoint body limits unchanged.
- Never log image bytes, base64 data, filenames, browser session cookies, or internal data in operator responses.
- Show only an attachment count in transcript SSE output; never send image base64 to the browser transcript.
- Preserve the current no-build embedded-asset architecture and use only hermetic Go/Node tests in `make test`.
- Develop on a feature branch in an isolated worktree based on current `origin/main`; cherry-pick only the approved design and this plan before implementation.

## File Structure

### New files

- `internal/attachments/image.go` — shared image type, limits, signature detection, normalization, and defense-in-depth validation.
- `internal/attachments/image_test.go` — boundary and signature tests for the shared image contract.
- `internal/server/messages.go` — bounded multipart decoder, sanitized JSON response, and GUI message handler.
- `internal/server/messages_test.go` — multipart, status mapping, no-temp-file, authentication, and logging tests.
- `internal/server/web/image-attachments.js` — testable per-session draft controller and multipart submission protocol.
- `internal/server/web/image-attachments.test.js` — pure/controller tests using injected `File`, `FormData`, object URL, and fetch seams.

### Modified files

- `internal/manager/pi.go`, `internal/manager/pi_test.go` — image-aware message dispatch and model capability checks.
- `internal/supervisor/pirpc/client.go`, `internal/supervisor/pirpc/client_test.go` — bounded image-sized Pi JSONL records.
- `internal/supervisorapi/handler.go`, `internal/supervisorapi/handler_test.go` — larger request limit for routed RPC only.
- `internal/supervisorapi/client.go`, `internal/supervisorapi/handler_test.go` — larger response limit for routed RPC only.
- `internal/server/security.go`, `internal/server/security_test.go` — route-specific multipart write boundary.
- `internal/server/server.go`, `internal/server/handler.go`, server test fakes — manager interface, route, and embedded asset wiring.
- `internal/server/web/index.html`, `internal/server/web/app.js`, `internal/server/web/app.css` — composer tray, picker/drop/paste binding, submission, and responsive layout.
- `internal/server/web/terminal-ui.js`, `internal/server/web/terminal-ui.test.js` — submit delegation without optimistic clearing and attachment capability state.
- `internal/manager/types.go`, `internal/manager/projection.go`, `internal/manager/projection_test.go` — attachment count projection while discarding image content.
- `internal/server/view.go`, `internal/server/web/activity.html`, `internal/server/handler_test.go` — safe attachment-count rendering.

---

### Task 1: Shared Image Contract and Native Pi Message Dispatch

**Files:**
- Create: `internal/attachments/image.go`
- Create: `internal/attachments/image_test.go`
- Modify: `internal/manager/pi.go`
- Modify: `internal/manager/pi_test.go`

**Interfaces:**
- Produces: `attachments.Image`, `attachments.NewImage([]byte)`, `attachments.Validate([]Image)`, and exact size/count constants.
- Produces: `manager.ErrImageInputUnsupported` and `(*Manager).SendMessage(context.Context, string, string, []attachments.Image) error`.
- Preserves: `(*Manager).Steer(context.Context, string, string) error` as a no-attachment wrapper.

- [ ] **Step 1: Write failing shared-contract tests**

Create table tests with real magic-byte fixtures and exact boundaries:

```go
func TestNewImageDetectsSupportedSignatures(t *testing.T) {
    tests := []struct {
        name, want string
        data       []byte
    }{
        {"png", "image/png", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)},
        {"jpeg", "image/jpeg", []byte("\xff\xd8\xff\xe0JFIF\x00")},
        {"gif", "image/gif", []byte("GIF89a\x01\x00\x01\x00")},
        {"webp", "image/webp", []byte("RIFF\x04\x00\x00\x00WEBPVP8 ")},
    }
    for _, test := range tests {
        t.Run(test.name, func(t *testing.T) {
            got, err := NewImage(test.data)
            if err != nil { t.Fatal(err) }
            if got.MIMEType != test.want { t.Fatalf("MIMEType = %q, want %q", got.MIMEType, test.want) }
        })
    }
}
```

Add tests proving empty data, SVG, text, 4/5 images, `3 MiB`/`3 MiB+1`, and `8 MiB`/`8 MiB+1` behave exactly as the global constraints require. Mutate the caller's input after `NewImage` and assert the stored bytes do not change.

- [ ] **Step 2: Run the shared-contract tests and verify red**

Run:

```bash
go test ./internal/attachments -run 'Test(NewImage|Validate)' -count=1
```

Expected: FAIL because `internal/attachments` and its exported contract do not exist.

- [ ] **Step 3: Implement the shared image package**

Use these exact public names and sentinel errors:

```go
package attachments

const (
    MaxImages     = 4
    MaxImageBytes = 3 << 20
    MaxTotalBytes = 8 << 20
    MaxMessageBytes = 64 << 10
)

var (
    ErrEmptyImage        = errors.New("image is empty")
    ErrUnsupportedFormat = errors.New("image format is unsupported")
    ErrTooManyImages     = errors.New("too many images")
    ErrImageTooLarge     = errors.New("image exceeds per-image limit")
    ErrImagesTooLarge    = errors.New("images exceed aggregate limit")
)

type Image struct {
    MIMEType string
    Data     []byte
}

func NewImage(data []byte) (Image, error)
func Validate(images []Image) error
```

`NewImage` must detect actual bytes with `http.DetectContentType`, normalize `image/jpg` to `image/jpeg`, and detect WebP explicitly when bytes 0–3 are `RIFF` and bytes 8–11 are `WEBP`. It copies the bytes and calls the same size checks used by `Validate`. `Validate` must re-detect each image and reject a MIME/signature mismatch so non-server callers cannot bypass the trust boundary.

- [ ] **Step 4: Run shared-contract tests and verify green**

Run:

```bash
go test ./internal/attachments -count=1
```

Expected: PASS.

- [ ] **Step 5: Write failing manager dispatch tests**

Add tests that decode the exact second RPC payload instead of comparing strings:

```go
func TestSendMessageIdleIncludesNativeImages(t *testing.T) {
    image, err := attachments.NewImage(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...))
    if err != nil { t.Fatal(err) }
    client := imageCapablePIControlClient(false)
    m := piManagerWithSession("root", client, rootTree("root"))

    if err := m.SendMessage(context.Background(), "root", "inspect", []attachments.Image{image}); err != nil {
        t.Fatal(err)
    }
    var command struct {
        Type, Message string
        Images []struct { Type, Data, MIMEType string }
    }
    if err := json.Unmarshal(client.callLog[1].payload, &command); err != nil { t.Fatal(err) }
    if command.Type != "prompt" || command.Message != "inspect" { t.Fatalf("command = %#v", command) }
    if len(command.Images) != 1 || command.Images[0].Type != "image" || command.Images[0].MIMEType != "image/png" {
        t.Fatalf("images = %#v", command.Images)
    }
    if got := command.Images[0].Data; got != base64.StdEncoding.EncodeToString(image.Data) { t.Fatalf("data mismatch") }
}
```

Cover idle `prompt`, streaming `steer`, multiple image order, absence of the `images` field for `Steer`, nil model, model input `['text']`, model input `['text','image']`, validation before the second RPC, and exact propagation of Pi rejection.

- [ ] **Step 6: Run manager tests and verify red**

Run:

```bash
go test ./internal/manager -run 'Test(SendMessage|Steer)' -count=1
```

Expected: FAIL because `SendMessage`, the capability-bearing state shape, and `ErrImageInputUnsupported` do not exist.

- [ ] **Step 7: Implement image-aware manager dispatch**

Extend state decoding without exposing provider/model identifiers:

```go
type piStateData struct {
    IsStreaming bool `json:"isStreaming"`
    SessionID   string `json:"sessionId,omitempty"`
    Model       *struct {
        Input []string `json:"input"`
    } `json:"model,omitempty"`
}

var ErrImageInputUnsupported = errors.New("manager: selected model does not support image input")

func (m *Manager) Steer(ctx context.Context, sessionID, message string) error {
    return m.SendMessage(ctx, sessionID, message, nil)
}

func (m *Manager) SendMessage(ctx context.Context, sessionID, message string, images []attachments.Image) error
```

`SendMessage` must call `attachments.Validate` before network I/O, call `get_state`, require an `image` input capability only when images are non-empty, preserve image order, base64 encode with `base64.StdEncoding`, omit `images` when empty, select `prompt`/`steer` from `isStreaming`, and validate the corresponding Pi response command.

- [ ] **Step 8: Run manager and shared tests**

Run:

```bash
go test ./internal/attachments ./internal/manager -count=1
```

Expected: PASS.

- [ ] **Step 9: Commit the shared contract and manager dispatch**

```bash
git add internal/attachments/image.go internal/attachments/image_test.go internal/manager/pi.go internal/manager/pi_test.go
git commit -m "feat(manager): send native image attachments"
```

---

### Task 2: Bounded Routed RPC Transport

**Files:**
- Modify: `internal/supervisor/pirpc/client.go`
- Modify: `internal/supervisor/pirpc/client_test.go`
- Modify: `internal/supervisorapi/handler.go`
- Modify: `internal/supervisorapi/handler_test.go`
- Modify: `internal/supervisorapi/client.go`

**Interfaces:**
- Consumes: `pirpc.MaxRecordBytes` as the authoritative routed image RPC ceiling.
- Produces: `supervisorapi.MaxRPCRequestBodyBytes` equal to `pirpc.MaxRecordBytes`.
- Preserves: `supervisorapi.MaxRequestBodyBytes == 1 << 20` for non-RPC JSON bodies and the 1 MiB generic descendant-response cap.

- [ ] **Step 1: Write failing request/response boundary tests**

Add a handler test proving `/rpc` accepts exactly the enlarged limit while question/child JSON stays at 1 MiB:

```go
func TestRPCBodyUsesImageAwareLimit(t *testing.T) {
    payload := `{"type":"prompt","message":"` + strings.Repeat("x", MaxRequestBodyBytes) + `"}`
    response := jsonRequest(t, NewHandler(&fakeService{}), http.MethodPost, "/v1/sessions/self/rpc", payload)
    if response.Code != http.StatusOK { t.Fatalf("status = %d; body = %s", response.Code, response.Body.String()) }
}
```

Add an exact `MaxRPCRequestBodyBytes+1` rejection test and a `DescendantClient.CallRPC` test whose fake Unix HTTP server returns JSON above 1 MiB but below the Pi record limit. Assert a normal `Snapshot` response above 1 MiB still fails.

- [ ] **Step 2: Run supervisor transport tests and verify red**

Run:

```bash
go test ./internal/supervisor/pirpc ./internal/supervisorapi -run 'Test.*(Record|RPCBody|Response|Oversize)' -count=1
```

Expected: FAIL because routed requests/responses still use 1 MiB and Pi records still use 4 MiB.

- [ ] **Step 3: Implement narrow transport ceilings**

Set:

```go
// 8 MiB raw images expand to about 10.7 MiB; 12 MiB leaves JSON/text headroom
// while remaining below the default 16 MiB event-byte budget.
const MaxRecordBytes = 12 << 20
```

In `supervisorapi/handler.go`, retain `MaxRequestBodyBytes` and add:

```go
const MaxRPCRequestBodyBytes = pirpc.MaxRecordBytes

func readJSONBodyLimit(w http.ResponseWriter, request *http.Request, limit int64, overflowMessage string) (json.RawMessage, error)
```

Make `readRPCBody` use `MaxRPCRequestBodyBytes`; leave `readJSONBody` as the 1 MiB wrapper used by children, handoff, and questions.

In `supervisorapi/client.go`, change the decoder to accept an explicit maximum:

```go
func readDescendantJSON(response *http.Response, target any, maxBytes int64) error
```

Use `pirpc.MaxRecordBytes` only in `CallRPC`; use `maxDescendantResponseBytes` everywhere else. Keep `maxDescendantSSELineBytes = pirpc.MaxRecordBytes + (1 << 20)`.

- [ ] **Step 4: Run transport tests and verify green**

Run:

```bash
go test ./internal/supervisor/pirpc ./internal/supervisorapi -count=1
```

Expected: PASS, including existing one-byte-over-limit failures.

- [ ] **Step 5: Commit routed transport changes**

```bash
git add internal/supervisor/pirpc/client.go internal/supervisor/pirpc/client_test.go internal/supervisorapi/handler.go internal/supervisorapi/handler_test.go internal/supervisorapi/client.go
git commit -m "feat(supervisor): route bounded image RPC payloads"
```

---

### Task 3: Multipart Message Endpoint and Route-Specific Security

**Files:**
- Create: `internal/server/messages.go`
- Create: `internal/server/messages_test.go`
- Modify: `internal/server/security.go`
- Modify: `internal/server/security_test.go`
- Modify: `internal/server/server.go`
- Modify: `internal/server/handler.go`
- Modify: `internal/server/server_test.go`
- Modify: `internal/server/handler_test.go`
- Modify: `internal/server/actions_test.go`

**Interfaces:**
- Consumes: `attachments.Image`, all attachment limits, and `fleetManager.SendMessage(ctx, sessionID, message, images)`.
- Produces: authenticated `POST /ui/sessions/{sessionID}/messages` with multipart request and strict JSON response.
- Preserves: existing `/steer` JSON/Datastar behavior by keeping `fleetManager.Steer`.

- [ ] **Step 1: Write failing multipart decoder tests**

Create a helper that builds multipart bodies without `ParseMultipartForm`:

```go
func multipartMessage(t *testing.T, message string, images ...multipartFixture) (string, []byte) {
    t.Helper()
    var body bytes.Buffer
    writer := multipart.NewWriter(&body)
    if err := writer.WriteField("message", message); err != nil { t.Fatal(err) }
    for _, image := range images {
        part, err := writer.CreateFormFile("image", image.name)
        if err != nil { t.Fatal(err) }
        if _, err := part.Write(image.data); err != nil { t.Fatal(err) }
    }
    if err := writer.Close(); err != nil { t.Fatal(err) }
    return writer.FormDataContentType(), body.Bytes()
}
```

Test valid signatures, 0 and 4 images, 5 images, `3 MiB+1`, aggregate `8 MiB+1`, directive `64 KiB+1`, missing/duplicate message, unknown fields, non-file image parts, malformed boundaries, truncated bodies, SVG/text disguised as PNG, and blank directive with images becoming the exact neutral prompt.

- [ ] **Step 2: Write failing endpoint/security tests**

Use a recording fleet fake with:

```go
type sentMessage struct {
    sessionID string
    message   string
    images    []attachments.Image
}

func (f *streamFakeFleet) SendMessage(_ context.Context, sessionID, message string, images []attachments.Image) error {
    f.sentMessage = sentMessage{sessionID: sessionID, message: message, images: cloneImages(images)}
    return f.messageErr
}
```

Assert:

- valid upload returns HTTP 202 and exactly `{"accepted":true}`;
- malformed multipart returns 400 with `{"accepted":false,"error":"The message upload was not valid."}`;
- count/size overflow returns 413 with `{"accepted":false,"error":"The image attachment limits were exceeded."}`;
- unsupported signatures return 415 with `{"accepted":false,"error":"Only PNG, JPEG, GIF, and WebP images are supported."}`;
- `manager.ErrImageInputUnsupported` returns 409 without leaking internal text;
- other manager failures return 503, are logged, and do not log multipart bytes or filename;
- unauthenticated and cross-origin multipart requests fail;
- multipart is accepted only on `/messages` and JSON remains required on existing write routes;
- a `TMPDIR` pointed at an empty test directory remains empty after valid and oversized requests.

- [ ] **Step 3: Run server tests and verify red**

Run:

```bash
go test ./internal/server -run 'Test(Message|Multipart|WriteBoundary)' -count=1
```

Expected: FAIL because the decoder, route, interface method, and multipart boundary do not exist.

- [ ] **Step 4: Implement strict streaming multipart decoding**

Create these focused types/functions in `messages.go`:

```go
const (
    neutralImageMessage = "Please inspect the attached image(s)."
    maxMessageMultipartBytes = attachments.MaxTotalBytes + attachments.MaxMessageBytes + (128 << 10)
)

type messageRequest struct {
    Message string
    Images  []attachments.Image
}

type messageDecodeError struct {
    Status  int
    Message string
    Err     error
}

func decodeMessageRequest(w http.ResponseWriter, r *http.Request) (messageRequest, error)
func makeMessageHandler(fleet fleetManager, logger *slog.Logger) http.HandlerFunc
```

Wrap the whole body with `http.MaxBytesReader`, call `r.MultipartReader()`, iterate `NextPart`, read each part through `io.LimitReader(limit+1)`, and reject before `fleet.SendMessage` unless the entire request is valid. Do not call `ParseMultipartForm`. Close each part promptly. Call `attachments.NewImage` only after per-file/aggregate checks. Apply the neutral prompt server-side when images are present and `strings.TrimSpace(message) == ""`.

Return only these response shapes:

```json
{"accepted":true}
{"accepted":false,"error":"sanitized operator message"}
```

- [ ] **Step 5: Implement route-specific multipart write security**

Refactor without weakening the existing helper:

```go
func (b requestBoundary) requireWriteBoundary(next http.Handler) http.Handler {
    return b.requireWriteBoundaryFor("application/json")(next)
}

func (b requestBoundary) requireWriteBoundaryFor(allowed ...string) func(http.Handler) http.Handler
```

Keep Host, Origin, and `Sec-Fetch-Site` checks identical. Parse and compare the base media type so a valid multipart boundary parameter is accepted only when `multipart/form-data` is explicitly allowed.

Register `/messages` in its own protected write group with `sessionRequired` and `requireWriteBoundaryFor("multipart/form-data")`. In trusted-network mode the handler's `MultipartReader` remains the media-type validator.

- [ ] **Step 6: Extend the fleet interface and all server fakes**

Add:

```go
SendMessage(ctx context.Context, sessionID, message string, images []attachments.Image) error
```

Update concrete/fake implementations in `server_test.go`, `handler_test.go`, and `actions_test.go`. Do not replace `Steer`; the existing JSON handler continues to call it.

- [ ] **Step 7: Run targeted and complete server tests**

Run:

```bash
go test ./internal/server -count=1
```

Expected: PASS with no temporary files in the dedicated test directory.

- [ ] **Step 8: Commit the server boundary**

```bash
git add internal/server/messages.go internal/server/messages_test.go internal/server/security.go internal/server/security_test.go internal/server/server.go internal/server/handler.go internal/server/server_test.go internal/server/handler_test.go internal/server/actions_test.go
git commit -m "feat(server): accept bounded image messages"
```

---

### Task 4: Per-Session Browser Draft Controller

**Files:**
- Create: `internal/server/web/image-attachments.js`
- Create: `internal/server/web/image-attachments.test.js`
- Modify: `internal/server/handler.go`
- Modify: `internal/server/handler_test.go`

**Interfaces:**
- Produces global/CommonJS `KanediasImageAttachments`.
- Produces `createController(options)` with `selectSession`, `setText`, `stageFiles`, `removeImage`, `reconcileSessions`, `submit`, `draft`, and `destroy` methods.
- Consumes: injected `fetch`, `FormData`, `URL.createObjectURL`, `URL.revokeObjectURL`, and observer callbacks; it does not depend on a browser DOM.

- [ ] **Step 1: Write failing controller tests**

Use lightweight fake files:

```js
const imageFile = (name, type, size, lastModified = 1) => ({name, type, size, lastModified});

const options = () => ({
  fetch: async () => ({status: 202, headers: {get: () => "application/json"}, json: async () => ({accepted: true})}),
  FormData: FakeFormData,
  createObjectURL: file => "blob:" + file.name,
  revokeObjectURL: url => revoked.push(url),
  onChange: snapshot => changes.push(snapshot),
  onStatus: status => statuses.push(status)
});
```

Test:

- staging and submission reject safely when no session is selected;
- independent text/images for sessions A and B;
- restoring A after selecting B;
- definitely unsupported non-empty MIME rejection while an empty MIME may stage;
- 4/5 files, `3 MiB`/`3 MiB+1`, and `8 MiB`/`8 MiB+1`;
- removal and URL revocation;
- deterministic pasted-image fallback labels;
- same file selectable after a picker reset at the binding layer;
- submission URL percent-escapes the session ID;
- multipart field order is message then images in staged order;
- empty text with images uses the neutral prompt;
- no-images empty text is rejected client-side;
- in-flight draft is locked while another session remains editable;
- 202 strict acceptance clears only the captured draft;
- 4xx/5xx strict rejection preserves the draft;
- malformed response and thrown fetch preserve the draft and report unknown delivery;
- reconciliation revokes and deletes drafts for removed sessions;
- destroy revokes every remaining URL.

- [ ] **Step 2: Run Node tests and verify red**

Run:

```bash
node --test internal/server/web/image-attachments.test.js
```

Expected: FAIL because the module does not exist.

- [ ] **Step 3: Implement the UMD draft controller**

Export this exact shape:

```js
(function (root, factory) {
  "use strict";
  var api = factory();
  if (typeof module === "object" && module.exports) module.exports = api;
  if (root) root.KanediasImageAttachments = api;
})(typeof globalThis !== "undefined" ? globalThis : this, function () {
  "use strict";
  var LIMITS = {maxImages: 4, maxImageBytes: 3 * 1024 * 1024, maxTotalBytes: 8 * 1024 * 1024};
  var NEUTRAL_MESSAGE = "Please inspect the attached image(s).";

  function createController(options) {
    var state = {drafts: new Map(), selectedSessionID: "", nextAttachmentID: 1};
    return {
      selectSession: function (sessionID) { state.selectedSessionID = sessionID; return snapshot(state, sessionID); },
      setText: function (text) { setDraftText(state, text, options); },
      stageFiles: function (files) { return stageDraftFiles(state, files, options); },
      removeImage: function (id) { return removeDraftImage(state, id, options); },
      reconcileSessions: function (sessionIDs) { reconcileDrafts(state, sessionIDs, options); },
      submit: function (sessionID) { return submitDraft(state, sessionID, options); },
      draft: function (sessionID) { return snapshot(state, sessionID); },
      destroy: function () { destroyDrafts(state, options); }
    };
  }

  return {LIMITS: LIMITS, NEUTRAL_MESSAGE: NEUTRAL_MESSAGE, createController: createController};
});
```

Implement these module-private helpers with the exact signatures used above:

```js
function snapshot(state, sessionID)
function setDraftText(state, text, options)
function stageDraftFiles(state, files, options)
function removeDraftImage(state, attachmentID, options)
function reconcileDrafts(state, sessionIDs, options)
function submitDraft(state, sessionID, options)
function destroyDrafts(state, options)
```

Use plain arrays/maps and immutable snapshots for callbacks. `stageDraftFiles` increments `state.nextAttachmentID` only for accepted files. Never place a `File`, object URL, filename, or response text into HTML. A definitive response requires `Content-Type: application/json` and exactly one of the two documented object shapes. Treat a fetch exception or malformed response as unknown delivery; treat a valid `accepted:false` response as definitive rejection.

- [ ] **Step 4: Serve the dormant embedded module**

Add `/assets/image-attachments.js` with `text/javascript; charset=utf-8`. Extend asset tests to assert its body contains `KanediasImageAttachments`, it requires no session cookie, and it carries no external URL.

- [ ] **Step 5: Run browser-module and handler asset tests**

Run:

```bash
node --test internal/server/web/image-attachments.test.js
go test ./internal/server -run 'Test.*Asset' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the draft controller**

```bash
git add internal/server/web/image-attachments.js internal/server/web/image-attachments.test.js internal/server/handler.go internal/server/handler_test.go
git commit -m "feat(ui): add per-session image draft controller"
```

---

### Task 5: Composer UI, Drop/Picker/Paste Binding, and Submission

**Files:**
- Modify: `internal/server/web/index.html`
- Modify: `internal/server/web/app.js`
- Modify: `internal/server/web/app.css`
- Modify: `internal/server/web/terminal-ui.js`
- Modify: `internal/server/web/terminal-ui.test.js`
- Modify: `internal/server/handler_test.go`

**Interfaces:**
- Consumes: `window.KanediasImageAttachments.createController` from Task 4.
- Consumes: `terminalUI.performAction(action, {submit})` for Enter.
- Produces: accessible `#image-attachment-tray`, `#attach-images-button`, and `#image-file-input` bindings.

- [ ] **Step 1: Write failing terminal submit tests**

Replace optimistic-clear expectations with delegated submission:

```js
test("Enter delegates submission without clearing before acceptance", () => {
  const fixture = fakeSubmitDocument(false);
  fixture.input.value = "inspect this";
  let submits = 0;
  ui.performAction("submit", {
    event: {preventDefault() {}},
    document: fixture.document,
    submit() { submits++; }
  });
  assert.equal(submits, 1);
  assert.equal(fixture.input.value, "inspect this");
});
```

Add capability tests proving the attach button and file input follow `data-can-steer`, and that Ctrl-C still dispatches an input event so draft text updates.

- [ ] **Step 2: Write failing page-structure tests**

Extend the index tests to require:

```html
<div id="image-attachment-tray" aria-live="polite" hidden></div>
<input id="image-file-input" type="file" accept="image/png,image/jpeg,image/gif,image/webp" multiple hidden>
<button id="attach-images-button" type="button">…</button>
```

Require `/assets/image-attachments.js` before `/assets/app.js`, no `data-bind="commandMessage"` on the directive input, no Datastar click action on `#steerBtn`, and no inline event handlers.

- [ ] **Step 3: Run focused UI tests and verify red**

Run:

```bash
node --test internal/server/web/terminal-ui.test.js
go test ./internal/server -run 'Test.*(Page|Index|Script|SessionPanel)' -count=1
```

Expected: FAIL on optimistic clear, missing attachment controls, and missing page script.

- [ ] **Step 4: Refactor terminal submission and capability synchronization**

Implement:

```js
if (action === "submit") {
  if (typeof context.submit === "function") context.submit();
  else {
    var steer = documentObject.getElementById("steerBtn");
    if (steer && !steer.disabled) steer.click();
  }
  return true;
}
```

Do not clear the deck in `performAction`. Extend `syncDeckState` so `#attach-images-button` and `#image-file-input` are enabled only when steer is enabled. Preserve select-all, clear, interrupt, and tool-toggle behavior.

- [ ] **Step 5: Add semantic composer markup**

Restructure the footer into a preview row and one controls row. Keep all existing button IDs used by terminal controls. The Steer button becomes `type="button"` and loses its Datastar POST attribute because the controller owns all browser message sends. Add the attachment script before `app.js`.

Use only text nodes and data attributes for preview content; do not introduce an image filename into template HTML.

- [ ] **Step 6: Bind drafts and inputs in `app.js`**

Instantiate the controller with browser globals and callbacks. Bind:

- row click: save current input, select `row.dataset.sessionId`, restore its text;
- input: call `controller.setText(input.value)`;
- attach button/file input: open picker, stage files, then reset `fileInput.value = ""`;
- focused input paste: if clipboard contains image files, prevent default and stage them; otherwise return without preventing;
- composer drag enter/over/leave/drop: prevent only file drops, maintain drag depth, and stage dropped files;
- preview remove buttons: call `controller.removeImage(id)`;
- Steer click and Enter submit callback: call `controller.submit(capturedSessionID)`;
- fleet mutations: reconcile controller session IDs with current `.row[data-session-id]` elements;
- `beforeunload`: call `controller.destroy()`.

Render thumbnails with `document.createElement("img")`, assign `src` from the object URL, use empty alt text, and add adjacent filename/size text. Set deck status with `textContent`, never `innerHTML`. Disable the selected draft's input/attach/remove/submit controls while it is busy; other session drafts remain usable.

- [ ] **Step 7: Add responsive attachment styling**

Use a two-row deck and toggle a class on `.app` when the selected draft has images:

```css
.app.has-image-draft{ --deck-h:132px; }
.deck{ flex-direction:column; align-items:stretch; }
.deck-row{ display:flex; align-items:center; gap:10px; min-width:0; }
.image-attachment-tray{ display:flex; gap:8px; overflow-x:auto; }
.image-attachment-card img{ width:44px; height:44px; object-fit:cover; }
.deck.drop-active{ box-shadow:inset 0 0 0 2px var(--cyan); }
```

Add visible focus styles, a remove-button hit target, truncated filename text, disabled/busy styles, and mobile rules that preserve horizontal control access without covering the transcript or sidebar.

- [ ] **Step 8: Run all browser and template tests**

Run:

```bash
node --test internal/server/web/*.test.js
go test ./internal/server -count=1
```

Expected: PASS.

- [ ] **Step 9: Commit the wired composer UI**

```bash
git add internal/server/web/index.html internal/server/web/app.js internal/server/web/app.css internal/server/web/terminal-ui.js internal/server/web/terminal-ui.test.js internal/server/handler_test.go
git commit -m "feat(ui): attach images from the session composer"
```

---

### Task 6: Transcript Attachment Count Projection

**Files:**
- Modify: `internal/manager/types.go`
- Modify: `internal/manager/projection.go`
- Modify: `internal/manager/projection_test.go`
- Modify: `internal/server/view.go`
- Modify: `internal/server/web/activity.html`
- Modify: `internal/server/web/app.css`
- Modify: `internal/server/handler_test.go`

**Interfaces:**
- Produces: `ActivityItem.ImageCount int` for user `message_end` events.
- Produces: precomputed `activityItemView.AttachmentLabel string` such as `1 image attached` or `2 images attached`.
- Guarantees: image `data` and MIME payloads never enter `activityItemView` or rendered activity HTML.

- [ ] **Step 1: Write failing projection tests**

Add a user message event with text and two base64 image blocks:

```go
projector.Apply(piEvent(7, "s", "message_end", map[string]any{
    "message": map[string]any{
        "role": "user",
        "content": []any{
            map[string]any{"type": "text", "text": "inspect"},
            map[string]any{"type": "image", "mimeType": "image/png", "data": "SECRET_BASE64_A"},
            map[string]any{"type": "image", "mimeType": "image/jpeg", "data": "SECRET_BASE64_B"},
        },
    },
}))
```

Assert one `user_message` item with `Text == "inspect"`, `ImageCount == 2`, and no secret strings in any projected field. Add an image-only user message and assert it still produces an activity item.

- [ ] **Step 2: Write failing template tests**

Construct an activity item with `ImageCount: 1` and another with `ImageCount: 2`. Assert exact singular/plural labels and assert rendered HTML does not contain supplied base64 secrets or a `data:image` URL.

- [ ] **Step 3: Run projection/template tests and verify red**

Run:

```bash
go test ./internal/manager ./internal/server -run 'Test.*(Image|Attachment|Activity)' -count=1
```

Expected: FAIL because image counts are discarded and image-only messages are omitted.

- [ ] **Step 4: Project counts while discarding image fields**

Extend only the typed user-message projection:

```go
type messageEndPayload struct {
    Message *struct {
        Role string `json:"role"`
        Content []struct {
            Type string `json:"type"`
            Text string `json:"text"`
        } `json:"content"`
    } `json:"message"`
}
```

Count `Type == "image"`; never add `Data` or `MIMEType` fields to this struct. Append a user activity item when text is non-empty **or** image count is positive.

- [ ] **Step 5: Render precomputed safe labels**

Add:

```go
func imageAttachmentLabel(count int) string {
    if count == 1 { return "1 image attached" }
    if count > 1 { return fmt.Sprintf("%d images attached", count) }
    return ""
}
```

Map it into `activityItemView.AttachmentLabel` and render a plain escaped `<div class="t-attachments">` below the user text. Style it as compact muted metadata without an `<img>` element.

- [ ] **Step 6: Run projection, server, and full package tests**

Run:

```bash
go test ./internal/manager ./internal/server -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit transcript projection**

```bash
git add internal/manager/types.go internal/manager/projection.go internal/manager/projection_test.go internal/server/view.go internal/server/web/activity.html internal/server/web/app.css internal/server/handler_test.go
git commit -m "feat(ui): show sent image attachment counts"
```

---

### Task 7: Full Verification, Adversarial Review, PR, and Merge

**Files:**
- Modify only files required by verified review findings.

**Interfaces:**
- Consumes: all completed tasks and the approved design.
- Produces: a reviewed pull request whose required checks are green and which is merged into `main`.

- [ ] **Step 1: Run formatters**

```bash
gofmt -w internal/attachments/*.go internal/manager/*.go internal/server/*.go internal/supervisor/pirpc/*.go internal/supervisorapi/*.go
npm --version >/dev/null
```

Inspect `git diff --check`; expected: no whitespace errors.

- [ ] **Step 2: Run the hermetic suite**

```bash
make test
```

Expected: all `go test ./...` and `node --test internal/server/web/*.test.js` tests pass.

- [ ] **Step 3: Run build and lint**

```bash
make build
make lint
```

Expected: binary builds, `gofmt` check passes, and `golangci-lint` reports no findings.

- [ ] **Step 4: Exercise race-sensitive Go packages**

```bash
go test -race ./internal/manager ./internal/server ./internal/supervisorapi ./internal/supervisor/pirpc -count=1
```

Expected: PASS without race reports.

- [ ] **Step 5: Perform manual browser acceptance when an image-capable profile is available**

Start the configured server in the isolated worktree, then execute all nine manual acceptance steps in `docs/superpowers/specs/2026-08-10-session-image-attachments-design.md`. If the local configuration exposes only a text-only model, perform the rejection/preservation flow and record the image-capable live path as an explicit residual verification item rather than claiming it ran.

- [ ] **Step 6: Request independent adversarial reviews**

Dispatch fresh-context read-only reviewers in parallel with these exact scopes:

```text
Security reviewer: attack multipart parsing, content-type/origin boundaries, memory limits, logging, temp-file claims, MIME confusion, base64/JSONL amplification, and descendant routing. Report only actionable findings with file:line evidence.

Correctness reviewer: attack per-session draft isolation, selection races, in-flight mutation, unknown-delivery behavior, object URL lifecycle, idle/streaming dispatch, model capability checks, image-only messages, and transcript projection. Report only actionable findings with file:line evidence.

Test reviewer: compare every approved spec requirement to tests, identify false-positive tests and missing one-byte boundary cases, and report exact additions needed.
```

- [ ] **Step 7: Triage and fix every validated review finding with TDD**

For each validated finding, first add the smallest failing regression test, run it to observe failure, implement the fix, and rerun the focused test. Reject findings only with concrete code/test evidence. Commit review fixes separately:

After inspecting `git status --short` to verify every changed path belongs to a validated finding, stage the complete isolated-worktree fix set:

```bash
git add -A
git diff --cached --check
git commit -m "fix: address image attachment review findings"
```

If no finding is validated, do not create an empty commit.

- [ ] **Step 8: Re-run final verification from a clean index**

```bash
git diff --check
git status --short
make test
make build
make lint
go test -race ./internal/manager ./internal/server ./internal/supervisorapi ./internal/supervisor/pirpc -count=1
```

Expected: only intentional untracked runtime artifacts are absent, all commands pass, and the worktree is clean after committing.

- [ ] **Step 9: Push and create the pull request**

Write the reviewed facts to the PR body, using the second manual-acceptance line only when no image-capable profile was available:

```bash
cat > /tmp/kanedias-session-image-attachments-pr.md <<'EOF'
## Summary
- stage independent image drafts for selected supervised sessions via drop, picker, or clipboard
- validate bounded multipart images and send native Pi prompt/steer image blocks only to image-capable models
- retain safe attachment counts in the transcript without replaying base64 to the browser

## Limits
- PNG, JPEG, GIF, WebP
- 4 images maximum; 3 MiB per image; 8 MiB combined; 64 KiB directive
- 12 MiB routed Pi RPC record ceiling; unrelated HTTP limits unchanged

## Verification
- `make test`
- `make build`
- `make lint`
- `go test -race ./internal/manager ./internal/server ./internal/supervisorapi ./internal/supervisor/pirpc -count=1`

## Manual acceptance
- Text-only model rejection and draft preservation verified.
- Image-capable live submission was not run because no authorized image-capable profile was available; automated native RPC payload tests passed.

## Design
- `docs/superpowers/specs/2026-08-10-session-image-attachments-design.md`
- `docs/superpowers/plans/2026-08-10-session-image-attachments.md`
EOF

git push -u origin feat/session-image-attachments
gh pr create --base main --head feat/session-image-attachments \
  --title "feat(ui): attach images to supervised sessions" \
  --body-file /tmp/kanedias-session-image-attachments-pr.md
```

If the image-capable live path ran successfully, replace the two manual-acceptance lines before `gh pr create` with the exact line `- Image-capable idle prompt, streaming steer, and image-only submission verified live.` Do not claim any command or manual path that did not run successfully.

- [ ] **Step 10: Wait for and inspect required checks**

```bash
gh pr checks --watch
```

Expected: all required checks pass. If a check fails, inspect its logs, reproduce locally, add a failing regression test when applicable, fix, rerun the complete verification, commit, and push before watching checks again.

- [ ] **Step 11: Verify mergeability and squash-merge**

```bash
gh pr view --json number,state,mergeable,reviewDecision,statusCheckRollup,url
gh pr merge --squash --delete-branch
```

Run the merge command only when the PR is open, mergeable, and every required check is successful. Confirm the resulting PR state is `MERGED` and report the PR URL and squash commit.
