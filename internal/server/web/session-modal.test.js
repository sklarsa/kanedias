"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const modalUI = require("./session-modal.js");

class FakeTarget {
  constructor() {
    this.listeners = new Map();
    this.disabled = false;
    this.attributes = new Map();
    this.hidden = false;
    this.parent = null;
  }
  addEventListener(type, listener, options) {
    const listeners = this.listeners.get(type) || [];
    listeners.push({ listener, capture: options === true || Boolean(options && options.capture) });
    this.listeners.set(type, listeners);
  }
  removeEventListener(type, listener, options) {
    const capture = options === true || Boolean(options && options.capture);
    const listeners = this.listeners.get(type) || [];
    this.listeners.set(type, listeners.filter((item) => item.listener !== listener || item.capture !== capture));
  }
  dispatch(type, init = {}) {
    const event = Object.assign({
      type,
      target: this,
      defaultPrevented: false,
      propagationStopped: false,
      immediatePropagationStopped: false,
      preventDefault() { this.defaultPrevented = true; },
      stopPropagation() { this.propagationStopped = true; },
      stopImmediatePropagation() {
        this.immediatePropagationStopped = true;
        this.propagationStopped = true;
      }
    }, init);
    for (const phase of [true, false]) {
      for (const item of this.listeners.get(type) || []) {
        if (item.capture !== phase || event.immediatePropagationStopped) continue;
        item.listener(event);
      }
    }
    return event;
  }
  getAttribute(name) { return this.attributes.has(name) ? this.attributes.get(name) : null; }
  setAttribute(name, value) { this.attributes.set(name, String(value)); }
  removeAttribute(name) { this.attributes.delete(name); }
  contains(target) {
    for (let current = target; current; current = current.parent) {
      if (current === this) return true;
    }
    return false;
  }
}

class FakeOption extends FakeTarget {
  constructor(value, levels, defaultThinking, defaultSelected = false) {
    super();
    this.value = value;
    this.textContent = value;
    this.defaultSelected = defaultSelected;
    if (levels !== undefined) this.setAttribute("data-thinking-levels", levels);
    if (defaultThinking !== undefined) this.setAttribute("data-default-thinking", defaultThinking);
  }
}

class FakeSelect extends FakeTarget {
  constructor(options) {
    super();
    this.options = options;
    const configured = options.find((item) => item.defaultSelected);
    this._value = configured ? configured.value : options.length ? options[0].value : "";
  }
  get selectedIndex() { return this.options.findIndex((option) => option.value === this._value); }
  get value() { return this._value; }
  set value(value) { this._value = String(value); }
  reset() {
    const configured = this.options.find((item) => item.defaultSelected);
    this._value = configured ? configured.value : this.options.length ? this.options[0].value : "";
  }
  replaceChildren(...options) {
    this.options = options;
    if (!options.some((option) => option.value === this._value)) this._value = options.length ? options[0].value : "";
  }
}

class FakeInput extends FakeTarget {
  constructor(defaultValue = "") {
    super();
    this.defaultValue = defaultValue;
    this.value = defaultValue;
  }
  reset() { this.value = this.defaultValue; }
}

class FakeDialog extends FakeTarget {
  constructor() {
    super();
    this.open = false;
    this.returnValue = "";
  }
  showModal() { this.open = true; }
  close() { this.open = false; this.dispatch("close"); }
}

function option(value, levels, defaultThinking, defaultSelected) {
  return new FakeOption(value, levels, defaultThinking, defaultSelected);
}

function fixture() {
  const document = new FakeTarget();
  document.createElement = function (tag) {
    if (tag !== "option") throw new Error("unexpected element: " + tag);
    return new FakeOption("");
  };

  const dialog = new FakeDialog();
  dialog.parent = document;
  const trigger = new FakeTarget();
  const form = new FakeTarget();
  const close = new FakeTarget();
  const cancel = new FakeTarget();
  const launch = new FakeTarget();
  const status = new FakeTarget();
  status.textContent = "";
  const fieldset = new FakeTarget();
  const details = new FakeTarget();
  details.open = true;

  const sessionName = new FakeInput();
  const repositoryRoot = new FakeTarget();
  repositoryRoot.parent = dialog;
  const repositoryQuery = new FakeInput();
  repositoryQuery.parent = repositoryRoot;
  repositoryQuery.setAttribute("aria-expanded", "false");
  const startRepository = new FakeInput();
  startRepository.parent = repositoryRoot;
  const repositoryListbox = new FakeTarget();
  repositoryListbox.parent = repositoryRoot;
  repositoryListbox.hidden = true;
  const repositoryResults = new FakeTarget();
  repositoryResults.parent = repositoryRoot;
  repositoryResults.textContent = "";
  const repositoryOptions = ["", "one/alpha", "owner/repo"].map((value, index) => {
    const repositoryOption = new FakeTarget();
    repositoryOption.parent = repositoryListbox;
    repositoryOption.textContent = value || "/workspace";
    repositoryOption.setAttribute("id", "repository-option-" + index);
    repositoryOption.setAttribute("data-value", value);
    repositoryOption.setAttribute("aria-selected", value === "" ? "true" : "false");
    return repositoryOption;
  });
  const repositorySelectors = new Map([
    ["[data-repository-query]", repositoryQuery],
    ["[data-start-repository]", startRepository],
    ["[data-repository-listbox]", repositoryListbox],
    ["[data-repository-results]", repositoryResults]
  ]);
  repositoryRoot.querySelector = (selector) => repositorySelectors.get(selector) || null;
  repositoryRoot.querySelectorAll = (selector) => selector === "[data-repository-option]" ? repositoryOptions : [];

  form.resetCalls = 0;
  form.reset = function () {
    this.resetCalls++;
    for (const control of [sessionName, repositoryQuery, startRepository, rootModel, rootThinking, workerModelA, workerThinkingA, workerModelB, workerThinkingB]) {
      control.reset();
    }
  };
  const rootModel = new FakeSelect([
    option("deep", "high,xhigh", "high", true),
    option("fast", "off,medium", "off")
  ]);
  rootModel.focusCalls = 0;
  rootModel.focus = function () { this.focusCalls++; };
  const rootThinking = new FakeSelect([option("high"), option("xhigh", undefined, undefined, true)]);

  const workerModelA = new FakeSelect([
    option("deep", "high,xhigh", "high", true),
    option("fast", "off,medium", "off")
  ]);
  const workerThinkingA = new FakeSelect([option("high", undefined, undefined, true), option("xhigh")]);
  const workerA = new FakeTarget();
  workerA.setAttribute("data-worker-type", "oracle");
  workerA.querySelector = (selector) => selector === "[data-worker-model]" ? workerModelA : workerThinkingA;

  const workerModelB = new FakeSelect([
    option("deep", "high,xhigh", "high"),
    option("solo", "off", "off", true)
  ]);
  const workerThinkingB = new FakeSelect([option("off", undefined, undefined, true)]);
  const workerB = new FakeTarget();
  workerB.setAttribute("data-worker-type", "worker");
  workerB.querySelector = (selector) => selector === "[data-worker-model]" ? workerModelB : workerThinkingB;

  const controls = [close, cancel, launch, sessionName, repositoryQuery, startRepository, rootModel, rootThinking, workerModelA, workerThinkingA, workerModelB, workerThinkingB];
  const query = new Map([
    ["#new-session-modal", dialog],
    ["#new-session-button", trigger],
    ["#new-session-form", form],
    ["[data-modal-close]", close],
    ["#new-session-cancel", cancel],
    ["#new-session-launch", launch],
    ["#new-session-status", status],
    ["[data-session-name]", sessionName],
    ["[data-repository-combobox]", repositoryRoot],
    ["[data-start-repository]", startRepository],
    ["[data-root-model]", rootModel],
    ["[data-root-thinking]", rootThinking]
  ]);
  document.querySelector = (selector) => query.get(selector) || null;
  dialog.querySelector = (selector) => query.get(selector) || null;
  dialog.querySelectorAll = (selector) => {
    if (selector === "[data-worker-row]") return [workerA, workerB];
    if (selector === "button, select, input, textarea") return controls;
    if (selector === "fieldset") return [fieldset, workerA, workerB];
    return [];
  };

  function chooseRepository(value) {
    const repositoryOption = repositoryOptions.find((item) => item.getAttribute("data-value") === value);
    if (!repositoryOption) throw new Error("unknown repository: " + value);
    repositoryQuery.value = value;
    repositoryQuery.dispatch("input");
    repositoryOption.dispatch("pointerdown");
  }

  return {
    document, dialog, trigger, form, close, cancel, launch, status, fieldset, details,
    sessionName, repositoryRoot, repositoryQuery, startRepository, repositoryListbox, repositoryResults, repositoryOptions,
    rootModel, rootThinking, workerModelA, workerThinkingA, workerModelB, workerThinkingB,
    workerA, workerB, controls, chooseRepository
  };
}

function response(status, body, contentLength, contentType = "application/json") {
  return {
    status,
    ok: status >= 200 && status < 300,
    headers: { get(name) {
      if (name.toLowerCase() === "content-length") return contentLength || null;
      if (name.toLowerCase() === "content-type") return contentType;
      return null;
    } },
    text: async () => typeof body === "string" ? body : JSON.stringify(body)
  };
}

async function settle() {
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

test("open resets configured defaults, rebuilds thinking, and focuses the root model", () => {
  const f = fixture();
  // Native selects keep the server's selected attribute as defaultSelected even
  // when their current selectedIndex/value changes before binding.
  f.sessionName.value = "stale name";
  f.startRepository.value = "owner/repo";
  f.rootModel.value = "fast";
  f.rootThinking.value = "high";
  const controller = modalUI.bind(f.document, async () => response(201, { sessionId: "new" }));
  f.trigger.dispatch("click");
  assert.equal(f.form.resetCalls, 1);
  assert.equal(f.dialog.open, true);
  assert.equal(f.sessionName.value, "");
  assert.equal(f.startRepository.value, "");
  assert.equal(f.rootModel.value, "deep");
  assert.equal(f.rootModel.selectedIndex, 0);
  assert.equal(f.rootModel.options[0].defaultSelected, true);
  assert.deepEqual(f.rootThinking.options.map((item) => item.value), ["high", "xhigh"]);
  assert.equal(f.rootThinking.value, "xhigh");
  assert.equal(f.rootModel.focusCalls, 1);
  assert.equal(f.status.textContent, "");
  controller.destroy();
});

test("model changes replace thinking choices and clamp unsupported values to model default", () => {
  const f = fixture();
  f.rootModel.value = "deep";
  f.rootThinking.value = "medium";
  modalUI.rebuildThinking(f.document, f.rootModel, f.rootThinking);
  assert.deepEqual(f.rootThinking.options.map((item) => item.value), ["high", "xhigh"]);
  assert.equal(f.rootThinking.value, "high");

  f.rootThinking.value = "xhigh";
  modalUI.rebuildThinking(f.document, f.rootModel, f.rootThinking);
  assert.equal(f.rootThinking.value, "xhigh", "supported current selection is preserved");
});

test("one-level model displays but disables its thinking selector", () => {
  const f = fixture();
  f.workerModelB.value = "solo";
  modalUI.rebuildThinking(f.document, f.workerModelB, f.workerThinkingB);
  assert.deepEqual(f.workerThinkingB.options.map((item) => item.value), ["off"]);
  assert.equal(f.workerThinkingB.value, "off");
  assert.equal(f.workerThinkingB.disabled, true);
});

test("buildRequest returns raw name and repository with the root and every worker exactly once", () => {
  const f = fixture();
  f.sessionName.value = "release triage";
  f.startRepository.value = "owner/repo";
  f.rootModel.value = "fast";
  f.rootThinking.replaceChildren(option("off"), option("medium"));
  f.rootThinking.value = "medium";
  f.workerModelA.value = "fast";
  f.workerThinkingA.replaceChildren(option("off"), option("medium"));
  f.workerThinkingA.value = "off";
  f.workerModelB.value = "deep";
  f.workerThinkingB.replaceChildren(option("high"), option("xhigh"));
  f.workerThinkingB.value = "xhigh";
  const got = modalUI.buildRequest(f.dialog);
  assert.deepEqual(got, {
    name: "release triage",
    repository: "owner/repo",
    root: { modelType: "fast", thinkingLevel: "medium" },
    workers: [
      { workerType: "oracle", modelType: "fast", thinkingLevel: "off" },
      { workerType: "worker", modelType: "deep", thinkingLevel: "xhigh" }
    ]
  });
});

test("unmatched repository blocks fetch and preserves the modal", () => {
  const f = fixture();
  let fetchCalls = 0;
  modalUI.bind(f.document, async () => { fetchCalls++; return response(201, { sessionId: "x" }); });
  f.trigger.dispatch("click");
  f.repositoryQuery.value = "invented/repo";
  f.repositoryQuery.dispatch("input");
  f.form.dispatch("submit");
  assert.equal(fetchCalls, 0);
  assert.equal(f.dialog.open, true);
  assert.equal(f.status.textContent,
    "Choose a configured repository or clear the field to use /workspace.");
});

test("selected autocomplete value is the exact launch repository", async () => {
  const f = fixture();
  const calls = [];
  modalUI.bind(f.document, async (_url, options) => {
    calls.push(JSON.parse(options.body));
    return response(201, { sessionId: "created" });
  });
  f.trigger.dispatch("click");
  f.chooseRepository("owner/repo");
  f.form.dispatch("submit");
  await settle();
  assert.equal(calls[0].repository, "owner/repo");
});

test("Launch posts the exact complete request and disables controls while pending", async () => {
  const f = fixture();
  let resolveFetch;
  const calls = [];
  modalUI.bind(f.document, (url, options) => {
    calls.push({ url, options });
    return new Promise((resolve) => { resolveFetch = resolve; });
  });
  f.trigger.dispatch("click");
  f.sessionName.value = "release triage";
  f.startRepository.value = "owner/repo";
  const expected = modalUI.buildRequest(f.dialog);
  f.form.dispatch("submit");
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, "/ui/sessions");
  assert.deepEqual(calls[0].options, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(expected),
    credentials: "same-origin"
  });
  assert.equal(f.dialog.getAttribute("aria-busy"), "true");
  assert.equal(f.launch.textContent, "Launching…");
  assert.equal(f.sessionName.disabled, true);
  assert.equal(f.startRepository.disabled, true);
  assert.ok(f.controls.every((control) => control.disabled));

  resolveFetch(response(201, { sessionId: "created" }));
  await settle();
});

test("pending launch cannot be closed or reopened into a duplicate submission", async () => {
  const f = fixture();
  const pending = deferred();
  let fetchCalls = 0;
  const controller = modalUI.bind(f.document, () => {
    fetchCalls++;
    return pending.promise;
  });
  f.trigger.dispatch("click");
  f.form.dispatch("submit");
  const resets = f.form.resetCalls;

  for (const action of [
    () => f.document.dispatch("keydown", { key: "Escape" }),
    () => f.dialog.dispatch("cancel"),
    () => f.dialog.dispatch("click", { target: f.dialog }),
    () => f.cancel.dispatch("click"),
    () => f.close.dispatch("click"),
    () => controller.close(),
    () => f.trigger.dispatch("click"),
    () => f.form.dispatch("submit")
  ]) action();

  assert.equal(f.dialog.open, true);
  assert.equal(f.dialog.getAttribute("aria-busy"), "true");
  assert.equal(f.form.resetCalls, resets);
  assert.equal(fetchCalls, 1);
  pending.resolve(response(201, { sessionId: "created" }));
  await settle();
  assert.equal(f.dialog.open, false);
});

test("failed HTTP keeps modal open, restores controls, and shows bounded sanitized text", async () => {
  const f = fixture();
  const unsafe = " <b>not allowed</b> " + "x".repeat(700);
  modalUI.bind(f.document, async () => response(400, { error: unsafe }));
  f.trigger.dispatch("click");
  f.sessionName.value = "keep this name";
  f.chooseRepository("owner/repo");
  f.form.dispatch("submit");
  await settle();

  assert.equal(f.dialog.open, true);
  assert.equal(f.sessionName.value, "keep this name");
  assert.equal(f.repositoryQuery.value, "owner/repo");
  assert.equal(f.startRepository.value, "owner/repo");
  assert.equal(f.dialog.getAttribute("aria-busy"), null);
  assert.equal(f.launch.disabled, false);
  assert.equal(f.cancel.disabled, false);
  assert.equal(f.rootModel.disabled, false);
  assert.equal(f.workerThinkingB.disabled, true, "configured one-level selector remains disabled");
  assert.equal(f.launch.textContent, "Launch");
  assert.ok(f.status.textContent.startsWith("<b>not allowed</b>"));
  assert.ok(f.status.textContent.length <= 300);
  assert.equal(Object.prototype.hasOwnProperty.call(f.status, "innerHTML"), false);
});

test("oversized and malformed failure responses use a safe generic message", async () => {
  for (const reply of [
    response(500, { error: "secret" }, "70000"),
    { status: 500, ok: false, headers: { get() { return null; } }, text: async () => "{" }
  ]) {
    const f = fixture();
    modalUI.bind(f.document, async () => reply);
    f.trigger.dispatch("click");
    f.form.dispatch("submit");
    await settle();
    assert.equal(f.dialog.open, true);
    assert.equal(f.status.textContent, "The session could not be launched. Please try again.");
  }
});

test("synchronous fetch failure restores controls and keeps the modal open", async () => {
  const f = fixture();
  modalUI.bind(f.document, () => { throw new Error("offline"); });
  f.trigger.dispatch("click");
  f.form.dispatch("submit");
  await settle();
  assert.equal(f.dialog.open, true);
  assert.equal(f.launch.disabled, false);
  assert.equal(f.status.textContent, "The session could not be launched. Please try again.");
});

test("HTTP 200 with a valid-looking body is not launch success", async () => {
  const f = fixture();
  modalUI.bind(f.document, async () => response(200, { sessionId: "not-created" }));
  f.trigger.dispatch("click");
  f.form.dispatch("submit");
  await settle();
  assert.equal(f.dialog.open, true);
  assert.equal(f.status.textContent, "The session could not be launched. Please try again.");
  assert.equal(f.dialog.getAttribute("aria-busy"), null);
});

test("streaming response without Content-Length is canceled at the byte bound", async () => {
  const f = fixture();
  const chunks = [new Uint8Array(40000), new Uint8Array(40000)];
  let reads = 0;
  let cancels = 0;
  const reply = {
    status: 201,
    headers: { get(name) { return name.toLowerCase() === "content-type" ? "application/json" : null; } },
    body: {
      getReader() {
        return {
          async read() {
            const value = chunks[reads++];
            return value ? { done: false, value } : { done: true };
          },
          cancel() { cancels++; }
        };
      }
    },
    text() { throw new Error("streaming response must not use text()"); }
  };
  modalUI.bind(f.document, async () => reply);
  f.trigger.dispatch("click");
  f.form.dispatch("submit");
  await settle();
  assert.equal(reads, 2);
  assert.equal(cancels, 1);
  assert.equal(f.dialog.open, true);
  assert.equal(f.status.textContent, "The session could not be launched. Please try again.");
});

test("201 success requires exact JSON session object and JSON content type", async () => {
  const invalidReplies = [
    response(201, {}),
    response(201, { sessionId: "" }),
    response(201, { sessionId: "  " }),
    response(201, { sessionId: 42 }),
    response(201, { sessionId: "created", extra: true }),
    response(201, "not-json"),
    response(201, { sessionId: "created" }, null, "text/plain"),
    response(201, { sessionId: "created" }, null, null)
  ];
  for (const reply of invalidReplies) {
    const f = fixture();
    modalUI.bind(f.document, async () => reply);
    f.trigger.dispatch("click");
    f.form.dispatch("submit");
    await settle();
    assert.equal(f.dialog.open, true);
    assert.equal(f.status.textContent, "The session could not be launched. Please try again.");
    assert.equal(f.dialog.getAttribute("aria-busy"), null);
  }
});

test("201 closes and resets the modal", async () => {
  const f = fixture();
  modalUI.bind(f.document, async () => response(201, { sessionId: "created" }));
  f.trigger.dispatch("click");
  f.sessionName.value = "completed name";
  f.startRepository.value = "owner/repo";
  f.rootModel.value = "fast";
  f.form.dispatch("submit");
  await settle();
  assert.equal(f.dialog.open, false);
  assert.equal(f.form.resetCalls, 2);
  assert.equal(f.sessionName.value, "");
  assert.equal(f.startRepository.value, "");
  assert.equal(f.rootModel.value, "deep");
  assert.equal(f.status.textContent, "");
});

test("Cancel, close, backdrop, and native cancel close without fetch", () => {
  const f = fixture();
  let fetchCalls = 0;
  modalUI.bind(f.document, async () => { fetchCalls++; return response(201, {}); });
  for (const closeAction of [
    () => f.cancel.dispatch("click"),
    () => f.close.dispatch("click"),
    () => f.dialog.dispatch("click", { target: f.dialog }),
    () => f.dialog.dispatch("cancel")
  ]) {
    f.trigger.dispatch("click");
    const before = f.form.resetCalls;
    const event = closeAction();
    assert.equal(f.dialog.open, false);
    assert.equal(f.form.resetCalls, before + 1);
    if (event) assert.equal(event.defaultPrevented, true);
  }
  assert.equal(fetchCalls, 0);
});

test("Escape is capture-consumed while open before terminal interrupt but unchanged outside", () => {
  const f = fixture();
  let interrupts = 0;
  let fetchCalls = 0;
  modalUI.bind(f.document, async () => { fetchCalls++; return response(201, {}); });
  f.document.addEventListener("keydown", (event) => {
    if (event.key === "Escape") interrupts++;
  });

  f.trigger.dispatch("click");
  const guarded = f.document.dispatch("keydown", { key: "Escape" });
  assert.equal(guarded.defaultPrevented, true);
  assert.equal(guarded.immediatePropagationStopped, true);
  assert.equal(f.dialog.open, false);
  assert.equal(interrupts, 0);
  assert.equal(fetchCalls, 0);

  const outside = f.document.dispatch("keydown", { key: "Escape" });
  assert.equal(outside.defaultPrevented, false);
  assert.equal(interrupts, 1);
});

test("stale resolved and rejected fetches cannot mutate after destroy", async () => {
  for (const completion of ["resolve", "reject"]) {
    const f = fixture();
    const pending = deferred();
    const controller = modalUI.bind(f.document, () => pending.promise);
    f.trigger.dispatch("click");
    f.form.dispatch("submit");
    assert.equal(f.dialog.getAttribute("aria-busy"), "true");

    controller.destroy();
    f.status.textContent = completion + "-after-destroy";
    const snapshot = {
      open: f.dialog.open,
      busy: f.dialog.getAttribute("aria-busy"),
      launchDisabled: f.launch.disabled,
      status: f.status.textContent
    };

    if (completion === "resolve") pending.resolve(response(201, { sessionId: "stale" }));
    else pending.reject(new Error("stale failure"));
    await settle();
    assert.deepEqual({
      open: f.dialog.open,
      busy: f.dialog.getAttribute("aria-busy"),
      launchDisabled: f.launch.disabled,
      status: f.status.textContent
    }, snapshot, completion + " completion mutated state after destroy");
  }
});

test("bind is idempotent and destroy removes every registered interaction listener", () => {
  const f = fixture();
  let fetchCalls = 0;
  const first = modalUI.bind(f.document, async () => { fetchCalls++; return response(201, {}); });
  const second = modalUI.bind(f.document, async () => { fetchCalls++; return response(201, {}); });
  assert.equal(first, second);
  f.trigger.dispatch("click");
  assert.equal(f.form.resetCalls, 1);
  second.destroy();
  f.dialog.close();

  f.repositoryQuery.value = "owner/repo";
  f.repositoryQuery.dispatch("input");
  f.repositoryOptions[2].dispatch("pointerdown");
  assert.equal(f.startRepository.value, "", "repository combobox listener remained");

  const resetCalls = f.form.resetCalls;
  f.trigger.dispatch("click");
  assert.equal(f.dialog.open, false, "trigger listener remained");

  const submit = f.form.dispatch("submit");
  assert.equal(submit.defaultPrevented, false, "submit listener remained");
  assert.equal(fetchCalls, 0);

  f.rootModel.value = "fast";
  f.workerModelA.value = "fast";
  f.workerModelB.value = "deep";
  const thinkingBefore = [f.rootThinking, f.workerThinkingA, f.workerThinkingB]
    .map((select) => select.options.map((item) => item.value));
  f.rootModel.dispatch("change");
  f.workerModelA.dispatch("change");
  f.workerModelB.dispatch("change");
  assert.deepEqual(
    [f.rootThinking, f.workerThinkingA, f.workerThinkingB]
      .map((select) => select.options.map((item) => item.value)),
    thinkingBefore,
    "a model listener remained"
  );

  for (const dispatch of [
    () => f.cancel.dispatch("click"),
    () => f.close.dispatch("click"),
    () => f.dialog.dispatch("click", { target: f.dialog }),
    () => f.dialog.dispatch("cancel")
  ]) {
    f.dialog.open = true;
    const event = dispatch();
    assert.equal(f.dialog.open, true, "cancel/close listener remained");
    assert.equal(event.defaultPrevented, false);
  }

  f.dialog.open = true;
  const escape = f.document.dispatch("keydown", { key: "Escape" });
  assert.equal(escape.defaultPrevented, false, "document Escape listener remained");
  assert.equal(f.dialog.open, true);
  assert.equal(f.form.resetCalls, resetCalls);
});
