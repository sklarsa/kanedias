# Session-Scoped GUI Image Attachments — Design

Date: 2026-08-10

## Goal

Allow an operator to drag images into the Kanedias web command composer for a
selected session, review them as a session-scoped draft, and send them with the
next directive as native Pi image attachments.

The same staging flow also supports a file-picker button and clipboard image
paste. Multiple sessions may hold independent in-memory drafts while the
operator moves through the fleet.

## Scope

### In scope

- Drag-and-drop onto the command composer for the currently selected session.
- An **Attach images** file picker and image-only clipboard paste while the
  command input is focused.
- Multiple removable image previews.
- Per-session in-memory image and directive drafts.
- Native Pi image attachments for both idle prompts and streaming steers.
- Image-only submission with a neutral generated directive.
- Safe multipart upload, image validation, model capability validation, and
  bounded RPC transport changes.
- Compact sent-attachment counts in the transcript without sending image bytes
  to the browser through transcript SSE patches.
- Automated JavaScript and Go tests plus manual acceptance coverage.

### Out of scope

- Persisting drafts across page reloads or server restarts.
- Uploading images into `/workspace` or any host/session filesystem.
- Server-side temporary attachment storage or reusable attachment IDs.
- Dragging directly onto fleet rows or the transcript/detail pane.
- Non-image files, remote image URLs, SVG, PDF, video, or arbitrary binary
  attachments.
- Displaying sent image thumbnails in the transcript.
- Image editing, annotation, cropping, or client-side format conversion.
- Automatic model switching when the selected model lacks image input support.
- Exactly-once delivery across an ambiguous browser/network disconnect.

## Chosen Approach

Use a single bounded `multipart/form-data` request for each command submission.
The browser retains the original `File` objects and preview object URLs only in
memory. The server reads raw file parts into bounded memory, verifies them, and
constructs Pi's native base64 image blocks immediately before routed RPC.

This is preferable to base64 JSON from the browser because it avoids the 33%
base64 expansion and extra browser-side copies on the HTTP hop. It is preferable
to a two-step upload because no temporary storage, attachment ownership table,
expiration policy, or cleanup process is needed.

## User Experience

### Drop target and alternate inputs

The command composer is the only drop target. Its active drag state must be
visually obvious without covering the directive text. Dropping is accepted only
when a live session is selected.

An **Attach images** button opens a hidden multi-file input. Pasting while the
command input is focused stages supported clipboard image items; a paste with no
image items remains entirely browser-native so normal text paste is unaffected.
All three paths call the same attachment-staging logic.

### Draft previews

Accepted files appear in a preview strip above the command input. Each preview
contains:

- a thumbnail;
- the filename as untrusted text, or `Pasted image N` when none exists;
- a human-readable size; and
- a clearly labelled remove button.

The preview strip is keyboard reachable and usable at mobile widths. Validation
feedback is exposed through an `aria-live` status region. Removing an attachment
revokes its object URL immediately.

### Per-session drafts

A browser-memory map keyed by immutable Kanedias session ID owns each draft's
text and attachments. Selecting another session saves the current directive and
renders that session's independent draft. Returning to a session restores its
draft.

A submission snapshots its target session ID before network activity begins.
Changing the selected session while the request is in flight cannot retarget the
request or clear another session's draft. When a session disappears from the
fleet, its draft is discarded and all object URLs are revoked.

Reloading or closing the page clears all drafts. No draft bytes are written to
local storage, IndexedDB, the server, or a filesystem.

### Submission behavior

Enter and the **Steer** button invoke the same controller path. The controller
submits the directive plus zero or more staged images to the session ID captured
at submission time. If images are present and the directive is empty or
whitespace-only, the controller supplies:

> Please inspect the attached image(s).

While a draft is being submitted, only that draft's submit controls are busy.
On definitive Pi acceptance, the controller clears its text and attachments and
revokes its preview URLs. On validation or definitive rejection, the full draft
remains available for correction or retry.

The transcript renders the user text and a compact marker such as **2 images
attached**. It does not render sent thumbnails or base64 data. This prevents
large image bodies from being repeated on every Datastar activity-panel patch.

## Browser Architecture

A focused image-attachment module owns draft state and attachment behavior. Its
core decisions and state transitions are pure functions where practical, with a
small DOM-binding layer for:

- session selection changes;
- directive input synchronization;
- drag enter/leave/drop;
- file-input changes;
- clipboard paste;
- preview rendering and object URL lifetime;
- submission and response handling; and
- accessible status updates.

The existing terminal key-decision module continues deciding whether Enter means
submit. Its submit operation delegates to the attachment controller instead of
triggering a separate image-specific path. Text-only and image-bearing commands
therefore cannot diverge in keyboard or button behavior.

The controller uses `fetch` with `FormData`; the browser sets the multipart
boundary. Credentials remain same-origin. The endpoint returns
`application/json`: HTTP 202 with `{"accepted":true}` after Pi accepts the RPC,
or an appropriate 4xx/5xx status with
`{"accepted":false,"error":"sanitized operator message"}`. The controller
accepts no other response shape and never attempts to parse internal errors.

## Server Upload Boundary

The web composer posts to a new route:

```text
POST /ui/sessions/{sessionID}/messages
```

The route accepts `multipart/form-data` with:

- exactly one `message` text field; and
- zero to four `image` file fields.

The existing JSON `/ui/sessions/{sessionID}/steer` action remains available and
delegates to the same manager message operation with no attachments. The web
composer always uses the new message route for both text-only and image-bearing
submissions, so browser behavior has only one submission path.

The server uses `http.MaxBytesReader` around the complete body and iterates a
`multipart.Reader` directly. It does not call `ParseMultipartForm`, because that
API may spill files to disk. Every part is consumed through a bounded reader.
Unknown field names, duplicate message fields, malformed boundaries, truncated
parts, and unexpected non-file image parts are rejected before manager
invocation.

### Limits

The authoritative limits are:

- maximum directive text: 64 KiB;
- accepted formats: PNG, JPEG, GIF, and WebP;
- maximum attachments: 4;
- maximum decoded bytes per image: 3 MiB; and
- maximum combined decoded image bytes: 8 MiB.

The multipart request cap includes sufficient fixed overhead above the combined
field limits. The browser mirrors these limits for early feedback, but only
server checks are authoritative. Because browser and clipboard `File.type` may
be empty or inaccurate, client staging rejects only files that are definitely
unsupported; an unknown declared type may be staged and is decided by server
signature detection.

The 3 MiB per-image limit leaves headroom below Pi/provider inline-image base64
limits after expansion. The 8 MiB aggregate limit expands to approximately
10.7 MiB before JSON overhead, allowing one complete user-message event to stay
below the narrowly enlarged RPC record ceiling and the default 16 MiB supervisor
event byte budget.

### File validation

The server does not trust filename extensions or multipart `Content-Type`.
It detects the media type from file signatures, normalizes JPEG aliases to
`image/jpeg`, and accepts only the allowlist above. SVG is rejected even when
labelled as an image. Empty files and files whose signature does not match a
supported format are rejected.

Filenames are retained only for preview display in the browser; they are not
required by Pi's image block and are neither forwarded nor logged by the server.

## Browser Security Boundary

The message endpoint remains behind the browser-session middleware when session
authentication is enabled. Its write-boundary policy narrowly permits
`multipart/form-data` for this route while preserving the existing Host, Origin,
and `Sec-Fetch-Site` checks. Other action endpoints continue to require their
existing `application/json` content type.

The request body, image bytes, base64 payload, filenames, session cookie, and
internal RPC failures must not appear in application logs. Templates and DOM
updates treat filenames and validation messages as text rather than HTML.
Browser object URLs are revoked on removal, successful submission, session
removal, and page unload.

## Manager and Pi RPC Flow

The manager's message operation accepts typed attachments containing normalized
MIME type and bytes. It obtains `get_state` through the selected session's routed
root client before constructing the submission command.

The state projection used here includes:

- `isStreaming`; and
- the selected model's declared `input` capabilities.

When attachments are present, the manager requires the model input list to
contain `image`. A text-only or absent model is rejected before a prompt or steer
is submitted. Kanedias does not switch models automatically and does not silently
remove attachments.

After validation:

- an idle session receives Pi `prompt` with `message` and native `images`;
- a streaming session receives Pi `steer` with the same fields; and
- a text-only message follows the same existing idle/streaming decision.

Each Pi image has the exact RPC shape:

```json
{
  "type": "image",
  "data": "base64-encoded-data",
  "mimeType": "image/png"
}
```

Encoding occurs after all images pass validation, so a partially valid request
can never send only a subset.

## Routed Transport Limits

The current browser action cap, supervisor RPC request cap, descendant RPC
response cap, and Pi JSONL record cap are too small for native images. The
implementation introduces one shared image-aware RPC ceiling derived from the
approved decoded-byte limits and JSON/base64 overhead.

The larger ceiling applies only to:

- routed `/v1/sessions/{sessionID}/rpc` request bodies;
- routed Pi RPC responses such as `get_messages` for an image-bearing session;
- descendant SSE lines carrying a bounded Pi event; and
- Pi JSONL records.

Launch, question, handoff, and other JSON bodies retain their existing limits.
Oversized routed commands and records continue to fail deterministically rather
than being truncated. The enlarged record must remain below the default
supervisor event byte budget with explicit headroom for envelopes and directive
text.

Raw Pi events may contain native image data and therefore consume more of the
bounded supervisor replay ring, potentially evicting older events sooner. The
manager's browser activity projection keeps only the user text and attachment
count; it never forwards image data into the web view.

## Error Handling

The browser gives specific safe feedback for:

- unsupported image format;
- too many images;
- per-image or aggregate size overflow;
- a missing or no-longer-actionable session;
- a selected model without image input support;
- Pi prompt/steer rejection; and
- authentication or network failure.

The server maps internal errors to sanitized operator messages and logs only the
error category and request context required for diagnosis, never attachment
metadata or contents.

Pi's RPC response means accepted, not completed. Drafts clear after definitive
acceptance; later provider failures remain visible through normal session events.
If the HTTP connection fails after the upload and the browser cannot determine
whether Pi accepted it, the draft remains and the UI says:

> Delivery status unknown—check the transcript before retrying.

The design deliberately does not add an idempotency store. The warning makes the
possible duplicate explicit while keeping the feature free of server-side draft
state.

## Data Flow

1. The operator selects a session.
2. Drop, picker, or paste supplies candidate `File` objects.
3. Browser checks candidate type/count/size and creates preview object URLs.
4. The per-session draft renders in the composer.
5. Enter or **Steer** snapshots the session ID and builds `FormData`.
6. The server bounds the body, streams parts into bounded memory, and validates
   signatures and aggregate limits.
7. The manager routes `get_state` to the exact session.
8. The manager validates image capability and chooses `prompt` or `steer`.
9. Validated bytes become base64 Pi image blocks and traverse the bounded routed
   supervisor API.
10. Pi acknowledges acceptance.
11. The server returns a sanitized acceptance response.
12. The browser clears only the accepted draft and revokes its object URLs.
13. Pi events update the transcript with user text and an attachment count.

## Testing Strategy

### JavaScript tests

Use Node's built-in test runner with injected browser seams for files, object
URLs, selection, and fetch. Cover:

- drop, picker, and image clipboard paste sharing one staging path;
- normal text paste remaining native;
- per-session text and image isolation;
- save/restore across session switching;
- type, count, per-file, and aggregate validation;
- removing attachments and selecting the same file again;
- object URL revocation on removal, acceptance, session removal, and unload;
- stable target session during an in-flight selection change;
- success clearing only the submitted draft;
- rejection and transport failure preserving the draft;
- neutral text for an image-only directive; and
- Enter and button submission sharing one operation.

### Server tests

Cover:

- valid PNG, JPEG, GIF, and WebP multipart requests;
- signature-based rejection of renamed or mislabelled files;
- empty files, malformed boundaries, truncated parts, unknown fields, duplicate
  messages, excess files, and every size boundary;
- no manager invocation until the entire multipart request is valid;
- direct multipart-reader use without temporary-file artifacts;
- session authentication, same-origin checks, and route-specific media types;
- sanitized errors and absence of filenames/image data in logs; and
- unchanged body limits on unrelated endpoints.

### Manager and supervisor tests

Cover:

- exact native image RPC blocks for idle `prompt`;
- exact native image RPC blocks for streaming `steer`;
- text-only behavior remaining unchanged;
- image rejection for a text-only or absent model before submission;
- acceptance for an image-capable model;
- no partial command when one attachment is invalid;
- nested descendant routing at the enlarged request/response boundary;
- deterministic rejection one byte above every transport ceiling;
- successful parsing and projection of a user image event;
- attachment count retention with image data omitted from browser views; and
- bounded replay behavior for large image-bearing events.

`make test` remains the required hermetic verification entry point.

### Manual acceptance

Against a live local server and an image-capable configured model:

1. Stage multiple screenshots in session A.
2. Switch to session B, stage another image, return to A, and confirm both drafts
   remain isolated.
3. Remove one image and send the remainder with a typed directive.
4. Send an image-only directive and confirm the neutral generated text.
5. Attach while a turn is running and confirm the message is queued as a steer.
6. Select a text-only model and confirm rejection preserves the complete draft.
7. Confirm the transcript shows an attachment count without embedded base64.
8. Exercise desktop and mobile layouts, keyboard submission, clipboard paste,
   drag-and-drop, and the file picker.
9. Inspect host and session filesystems to confirm no attachment file was
   created.

## Success Criteria

The feature is complete when an operator can maintain independent image drafts
for multiple sessions, submit supported images to the exact intended session as
native Pi attachments, receive clear validation and model-capability feedback,
and observe sent attachment counts without unbounded browser or transport data.
All automated tests pass, the manual acceptance flow succeeds with both image-
capable and text-only models, and no image bytes are persisted outside Pi's own
normal session history.
