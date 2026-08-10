(function (root, factory) {
  "use strict";
  var api = factory();
  if (typeof module === "object" && module.exports) module.exports = api;
  if (root) root.KanediasComposerUI = api;
})(typeof globalThis !== "undefined" ? globalThis : this, function () {
  "use strict";

  function numericStyle(style, name) {
    var value = parseFloat(style && style[name]);
    return Number.isFinite(value) ? value : 0;
  }

  function autoSizeComposer(input, windowObject) {
    var style = windowObject.getComputedStyle(input);
    var lineHeight = numericStyle(style, "lineHeight") || 21.75;
    var chrome = numericStyle(style, "paddingTop") + numericStyle(style, "paddingBottom") +
      numericStyle(style, "borderTopWidth") + numericStyle(style, "borderBottomWidth");
    var minHeight = Math.ceil(lineHeight * 2 + chrome);
    var maxHeight = Math.ceil(lineHeight * 6 + chrome);
    input.style.height = "auto";
    var desired = Math.ceil(input.scrollHeight + numericStyle(style, "borderTopWidth") + numericStyle(style, "borderBottomWidth"));
    var height = Math.max(minHeight, Math.min(maxHeight, desired));
    var overflowing = desired > maxHeight;
    input.style.height = height + "px";
    input.style.overflowY = overflowing ? "auto" : "hidden";
    return {height: height, minHeight: minHeight, maxHeight: maxHeight, overflowing: overflowing};
  }

  function bindComposer(documentObject, windowObject) {
    var terminalUI = windowObject.KanediasTerminalUI;
    var appShell = documentObject.querySelector(".app");
    var deck = documentObject.querySelector(".deck");
    var input = documentObject.querySelector(".deck-input");
    var tray = documentObject.getElementById("image-attachment-tray");
    var attachButton = documentObject.getElementById("attach-images-button");
    var fileInput = documentObject.getElementById("image-file-input");
    var steerButton = documentObject.getElementById("steerBtn");
    var deckStatus = documentObject.getElementById("deck-status");
    var selectedSessionID = "";
    var dragDepth = 0;
    var fleetObserver = null;
    var capabilityObserver = null;
    var controller;
    var previewKey = null;
    var restoreFocusSessionID = "";
    var pendingRemovalIndex = null;
    var statuses = new Map();
    var listenerCleanups = [];
    var destroyed = false;

    function listen(target, type, listener) {
      target.addEventListener(type, listener);
      listenerCleanups.push(function () { target.removeEventListener(type, listener); });
    }

    tray.classList.add("image-attachment-tray");
    tray.setAttribute("aria-label", "Image attachments");

    function formatBytes(bytes) {
      if (bytes < 1024) return bytes + " B";
      if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(bytes < 10 * 1024 ? 1 : 0) + " KiB";
      return (bytes / (1024 * 1024)).toFixed(1) + " MiB";
    }

    function canEditSelectedDraft() {
      var detail = documentObject.getElementById("detail-panel");
      return !!selectedSessionID && !!detail && detail.getAttribute("data-session-id") === selectedSessionID &&
        terminalUI.detailCapability(documentObject, "steer") &&
        !controller.draft(selectedSessionID).busy;
    }

    function syncEditControls() {
      terminalUI.syncDeckState(documentObject);
      var canEdit = canEditSelectedDraft();
      terminalUI.setActionControlState(input, canEdit);
      terminalUI.setActionControlState(steerButton, canEdit);
      terminalUI.setActionControlState(attachButton, canEdit);
      terminalUI.setActionControlState(fileInput, canEdit);
      var removeButtons = tray.querySelectorAll("[data-remove-image]");
      for (var i = 0; i < removeButtons.length; i++) removeButtons[i].disabled = !canEdit;
      if (!canEdit) {
        dragDepth = 0;
        deck.classList.remove("drop-active");
      }
    }

    function renderDraft(snapshot) {
      if (!snapshot) return;
      if (snapshot.busy && snapshot.sessionID === selectedSessionID && documentObject.activeElement === input) {
        restoreFocusSessionID = snapshot.sessionID;
      }
      if (!snapshot.busy && restoreFocusSessionID === snapshot.sessionID && snapshot.sessionID !== selectedSessionID) {
        restoreFocusSessionID = "";
      }
      if (snapshot.sessionID !== selectedSessionID) return;
      if (input.value !== snapshot.text) input.value = snapshot.text;
      autoSizeComposer(input, windowObject);

      var nextPreviewKey = snapshot.images.map(function (image) { return image.id; }).join(",");
      if (nextPreviewKey !== previewKey) {
        while (tray.firstChild) tray.removeChild(tray.firstChild);
        snapshot.images.forEach(function (image) {
          var card = documentObject.createElement("div");
          card.className = "image-attachment-card";

          var preview = documentObject.createElement("img");
          preview.src = image.url;
          preview.alt = "";
          card.appendChild(preview);

          var metadata = documentObject.createElement("span");
          metadata.className = "image-attachment-meta";
          var filename = documentObject.createElement("span");
          filename.className = "image-attachment-name";
          filename.textContent = image.name;
          var size = documentObject.createElement("span");
          size.className = "image-attachment-size";
          size.textContent = formatBytes(image.size);
          metadata.appendChild(filename);
          metadata.appendChild(size);
          card.appendChild(metadata);

          var remove = documentObject.createElement("button");
          remove.type = "button";
          remove.className = "image-attachment-remove";
          remove.setAttribute("data-remove-image", String(image.id));
          remove.setAttribute("aria-label", "Remove " + image.name);
          remove.textContent = "×";
          remove.disabled = !canEditSelectedDraft();
          card.appendChild(remove);
          tray.appendChild(card);
        });
        previewKey = nextPreviewKey;
      }

      tray.hidden = snapshot.images.length === 0;
      appShell.classList.toggle("has-image-draft", snapshot.images.length > 0);
      deck.setAttribute("aria-busy", snapshot.busy ? "true" : "false");
      syncEditControls();
      if (pendingRemovalIndex !== null) {
        var adjacent = tray.querySelectorAll("[data-remove-image]");
        var target = adjacent[pendingRemovalIndex] || adjacent[pendingRemovalIndex - 1] ||
          (!attachButton.disabled ? attachButton : input);
        pendingRemovalIndex = null;
        if (target && typeof target.focus === "function") target.focus();
      }
      if (!snapshot.busy && restoreFocusSessionID === snapshot.sessionID) {
        restoreFocusSessionID = "";
        if (!input.disabled) input.focus();
      }
    }

    controller = windowObject.KanediasImageAttachments.createController({
      fetch: windowObject.fetch.bind(windowObject),
      FormData: windowObject.FormData,
      createObjectURL: windowObject.URL.createObjectURL.bind(windowObject.URL),
      revokeObjectURL: windowObject.URL.revokeObjectURL.bind(windowObject.URL),
      onChange: renderDraft,
      onStatus: function (message, sessionID) {
        if (sessionID) statuses.set(sessionID, message);
        if (sessionID === "" || sessionID === selectedSessionID) deckStatus.textContent = message;
      }
    });

    function selectComposerSession(sessionID, revokeCapability) {
      if (selectedSessionID && canEditSelectedDraft()) controller.setText(input.value);
      if (revokeCapability) {
        var detail = documentObject.getElementById("detail-panel");
        if (detail) detail.setAttribute("data-can-steer", "false");
      }
      dragDepth = 0;
      deck.classList.remove("drop-active");
      selectedSessionID = sessionID || "";
      previewKey = null;
      deckStatus.textContent = statuses.get(selectedSessionID) || "";
      renderDraft(controller.selectSession(selectedSessionID));
    }

    function submitSelectedDraft() {
      var capturedSessionID = selectedSessionID;
      if (!canEditSelectedDraft()) {
        if (!capturedSessionID) controller.submit("");
        return;
      }
      if (!controller.setText(input.value)) return;
      controller.submit(capturedSessionID);
    }

    listen(input, "input", function () {
      autoSizeComposer(input, windowObject);
      if (canEditSelectedDraft()) controller.setText(input.value);
    });
    listen(windowObject, "resize", function () {
      autoSizeComposer(input, windowObject);
    });
    if (documentObject.fonts && documentObject.fonts.ready) {
      documentObject.fonts.ready.then(function () {
        if (!destroyed) autoSizeComposer(input, windowObject);
      });
    }

    listen(attachButton, "click", function () {
      if (canEditSelectedDraft()) fileInput.click();
    });

    listen(fileInput, "change", function () {
      try {
        if (canEditSelectedDraft()) controller.stageFiles(fileInput.files);
      } finally {
        fileInput.value = "";
      }
    });

    listen(input, "paste", function (event) {
      var items = event.clipboardData && event.clipboardData.items;
      if (!items || !canEditSelectedDraft()) return;
      var images = [];
      for (var i = 0; i < items.length; i++) {
        var declaredType = String(items[i].type || "").trim().toLowerCase();
        if (items[i].kind !== "file" || (declaredType && declaredType.indexOf("image/") !== 0)) continue;
        var image = items[i].getAsFile();
        if (image) images.push(image);
      }
      if (images.length === 0) return;
      event.preventDefault();
      controller.stageFiles(images);
    });

    function hasFiles(dataTransfer) {
      if (!dataTransfer) return false;
      var types = dataTransfer.types || [];
      for (var i = 0; i < types.length; i++) {
        if (types[i] === "Files") return true;
      }
      return !!dataTransfer.files && dataTransfer.files.length > 0;
    }

    listen(deck, "dragenter", function (event) {
      if (!hasFiles(event.dataTransfer) || !canEditSelectedDraft()) return;
      event.preventDefault();
      dragDepth++;
      deck.classList.add("drop-active");
    });
    listen(deck, "dragover", function (event) {
      if (!hasFiles(event.dataTransfer) || !canEditSelectedDraft()) return;
      event.preventDefault();
      if (event.dataTransfer) event.dataTransfer.dropEffect = "copy";
    });
    listen(deck, "dragleave", function (event) {
      if (!hasFiles(event.dataTransfer) && dragDepth === 0) return;
      event.preventDefault();
      dragDepth = Math.max(0, dragDepth - 1);
      if (dragDepth === 0) deck.classList.remove("drop-active");
    });
    listen(deck, "drop", function (event) {
      if (!hasFiles(event.dataTransfer)) return;
      event.preventDefault();
      dragDepth = 0;
      deck.classList.remove("drop-active");
      if (canEditSelectedDraft()) controller.stageFiles(event.dataTransfer.files);
    });

    listen(tray, "click", function (event) {
      var remove = event.target.closest("[data-remove-image]");
      if (!remove || !canEditSelectedDraft()) return;
      var removeButtons = tray.querySelectorAll("[data-remove-image]");
      if (documentObject.activeElement === remove) {
        pendingRemovalIndex = Array.prototype.indexOf.call(removeButtons, remove);
      }
      controller.removeImage(remove.getAttribute("data-remove-image"));
    });

    listen(steerButton, "click", function () {
      if (canEditSelectedDraft()) submitSelectedDraft();
    });

    listen(documentObject, "click", function (event) {
      var row = event.target.closest(".row[data-session-id]");
      if (!row) return;
      selectComposerSession(row.dataset.sessionId, true);
    });

    function reconcileFleetSessions() {
      var rows = documentObject.querySelectorAll(".row[data-session-id]");
      var sessionIDs = Array.prototype.map.call(rows, function (row) { return row.dataset.sessionId; });
      var retainedSessionIDs = new Set(sessionIDs);
      statuses.forEach(function (_, sessionID) {
        if (!retainedSessionIDs.has(sessionID)) statuses.delete(sessionID);
      });
      controller.reconcileSessions(sessionIDs);
      if (selectedSessionID && !retainedSessionIDs.has(selectedSessionID)) {
        selectedSessionID = "";
        deckStatus.textContent = "";
        renderDraft(controller.draft(""));
      }
    }

    var MutationObserverClass = windowObject.MutationObserver;
    var fleetPanel = documentObject.getElementById("fleet-panel");
    if (fleetPanel && MutationObserverClass) {
      fleetObserver = new MutationObserverClass(reconcileFleetSessions);
      fleetObserver.observe(fleetPanel, {childList: true, subtree: true});
      reconcileFleetSessions();
    }
    var mainStack = documentObject.getElementById("main-stack");
    if (mainStack && MutationObserverClass) {
      capabilityObserver = new MutationObserverClass(syncEditControls);
      capabilityObserver.observe(mainStack, {
        childList: true,
        subtree: true,
        attributes: true,
        attributeFilter: ["data-can-steer", "data-session-id"]
      });
    }
    syncEditControls();

    function destroy() {
      if (destroyed) return;
      destroyed = true;
      if (fleetObserver) fleetObserver.disconnect();
      if (capabilityObserver) capabilityObserver.disconnect();
      while (listenerCleanups.length) listenerCleanups.pop()();
      controller.destroy();
    }
    listen(windowObject, "beforeunload", destroy);

    return {
      controller: controller,
      canEditSelectedDraft: canEditSelectedDraft,
      selectSession: selectComposerSession,
      submit: submitSelectedDraft,
      syncEditControls: syncEditControls,
      destroy: destroy
    };
  }

  return {autoSizeComposer: autoSizeComposer, bindComposer: bindComposer};
});

if (typeof module === "undefined" && typeof window !== "undefined" && typeof document !== "undefined") (function () {
  "use strict";

  /* -------- Accessible New Session modal -------- */
  window.KanediasSessionModal.bind(document, window.fetch.bind(window));

  var terminalUI = window.KanediasTerminalUI;
  var fleetLayout = window.KanediasFleetLayout.bind(document, window);
  var composerBinding = window.KanediasComposerUI.bindComposer(document, window);
  var submitSelectedDraft = composerBinding.submit;

  /* -------- Tab switching (delegated) -------- */
  document.addEventListener("click", function (e) {
    var tab = e.target.closest(".tab");
    if (!tab) return;
    var container = tab.closest(".tabs");
    if (!container) return;
    container.querySelectorAll(".tab").forEach(function (t) {
      t.classList.remove("active");
    });
    tab.classList.add("active");
    var name = tab.getAttribute("data-tab");
    document.querySelectorAll(".tabpane").forEach(function (p) {
      p.classList.remove("active");
    });
    var pane = document.querySelector('.tabpane[data-pane="' + name + '"]');
    if (pane) pane.classList.add("active");
  });

  /* -------- Search / filter (delegated) -------- */
  document.addEventListener("input", function (e) {
    if (e.target.id !== "search") return;
    var q = e.target.value.trim().toLowerCase();
    document.querySelectorAll(".row").forEach(function (row) {
      var hay = (
        (row.getAttribute("data-session-id") || "") +
        " " +
        (row.getAttribute("data-lifecycle") || "") +
        " " +
        (row.getAttribute("data-worker-type") || "")
      ).toLowerCase();
      var match = !q || hay.indexOf(q) !== -1;
      var container =
        row.closest("details") && row.parentElement.tagName === "SUMMARY"
          ? row.closest("details")
          : row;
      if (container === row) {
        row.style.display = match ? "" : "none";
      } else {
        var anyChild = Array.prototype.some.call(
          container.querySelectorAll(".row"),
          function (cr) {
            var h = (
              (cr.getAttribute("data-session-id") || "") +
              " " +
              (cr.getAttribute("data-lifecycle") || "")
            ).toLowerCase();
            return !q || h.indexOf(q) !== -1;
          }
        );
        container.style.display = anyChild ? "" : "none";
        if (q && anyChild) container.setAttribute("open", "");
      }
    });
  });

  /* -------- Pi-like keyboard decisions (delegated) -------- */
  var toolExpansion = terminalUI.createToolExpansionController();

  document.addEventListener("keydown", function (e) {
    var action = terminalUI.keyAction(e, {
      target: terminalUI.keyboardTarget(e.target),
      hasSelection: terminalUI.hasTextSelection(window, e.target),
      canInterrupt: terminalUI.detailCapability(document, "interrupt")
    });
    terminalUI.performAction(action, {
      event: e,
      document: document,
      Event: window.Event,
      tools: toolExpansion,
      submit: submitSelectedDraft
    });
  });

  /* -------- Transcript auto-scrolls to the newest content (TUI-like) -------- */
  (function () {
    var panel = document.getElementById("activity-panel");
    if (!panel) return;
    var shouldStick = true;
    var lastTranscript = null;

    // Highlight any newly-inserted tool args/result <code> blocks via the
    // sandboxed KanediasMarkdown.highlight, matching Task 1's markdown flow.
    // Only blocks that are not yet marked [data-highlighted] are processed.
    function highlightToolCode(root) {
      if (!window.KanediasMarkdown || typeof window.KanediasMarkdown.highlight !== "function") return;
      var nodes = root.querySelectorAll("[data-tool-code]:not([data-highlighted])");
      for (var i = 0; i < nodes.length; i++) {
        var el = nodes[i];
        var lang = el.getAttribute && el.getAttribute("data-language") ? el.getAttribute("data-language") : "";
        var html;
        try {
          html = window.KanediasMarkdown.highlight(el.textContent || "", lang);
        } catch (e) {
          continue;
        }
        try {
          el.innerHTML = html;
        } catch (e) {
          continue;
        }
        if (el.setAttribute) el.setAttribute("data-highlighted", "true");
      }
    }

    function stick() {
      var t = panel.querySelector(".transcript");
      if (!t) return;
      if (lastTranscript !== t) {
        lastTranscript = t;
        shouldStick = true;
        t.addEventListener("scroll", function () {
          shouldStick = t.scrollHeight - t.scrollTop - t.clientHeight < 40;
        });
      }
      if (shouldStick) t.scrollTop = t.scrollHeight;
    }
    // After every activity patch, render any newly-flagged Markdown and
    // highlight tool blocks before measuring for auto-scroll so the rendered
    // height is correct.
    function refresh() {
      toolExpansion.refresh(panel);
      if (window.KanediasMarkdown && window.KanediasMarkdown.renderPending) {
        window.KanediasMarkdown.renderPending(panel);
      }
      highlightToolCode(panel);
      stick();
    }
    new MutationObserver(refresh).observe(panel, { childList: true, subtree: true, characterData: true });
    refresh();
  })();

  function copyText(text, btn) {
    var done = function (ok) {
      var label = ok ? "copied" : "copy failed";
      var original = btn.getAttribute("data-copy-label") || "copy";
      btn.textContent = label;
      setTimeout(function () { btn.textContent = original; }, 1200);
    };
    var finish = function (ok) { done(ok); };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(
        function () { finish(true); },
        function () { fallbackCopy(text, finish); }
      );
    } else {
      fallbackCopy(text, finish);
    }
  }

  /* -------- Copy fenced code from .code-block (delegated) -------- */
  document.addEventListener("click", function (e) {
    var btn = e.target.closest("[data-copy-code]");
    if (!btn) return;
    e.stopPropagation();
    var block = btn.closest(".code-block");
    var code = block ? block.querySelector("code") : null;
    if (!code || typeof code.textContent !== "string") return;
    copyText(code.textContent, btn);
  });

  /* -------- Copy a tool card's args/result block (delegated) -------- */
  document.addEventListener("click", function (e) {
    var btn = e.target.closest("[data-copy-tool]");
    if (!btn) return;
    // Never let a copy click toggle the collapsed <details> tool card.
    e.stopPropagation();
    var section = btn.closest(".tool-section");
    var code = section ? section.querySelector("[data-tool-code]") : null;
    if (!code || typeof code.textContent !== "string") return;
    copyText(code.textContent, btn);
  });

  function fallbackCopy(text, finish) {
    var ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed";
    ta.style.top = "-1000px";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    var ok = false;
    try {
      ok = document.execCommand("copy");
    } catch (e) {
      ok = false;
    }
    document.body.removeChild(ta);
    finish(ok);
  }

  /* -------- Detail stream is authoritative for deck capabilities -------- */
  function disarmSessionActions() {
    terminalUI.setActionControlState(document.querySelector(".deck-input"), false);
    terminalUI.setActionControlState(document.getElementById("steerBtn"), false);
    terminalUI.setActionControlState(document.getElementById("attach-images-button"), false);
    terminalUI.setActionControlState(document.getElementById("image-file-input"), false);
    terminalUI.setActionControlState(document.getElementById("interruptBtn"), false);
    terminalUI.setActionControlState(document.querySelector(".dbtn.stop"), false);
  }

  document.addEventListener("click", function (e) {
    var row = e.target.closest(".row");
    if (!row) return;
    disarmSessionActions();
  });

  terminalUI.observeDeckCapabilities(document, MutationObserver);

  /* -------- Deck status success auto-clears after 2000ms; errors persist -------- */
  (function () {
    var deckStatus = document.getElementById("deck-status");
    if (!deckStatus) return;
    var deckStatusController = terminalUI.createDeckStatusController({ delay: 2000 });
    var observer = new MutationObserver(function () {
      deckStatusController.schedule(document);
    });
    observer.observe(deckStatus, {
      childList: true,
      subtree: true,
      attributes: true,
      attributeFilter: ["data-success-id"]
    });
    // Handle a success already present before the observer was installed.
    deckStatusController.schedule(document);
  })();

  /* -------- Alert banner jumps to first question (delegated) -------- */
  document.addEventListener("click", function (e) {
    if (!e.target.closest("#alertBanner")) return;
    var firstQ = document.querySelector(".row[data-lifecycle='question']");
    if (firstQ) {
      fleetLayout.show();
      var d = firstQ.closest("details");
      while (d) {
        d.setAttribute("open", "");
        d = d.parentElement ? d.parentElement.closest("details") : null;
      }
      firstQ.scrollIntoView({ block: "center", behavior: "smooth" });
    }
  });
})();
