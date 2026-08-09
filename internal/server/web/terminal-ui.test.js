const test = require("node:test");
const assert = require("node:assert/strict");
const ui = require("./terminal-ui.js");

const event = (key, extra = {}) => ({ key, ctrlKey: false, altKey: false, metaKey: false, shiftKey: false, isComposing: false, ...extra });

test("matches Pi editor and interrupt keys without stealing copy", () => {
  assert.equal(ui.keyAction(event("a", {ctrlKey:true}), {target:"deck", hasSelection:false, canInterrupt:false}), "line-start");
  assert.equal(ui.keyAction(event("c", {ctrlKey:true}), {target:"deck", hasSelection:true, canInterrupt:false}), null);
  assert.equal(ui.keyAction(event("c", {ctrlKey:true}), {target:"deck", hasSelection:false, canInterrupt:false}), "clear");
  assert.equal(ui.keyAction(event("Escape"), {target:"deck", hasSelection:false, canInterrupt:true}), "interrupt");
  assert.equal(ui.keyAction(event("Escape"), {target:"deck", hasSelection:false, canInterrupt:false}), null);
  assert.equal(ui.keyAction(event("o", {ctrlKey:true}), {target:"body", hasSelection:false, canInterrupt:false}), "toggle-tools");
});

test("ignores composition, conflicting modifiers, and unrelated editors", () => {
  const deck = {target:"deck", hasSelection:false, canInterrupt:true};
  assert.equal(ui.keyAction(event("Enter"), deck), "submit");
  assert.equal(ui.keyAction(event("Enter"), {...deck, target:"body"}), null);
  assert.equal(ui.keyAction(event("a", {ctrlKey:true, isComposing:true}), deck), null);
  assert.equal(ui.keyAction(event("a", {ctrlKey:true, shiftKey:true}), deck), null);
  assert.equal(ui.keyAction(event("c", {ctrlKey:true}), {...deck, target:"other-editable"}), null);
  assert.equal(ui.keyAction(event("c", {ctrlKey:true}), {...deck, target:"body"}), "clear");
  assert.equal(ui.keyAction(event("o", {ctrlKey:true}), {...deck, target:"other-editable"}), null);
});

test("global tool toggle expands unless every card is open", () => {
  assert.equal(ui.nextToolExpansion([]), true);
  assert.equal(ui.nextToolExpansion([true, false]), true);
  assert.equal(ui.nextToolExpansion([true, true]), false);
});
