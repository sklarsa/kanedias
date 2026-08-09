(function (root, factory) {
  "use strict";
  var api = factory();
  if (typeof module === "object" && module.exports) module.exports = api;
  if (root) root.KanediasTerminalUI = api;
})(typeof globalThis !== "undefined" ? globalThis : this, function () {
  "use strict";

  var capabilityAttributes = ["data-can-steer", "data-can-interrupt", "data-can-stop"];

  function keyAction(event, context) {
    context = context || {};
    if (!event || event.isComposing || event.altKey || event.metaKey || event.shiftKey) return null;
    if (context.target === "other-editable") return null;

    var key = typeof event.key === "string" ? event.key.toLowerCase() : "";
    var inConsole = context.target === "deck" || context.target === "body";

    if (!event.ctrlKey && event.key === "Enter" && context.target === "deck") return "submit";
    if (!event.ctrlKey && event.key === "Escape" && inConsole && context.canInterrupt) return "interrupt";
    if (!event.ctrlKey || !inConsole) return null;
    if (key === "a") return context.target === "deck" ? "line-start" : null;
    if (key === "c") return context.hasSelection ? null : "clear";
    if (key === "o") return "toggle-tools";
    return null;
  }

  function nextToolExpansion(openStates) {
    return openStates.length === 0 || !openStates.every(Boolean);
  }

  function hasTextSelection(windowObject, target) {
    var selection = windowObject && windowObject.getSelection && windowObject.getSelection();
    if (selection && !selection.isCollapsed) return true;
    if (target && (target.tagName === "INPUT" || target.tagName === "TEXTAREA") &&
        typeof target.selectionStart === "number" && typeof target.selectionEnd === "number") {
      return target.selectionStart !== target.selectionEnd;
    }
    return false;
  }

  function keyboardTarget(target) {
    if (target && target.closest && target.closest(".deck-input")) return "deck";
    if (target && target.closest && target.closest("input, textarea, select, button, [contenteditable]:not([contenteditable='false'])")) {
      return "other-editable";
    }
    return "body";
  }

  function detailCapability(documentObject, name) {
    var detail = documentObject.getElementById("detail-panel");
    return !!detail && detail.getAttribute("data-can-" + name) === "true";
  }

  function setActionControlState(control, enabled) {
    if (!control) return;
    control.disabled = !enabled;
    control.setAttribute("aria-disabled", enabled ? "false" : "true");
    control.classList.toggle("armed", enabled);
  }

  function syncDeckState(documentObject) {
    setActionControlState(documentObject.getElementById("steerBtn"), detailCapability(documentObject, "steer"));
    setActionControlState(documentObject.getElementById("interruptBtn"), detailCapability(documentObject, "interrupt"));
    setActionControlState(documentObject.querySelector(".dbtn.stop"), detailCapability(documentObject, "stop"));
  }

  function observeDeckCapabilities(documentObject, MutationObserverClass) {
    var mainStack = documentObject.getElementById("main-stack");
    syncDeckState(documentObject);
    if (!mainStack || !MutationObserverClass) return null;
    var observer = new MutationObserverClass(function () {
      syncDeckState(documentObject);
    });
    observer.observe(mainStack, {
      childList: true,
      subtree: true,
      attributes: true,
      attributeFilter: capabilityAttributes.slice()
    });
    return observer;
  }

  function clearDeck(input, EventClass) {
    if (!input) return;
    input.value = "";
    input.dispatchEvent(new EventClass("input", {bubbles: true}));
    input.focus();
  }

  function createToolExpansionController() {
    var expansionMode = null;

    function refresh(root) {
      if (expansionMode === null) return;
      var cards = root.querySelectorAll("[data-tool-card]");
      for (var i = 0; i < cards.length; i++) {
        if (cards[i].open !== expansionMode) cards[i].open = expansionMode;
      }
    }

    function toggle(root) {
      var cards = Array.prototype.slice.call(root.querySelectorAll("[data-tool-card]"));
      expansionMode = nextToolExpansion(cards.map(function (card) { return card.open; }));
      refresh(root);
      return expansionMode;
    }

    return {
      mode: function () { return expansionMode; },
      refresh: refresh,
      toggle: toggle
    };
  }

  function performAction(action, context) {
    if (!action) return false;
    var event = context.event;
    var documentObject = context.document;
    event.preventDefault();

    if (action === "submit") {
      var steer = documentObject.getElementById("steerBtn");
      if (steer && !steer.disabled) {
        steer.click();
        clearDeck(documentObject.querySelector(".deck-input"), context.Event);
      }
      return true;
    }
    if (action === "line-start") {
      var input = documentObject.querySelector(".deck-input");
      if (input) {
        input.focus();
        input.setSelectionRange(0, 0);
      }
      return true;
    }
    if (action === "clear") {
      clearDeck(documentObject.querySelector(".deck-input"), context.Event);
      return true;
    }
    if (action === "interrupt") {
      var interrupt = documentObject.getElementById("interruptBtn");
      if (interrupt && !interrupt.disabled) interrupt.click();
      return true;
    }
    if (action === "toggle-tools") {
      if (context.tools) context.tools.toggle(documentObject);
      return true;
    }
    return false;
  }

  return {
    keyAction: keyAction,
    nextToolExpansion: nextToolExpansion,
    hasTextSelection: hasTextSelection,
    keyboardTarget: keyboardTarget,
    detailCapability: detailCapability,
    setActionControlState: setActionControlState,
    syncDeckState: syncDeckState,
    observeDeckCapabilities: observeDeckCapabilities,
    createToolExpansionController: createToolExpansionController,
    performAction: performAction
  };
});
