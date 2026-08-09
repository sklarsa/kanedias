(function (root, factory) {
  "use strict";
  var api = factory();
  if (typeof module === "object" && module.exports) module.exports = api;
  if (root) root.KanediasTerminalUI = api;
})(typeof globalThis !== "undefined" ? globalThis : this, function () {
  "use strict";

  function keyAction(event, context) {
    context = context || {};
    if (!event || event.isComposing || event.altKey || event.metaKey || event.shiftKey) return null;
    if (context.target === "other-editable") return null;

    var key = typeof event.key === "string" ? event.key.toLowerCase() : "";
    var inConsole = context.target === "deck" || context.target === "body";

    if (!event.ctrlKey && event.key === "Enter" && context.target === "deck") return "submit";
    if (!event.ctrlKey && event.key === "Escape" && inConsole && context.canInterrupt) return "interrupt";
    if (!event.ctrlKey || !inConsole) return null;
    if (key === "a") return "line-start";
    if (key === "c") return context.hasSelection ? null : "clear";
    if (key === "o") return "toggle-tools";
    return null;
  }

  function nextToolExpansion(openStates) {
    return openStates.length === 0 || !openStates.every(Boolean);
  }

  return {
    keyAction: keyAction,
    nextToolExpansion: nextToolExpansion
  };
});
