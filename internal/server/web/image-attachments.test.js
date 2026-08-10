"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const imageAttachments = require("./image-attachments.js");

const MiB = 1024 * 1024;
const imageFile = (name, type, size, lastModified = 1) => ({name, type, size, lastModified});

class FakeFormData {
  constructor() { this.parts = []; }
  append(name, value, filename) { this.parts.push({name, value, filename}); }
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return {promise, resolve, reject};
}

function jsonResponse(status, body, contentType = "application/json") {
  return {
    status,
    headers: {get(name) { return name.toLowerCase() === "content-type" ? contentType : null; }},
    json: async () => body
  };
}

function fixture(overrides = {}) {
  const revoked = [];
  const changes = [];
  const statuses = [];
  const requests = [];
  const options = {
    fetch: async (url, init) => {
      requests.push({url, init});
      return jsonResponse(202, {accepted: true});
    },
    FormData: FakeFormData,
    createObjectURL: (file) => "blob:" + (file.name || "pasted"),
    revokeObjectURL: (url) => revoked.push(url),
    onChange: (snapshot) => changes.push(snapshot),
    onStatus: (status) => statuses.push(status),
    ...overrides
  };
  return {controller: imageAttachments.createController(options), revoked, changes, statuses, requests};
}

function stage(controller, sessionID, files) {
  controller.selectSession(sessionID);
  controller.stageFiles(files);
  return controller.draft(sessionID);
}

test("exports the fixed limits and neutral image prompt", () => {
  assert.deepEqual(imageAttachments.LIMITS, {
    maxImages: 4,
    maxImageBytes: 3 * MiB,
    maxTotalBytes: 8 * MiB
  });
  assert.equal(imageAttachments.NEUTRAL_MESSAGE, "Please inspect the attached image(s).");
});

test("staging and submission reject safely without a selected session", async () => {
  const f = fixture();
  assert.deepEqual(f.controller.stageFiles([imageFile("a.png", "image/png", 1)]).images, []);
  assert.equal(f.controller.setText("hello"), false);
  assert.deepEqual(await f.controller.submit(), {outcome: "rejected", error: "Select a session before sending a message."});
  assert.equal(f.requests.length, 0);
  assert.equal(f.revoked.length, 0);
});

test("keeps independent session drafts and restores the selected snapshot", () => {
  const f = fixture();
  const a = imageFile("a.png", "image/png", 12);
  const b = imageFile("b.gif", "image/gif", 13);

  f.controller.selectSession("A");
  f.controller.setText("alpha");
  f.controller.stageFiles([a]);
  f.controller.selectSession("B");
  f.controller.setText("beta");
  f.controller.stageFiles([b]);

  assert.equal(f.controller.draft("A").text, "alpha");
  assert.deepEqual(f.controller.draft("A").images.map((image) => image.name), ["a.png"]);
  assert.equal(f.controller.draft("B").text, "beta");
  assert.deepEqual(f.controller.draft("B").images.map((image) => image.name), ["b.gif"]);
  assert.deepEqual(f.controller.selectSession("A"), f.controller.draft("A"));
});

test("rejects definitely unsupported MIME types while allowing an empty type", () => {
  const f = fixture();
  const draft = stage(f.controller, "A", [
    imageFile("notes.txt", "text/plain", 1),
    imageFile("camera", "", 2),
    imageFile("alias.jpg", "image/jpg", 3)
  ]);
  assert.deepEqual(draft.images.map((image) => image.name), ["camera"]);
  assert.match(f.statuses.at(-1), /PNG, JPEG, GIF, and WebP/);
});

test("enforces image count, per-image size, and total size at exact boundaries", () => {
  {
    const f = fixture();
    const files = [1, 2, 3, 4, 5].map((number) => imageFile(number + ".png", "image/png", 1));
    assert.equal(stage(f.controller, "A", files).images.length, 4);
  }
  {
    const f = fixture();
    const exact = imageFile("exact.png", "image/png", 3 * MiB);
    const over = imageFile("over.png", "image/png", 3 * MiB + 1);
    assert.deepEqual(stage(f.controller, "A", [exact, over]).images.map((image) => image.name), ["exact.png"]);
  }
  {
    const f = fixture();
    const exactTotal = [
      imageFile("a.png", "image/png", 3 * MiB),
      imageFile("b.png", "image/png", 3 * MiB),
      imageFile("c.png", "image/png", 2 * MiB)
    ];
    assert.equal(stage(f.controller, "A", exactTotal).totalBytes, 8 * MiB);
    f.controller.stageFiles([imageFile("extra.png", "image/png", 1)]);
    assert.equal(f.controller.draft("A").totalBytes, 8 * MiB);
  }
  {
    const f = fixture();
    const overTotal = [
      imageFile("a.png", "image/png", 3 * MiB),
      imageFile("b.png", "image/png", 3 * MiB),
      imageFile("c.png", "image/png", 2 * MiB + 1)
    ];
    assert.equal(stage(f.controller, "A", overTotal).totalBytes, 6 * MiB);
  }
});

test("removal revokes its object URL and accepted attachment IDs have no rejected gaps", () => {
  const f = fixture();
  const draft = stage(f.controller, "A", [
    imageFile("bad.bmp", "image/bmp", 1),
    imageFile("first.png", "image/png", 1),
    imageFile("large.png", "image/png", 3 * MiB + 1),
    imageFile("second.jpg", "image/jpeg", 1)
  ]);
  assert.deepEqual(draft.images.map((image) => image.id), [1, 2]);
  assert.equal(f.controller.removeImage("1"), true);
  assert.deepEqual(f.revoked, ["blob:first.png"]);
  assert.deepEqual(f.controller.draft("A").images.map((image) => image.id), [2]);
});

test("uses deterministic fallback labels for unnamed pasted images", () => {
  const f = fixture({createObjectURL: (file) => "blob:" + file.lastModified});
  const draft = stage(f.controller, "A", [
    imageFile("", "image/png", 1, 10),
    imageFile("", "image/webp", 1, 11)
  ]);
  assert.deepEqual(draft.images.map((image) => image.name), ["Pasted image 1", "Pasted image 2"]);
});

test("submits the server's singular image field after message in staged order", async () => {
  const f = fixture();
  f.controller.selectSession("a/b ?#");
  f.controller.setText("inspect");
  f.controller.stageFiles([
    imageFile("first.png", "image/png", 1),
    imageFile("second.jpg", "image/jpeg", 2)
  ]);

  assert.deepEqual(await f.controller.submit(), {outcome: "accepted"});
  assert.equal(f.requests[0].url, "/ui/sessions/a%2Fb%20%3F%23/messages");
  assert.equal(f.requests[0].init.method, "POST");
  assert.deepEqual(f.requests[0].init.body.parts.map((part) => [part.name, part.value.name || part.value]), [
    ["message", "inspect"],
    ["image", "first.png"],
    ["image", "second.jpg"]
  ]);
  assert.equal(f.requests[0].init.body.parts.some((part) => part.name === "images"), false,
    "the server rejects the plural field name");
});

test("uses the neutral prompt for image-only drafts and rejects entirely empty drafts", async () => {
  const f = fixture();
  f.controller.selectSession("A");
  assert.deepEqual(await f.controller.submit(), {outcome: "rejected", error: "Enter a message or attach an image."});
  assert.equal(f.requests.length, 0);

  f.controller.stageFiles([imageFile("only.png", "image/png", 1)]);
  await f.controller.submit();
  assert.equal(f.requests[0].init.body.parts[0].value, imageAttachments.NEUTRAL_MESSAGE);
});

test("locks only the captured in-flight draft while another session stays editable", async () => {
  const waiting = deferred();
  const f = fixture({fetch: () => waiting.promise});
  f.controller.selectSession("A");
  f.controller.setText("alpha");
  const submitted = f.controller.submit("A");
  assert.equal(f.controller.draft("A").busy, true);
  assert.equal(f.controller.setText("changed"), false);
  assert.equal(f.controller.draft("A").text, "alpha");

  f.controller.selectSession("B");
  assert.equal(f.controller.setText("beta"), true);
  assert.equal(f.controller.draft("B").text, "beta");
  assert.deepEqual(await f.controller.submit("A"), {outcome: "rejected", error: "This draft is already being sent."});

  waiting.resolve(jsonResponse(202, {accepted: true}));
  assert.deepEqual(await submitted, {outcome: "accepted"});
});

test("strict 202 acceptance clears and revokes only the captured draft", async () => {
  const f = fixture();
  stage(f.controller, "A", [imageFile("a.png", "image/png", 1)]);
  f.controller.setText("alpha");
  stage(f.controller, "B", [imageFile("b.png", "image/png", 1)]);
  f.controller.setText("beta");

  assert.deepEqual(await f.controller.submit("A"), {outcome: "accepted"});
  assert.equal(f.controller.draft("A").text, "");
  assert.deepEqual(f.controller.draft("A").images, []);
  assert.equal(f.controller.draft("B").text, "beta");
  assert.deepEqual(f.controller.draft("B").images.map((image) => image.name), ["b.png"]);
  assert.deepEqual(f.revoked, ["blob:a.png"]);
});

test("strict 4xx and 5xx rejection preserves and unlocks the draft", async () => {
  for (const status of [400, 503]) {
    const f = fixture({fetch: async () => jsonResponse(status, {accepted: false, error: "Safe rejection"})});
    f.controller.selectSession("A");
    f.controller.setText("keep me");
    assert.deepEqual(await f.controller.submit(), {outcome: "rejected", error: "Safe rejection"});
    assert.equal(f.controller.draft("A").text, "keep me");
    assert.equal(f.controller.draft("A").busy, false);
    assert.equal(f.statuses.at(-1), "Safe rejection");
  }
});

test("malformed responses and thrown fetches preserve drafts and report unknown delivery", async () => {
  const malformed = [
    jsonResponse(202, {accepted: true, extra: true}),
    jsonResponse(202, {accepted: false, error: "wrong status"}),
    jsonResponse(400, {accepted: false}),
    jsonResponse(400, {accepted: false, error: "x", extra: true}),
    jsonResponse(202, {accepted: true}, "text/plain"),
    {status: 202, headers: {get: () => "application/json"}, json: async () => { throw new Error("broken JSON"); }}
  ];
  for (const response of malformed) {
    const f = fixture({fetch: async () => response});
    f.controller.selectSession("A");
    f.controller.setText("keep me");
    assert.deepEqual(await f.controller.submit(), {outcome: "unknown"});
    assert.equal(f.controller.draft("A").text, "keep me");
    assert.equal(f.controller.draft("A").busy, false);
    assert.match(f.statuses.at(-1), /unknown/i);
  }

  const thrown = fixture({fetch: async () => { throw new Error("network down"); }});
  thrown.controller.selectSession("A");
  thrown.controller.setText("keep me");
  assert.deepEqual(await thrown.controller.submit(), {outcome: "unknown"});
  assert.equal(thrown.controller.draft("A").text, "keep me");
  assert.match(thrown.statuses.at(-1), /unknown/i);
});

test("snapshots passed to observers are detached and immutable", () => {
  const f = fixture();
  f.controller.selectSession("A");
  f.controller.stageFiles([imageFile("a.png", "image/png", 1)]);
  const observed = f.changes.at(-1);
  assert.equal(Object.isFrozen(observed), true);
  assert.equal(Object.isFrozen(observed.images), true);
  assert.equal(Object.isFrozen(observed.images[0]), true);
  assert.equal("file" in observed.images[0], false);
  assert.throws(() => { observed.images.push({}); }, TypeError);
  assert.deepEqual(f.controller.draft("A").images.map((image) => image.name), ["a.png"]);
});

test("late accepted or rejected outcomes cannot affect a reconciled-away and recreated session", async (t) => {
  const outcomes = [
    {name: "accepted", response: jsonResponse(202, {accepted: true}), result: {outcome: "accepted"}},
    {name: "rejected", response: jsonResponse(409, {accepted: false, error: "old rejection"}), result: {outcome: "rejected", error: "old rejection"}}
  ];

  for (const outcome of outcomes) {
    await t.test(outcome.name, async () => {
      const waiting = deferred();
      const f = fixture({fetch: () => waiting.promise});
      stage(f.controller, "A", [imageFile("old.png", "image/png", 1)]);
      f.controller.setText("old draft");
      const submitted = f.controller.submit("A");

      f.controller.reconcileSessions([]);
      f.controller.selectSession("A");
      f.controller.setText("replacement draft");
      f.controller.stageFiles([imageFile("replacement.png", "image/png", 1)]);
      const changesAtDetach = f.changes.length;
      const statusesAtDetach = f.statuses.length;

      waiting.resolve(outcome.response);
      assert.deepEqual(await submitted, outcome.result);
      assert.equal(f.controller.draft("A").text, "replacement draft");
      assert.deepEqual(f.controller.draft("A").images.map((image) => image.name), ["replacement.png"]);
      assert.equal(f.controller.draft("A").busy, false);
      assert.deepEqual(f.revoked, ["blob:old.png"]);
      assert.equal(f.changes.length, changesAtDetach, "detached request must not emit onChange");
      assert.equal(f.statuses.length, statusesAtDetach, "detached request must not emit onStatus");
    });
  }
});

test("destroy suppresses an in-flight request's late unknown-delivery observers", async () => {
  const waiting = deferred();
  const f = fixture({fetch: () => waiting.promise});
  stage(f.controller, "A", [imageFile("old.png", "image/png", 1)]);
  f.controller.setText("old draft");
  const submitted = f.controller.submit("A");

  f.controller.destroy();
  const changesAtDestroy = f.changes.length;
  const statusesAtDestroy = f.statuses.length;
  waiting.reject(new Error("late transport failure"));

  assert.deepEqual(await submitted, {outcome: "unknown"});
  assert.deepEqual(f.revoked, ["blob:old.png"]);
  assert.equal(f.changes.length, changesAtDestroy, "destroyed request must not emit onChange");
  assert.equal(f.statuses.length, statusesAtDestroy, "destroyed request must not emit onStatus");
});

test("reconciliation revokes and deletes drafts for removed sessions", () => {
  const f = fixture();
  stage(f.controller, "A", [imageFile("a.png", "image/png", 1)]);
  stage(f.controller, "B", [imageFile("b.png", "image/png", 1)]);
  f.controller.reconcileSessions(["B"]);
  assert.deepEqual(f.revoked, ["blob:a.png"]);
  assert.equal(f.controller.draft("A").text, "");
  assert.deepEqual(f.controller.draft("A").images, []);
  assert.deepEqual(f.controller.draft("B").images.map((image) => image.name), ["b.png"]);
});

test("destroy revokes every remaining object URL exactly once", () => {
  const f = fixture();
  stage(f.controller, "A", [imageFile("a.png", "image/png", 1)]);
  stage(f.controller, "B", [imageFile("b.png", "image/png", 1)]);
  f.controller.destroy();
  f.controller.destroy();
  assert.deepEqual(f.revoked.sort(), ["blob:a.png", "blob:b.png"]);
  assert.deepEqual(f.controller.draft("A").images, []);
  assert.deepEqual(f.controller.draft("B").images, []);
});
