const test = require("node:test");
const assert = require("node:assert/strict");
const renderer = require("./markdown-renderer.js");

test("renders GFM and highlighted fenced code", () => {
  const html = renderer.render("# Title\n\n| A | B |\n|---|---|\n| 1 | 2 |\n\n```js\nconst n = 1;\n```");
  assert.match(html, /<h1>Title<\/h1>/);
  assert.match(html, /<table>/);
  assert.match(html, /class="hljs"/);
  assert.match(html, /hljs-keyword/);
});

test("renders raw HTML literally and rejects active URLs", () => {
  const html = renderer.render('<img src=x onerror=alert(1)> [bad](javascript:alert(1)) [ok](https://example.com)');
  assert.doesNotMatch(html, /<img\b/i);
  assert.doesNotMatch(html, /href="javascript:/i);
  assert.match(html, /&lt;img src=x onerror=alert/);
  assert.match(html, /href="https:\/\/example.com"/);
  assert.match(html, /rel="noopener noreferrer"/);
  assert.equal(renderer.sanitizeURL("java\u0000script:alert(1)"), null);
});

test("safe link labels cannot carry active HTML (nested <img onerror>)", () => {
  const html = renderer.render('[<img src=x onerror=alert(1)>](https://example.com)');
  assert.doesNotMatch(html, /<img\b/i);
  assert.doesNotMatch(html, /<a[^>]*><img/i);
  assert.match(html, /&lt;img src=x onerror=alert\(1\)&gt;/);
  assert.match(html, /<a href="https:\/\/example.com"/);
});

test("image alt text is attribute-escaped (no onerror injection)", () => {
  const html = renderer.render('![x" onerror="alert(1)](https://example.com)');
  assert.doesNotMatch(html, /onerror\s*=\s*["']/);
  assert.match(html, /alt="x&quot; onerror=&quot;alert\(1\)"/);
  assert.match(html, /src="https:\/\/example.com"/);
});

test("sanitizeURL allows http/https/mailto/tel/ftp and relative; rejects data/obfuscated", () => {
  for (const s of ["http://a", "https://a", "mailto:a@b", "tel:+1-800", "ftp://a/file"]) {
    assert.equal(renderer.sanitizeURL(s), s, s);
  }
  for (const s of ["data:text/plain,hello", "data:image/png;base64,AA", "javascript:alert(1)", "vbscript:x", "java\u0000script:alert(1)"]) {
    assert.equal(renderer.sanitizeURL(s), null, s);
  }
  for (const s of ["./relative", "../up", "plaintext", "#anchor", "/root"]) {
    assert.equal(renderer.sanitizeURL(s), s, s);
  }
});

test("strict strikethrough rejects triple-tilde adjacency", () => {
  const html = renderer.render("a ~~~x~~~ b");
  assert.doesNotMatch(html, /<del>/);
  assert.match(html, /~~~x~~~/);
});

test("strict strikethrough lexes nested inline tokens", () => {
  const html = renderer.render("c ~~**bold**~~ d");
  assert.match(html, /<del><strong>bold<\/strong><\/del>/);
  assert.doesNotMatch(html, /<del>\*\*bold\*\*<\/del>/);
});

test("unknown fenced language auto-detects via highlightAuto", () => {
  const html = renderer.render("```noSuchLang\nconst n = 1;\n```");
  assert.match(html, /class="hljs"/);
  assert.match(html, /hljs-(keyword|type)/);
});

test("exported highlight interface highlights known and auto-detects unknown languages", () => {
  assert.match(renderer.highlight("const n = 1;", "js"), /hljs-keyword/);
  assert.match(renderer.highlight("<b>x</b>", "xml"), /hljs-tag/);
  assert.match(renderer.highlight("const n = 1;", "noSuchLang"), /hljs-(keyword|type)/);
});

test("renderPending marks a failure and does not retry the errored node", () => {
  const attrs = {};
  let innerSetCalls = 0;
  const node = {
    textContent: "original text",
    setAttribute(k, v) { attrs[k] = v; },
    getAttribute(k) { return attrs[k] === "true" ? "true" : null; },
  };
  Object.defineProperty(node, "innerHTML", {
    get() { return ""; },
    set() { innerSetCalls++; throw new Error("dom reject"); },
    configurable: true,
  });
  const root = { querySelectorAll() { return [node]; } };

  renderer.renderPending(root);
  assert.equal(node.textContent, "original text", "text is preserved on failure");
  assert.equal(attrs["data-markdown-error"], "true", "failure is marked");
  assert.equal(attrs["data-markdown-rendered"], undefined, "not marked rendered");
  assert.equal(innerSetCalls, 1, "one mutation attempt on first pass");

  // A second observer cycle must skip the already-errored node.
  renderer.renderPending(root);
  assert.equal(innerSetCalls, 1, "errored node is not retried");
});
