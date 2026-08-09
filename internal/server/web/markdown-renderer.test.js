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
