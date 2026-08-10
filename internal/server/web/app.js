(function (root, factory) {
  "use strict";
  var api = factory();
  if (typeof module === "object" && module.exports) module.exports = api;
  if (root) root.KanediasComposerUI = api;
})(typeof globalThis !== "undefined" ? globalThis : this, function () {
  "use strict";

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

    tray.classList.add("image-attachment-tray");
    tray.setAttribute("aria-label", "Image attachments");

    function formatBytes(bytes) {
      if (bytes < 1024) return bytes + " B";
      if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(bytes < 10 * 1024 ? 1 : 0) + " KiB";
      return (bytes / (1024 * 1024)).toFixed(1) + " MiB";
    }

    function canEditSelectedDraft() {
      return !!selectedSessionID &&
        terminalUI.detailCapability(documentObject, "steer") &&
        !controller.draft(selectedSessionID).busy;
    }

    function syncEditControls() {
      terminalUI.syncDeckState(documentObject);
      var canEdit = canEditSelectedDraft();
      input.disabled = !canEdit;
      var removeButtons = tray.querySelectorAll("[data-remove-image]");
      for (var i = 0; i < removeButtons.length; i++) removeButtons[i].disabled = !canEdit;
    }

    function renderDraft(snapshot) {
      if (!snapshot || snapshot.sessionID !== selectedSessionID) return;
      if (input.value !== snapshot.text) input.value = snapshot.text;

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

      tray.hidden = snapshot.images.length === 0;
      appShell.classList.toggle("has-image-draft", snapshot.images.length > 0);
      deck.setAttribute("aria-busy", snapshot.busy ? "true" : "false");
      syncEditControls();
    }

    controller = windowObject.KanediasImageAttachments.createController({
      fetch: windowObject.fetch.bind(windowObject),
      FormData: windowObject.FormData,
      createObjectURL: windowObject.URL.createObjectURL.bind(windowObject.URL),
      revokeObjectURL: windowObject.URL.revokeObjectURL.bind(windowObject.URL),
      onChange: renderDraft,
      onStatus: function (message, sessionID) {
        if (sessionID === "" || sessionID === selectedSessionID) deckStatus.textContent = message;
      }
    });

    function selectComposerSession(sessionID, revokeCapability) {
      if (selectedSessionID && canEditSelectedDraft()) controller.setText(input.value);
      if (revokeCapability) {
        var detail = documentObject.getElementById("detail-panel");
        if (detail) detail.setAttribute("data-can-steer", "false");
      }
      selectedSessionID = sessionID || "";
      renderDraft(controller.selectSession(selectedSessionID));
    }

    function submitSelectedDraft() {
      var capturedSessionID = selectedSessionID;
      if (!canEditSelectedDraft()) {
        if (!capturedSessionID) controller.submit("");
        return;
      }
      controller.setText(input.value);
      controller.submit(capturedSessionID);
    }

    input.addEventListener("input", function () {
      if (canEditSelectedDraft()) controller.setText(input.value);
    });

    attachButton.addEventListener("click", function () {
      if (canEditSelectedDraft()) fileInput.click();
    });

    fileInput.addEventListener("change", function () {
      try {
        if (canEditSelectedDraft()) controller.stageFiles(fileInput.files);
      } finally {
        fileInput.value = "";
      }
    });

    input.addEventListener("paste", function (event) {
      var items = event.clipboardData && event.clipboardData.items;
      if (!items || !canEditSelectedDraft()) return;
      var images = [];
      for (var i = 0; i < items.length; i++) {
        if (items[i].kind !== "file" || String(items[i].type || "").indexOf("image/") !== 0) continue;
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

    deck.addEventListener("dragenter", function (event) {
      if (!hasFiles(event.dataTransfer) || !canEditSelectedDraft()) return;
      event.preventDefault();
      dragDepth++;
      deck.classList.add("drop-active");
    });
    deck.addEventListener("dragover", function (event) {
      if (!hasFiles(event.dataTransfer) || !canEditSelectedDraft()) return;
      event.preventDefault();
      if (event.dataTransfer) event.dataTransfer.dropEffect = "copy";
    });
    deck.addEventListener("dragleave", function (event) {
      if (!hasFiles(event.dataTransfer) && dragDepth === 0) return;
      event.preventDefault();
      dragDepth = Math.max(0, dragDepth - 1);
      if (dragDepth === 0) deck.classList.remove("drop-active");
    });
    deck.addEventListener("drop", function (event) {
      if (!hasFiles(event.dataTransfer)) return;
      event.preventDefault();
      dragDepth = 0;
      deck.classList.remove("drop-active");
      if (canEditSelectedDraft()) controller.stageFiles(event.dataTransfer.files);
    });

    tray.addEventListener("click", function (event) {
      var remove = event.target.closest("[data-remove-image]");
      if (!remove || !canEditSelectedDraft()) return;
      controller.removeImage(remove.getAttribute("data-remove-image"));
    });

    steerButton.addEventListener("click", function () {
      if (canEditSelectedDraft()) submitSelectedDraft();
    });

    documentObject.addEventListener("click", function (event) {
      var row = event.target.closest(".row[data-session-id]");
      if (!row) return;
      selectComposerSession(row.dataset.sessionId, true);
    });

    function reconcileFleetSessions() {
      var rows = documentObject.querySelectorAll(".row[data-session-id]");
      var sessionIDs = Array.prototype.map.call(rows, function (row) { return row.dataset.sessionId; });
      controller.reconcileSessions(sessionIDs);
      if (selectedSessionID && sessionIDs.indexOf(selectedSessionID) === -1) {
        selectedSessionID = "";
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
        attributeFilter: ["data-can-steer"]
      });
    }
    syncEditControls();

    function destroy() {
      if (fleetObserver) fleetObserver.disconnect();
      if (capabilityObserver) capabilityObserver.disconnect();
      controller.destroy();
    }
    windowObject.addEventListener("beforeunload", destroy);

    return {
      controller: controller,
      canEditSelectedDraft: canEditSelectedDraft,
      selectSession: selectComposerSession,
      submit: submitSelectedDraft,
      syncEditControls: syncEditControls,
      destroy: destroy
    };
  }

  return {bindComposer: bindComposer};
});

if (typeof window !== "undefined" && typeof document !== "undefined") (function () {
  "use strict";

  /* -------- Accessible New Session modal -------- */
  window.KanediasSessionModal.bind(document, window.fetch.bind(window));

  var terminalUI = window.KanediasTerminalUI;
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

  /* -------- Mobile slide-over (delegated) -------- */
  document.addEventListener("click", function (e) {
    var sidebar = document.getElementById("sidebar");
    var scrim = document.getElementById("scrim");
    if (!sidebar || !scrim) return;

    if (e.target.id === "menuBtn" || e.target.closest("#menuBtn")) {
      if (sidebar.classList.contains("open")) {
        closeSheet(sidebar, scrim);
      } else {
        openSheet(sidebar, scrim);
      }
      return;
    }
    if (e.target === scrim || e.target.closest("#scrim") === scrim) {
      closeSheet(sidebar, scrim);
    }
  });

  function openSheet(sidebar, scrim) {
    sidebar.classList.add("open");
    scrim.classList.add("show");
    var menuBtn = document.getElementById("menuBtn");
    if (menuBtn) menuBtn.setAttribute("aria-expanded", "true");
  }

  function closeSheet(sidebar, scrim) {
    sidebar.classList.remove("open");
    scrim.classList.remove("show");
    var menuBtn = document.getElementById("menuBtn");
    if (menuBtn) menuBtn.setAttribute("aria-expanded", "false");
  }

  function closeSheetIfMobile(sidebar, scrim) {
    if (window.matchMedia("(max-width:820px)").matches) closeSheet(sidebar, scrim);
  }

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
      var d = firstQ.closest("details");
      while (d) {
        d.setAttribute("open", "");
        d = d.parentElement ? d.parentElement.closest("details") : null;
      }
      var sidebar = document.getElementById("sidebar");
      var scrim = document.getElementById("scrim");
      if (sidebar && scrim) openSheet(sidebar, scrim);
      firstQ.scrollIntoView({ block: "center", behavior: "smooth" });
    }
  });
})();
