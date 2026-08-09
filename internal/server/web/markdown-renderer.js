/* KanediasMarkdown — safe, Pi-compatible Markdown rendering.
 *
 * A UMD module. In CommonJS (Node tests) it requires the two vendored browser
 * libraries directly. In the browser it consumes the globals they install:
 * globalThis.marked (from marked.min.js) and globalThis.hljs (from
 * highlight.min.js). The three scripts are loaded in order in index.html before
 * the single app module, so globals are guaranteed available here.
 *
 * The renderer is deliberately sandboxed: raw HTML is escaped literally, active
 * URL schemes (javascript:, data:, vbscript:) are dropped, link/image targets
 * pass an allow-list, and fenced code is highlighted with a copy button.
 */
(function (root, factory) {
  if (typeof module === "object" && module.exports) {
    module.exports = factory(require("./marked.min.js"), require("./highlight.min.js"));
  } else {
    root.KanediasMarkdown = factory(root.marked, root.hljs);
  }
})(typeof globalThis !== "undefined" ? globalThis : this, function (marked, hljs) {
  "use strict";

  // Allow-list for URL targets in links and images. Relative paths (starting
  // with # or /) and plain-text data are permitted; every active/remote-exec
  // scheme and control characters are rejected.
  function sanitizeURL(value) {
    if (typeof value !== "string") return null;
    var url = value.replace(/[\u0000-\u001f\u007f-\u009f]/g, "").trim();
    if (url === "") return null;
    if (/^(https?:|mailto:|tel:|data:text\/plain)/i.test(url)) return url;
    if (/^[#/]/.test(url)) return url;
    return null;
  }

  // Strict strikethrough tokenizer: only the "~~text~~" form is recognized, so
  // a lone tilde can never produce accidental formatting.
  function SafeTokenizer() {
    if (marked && marked.Tokenizer) {
      var parent = marked.Tokenizer;
      var proto = Object.create(parent.prototype);
      proto.del = function (src) {
        var cap = /^~~(?=\S)([\s\S]*?\S)~~/.exec(src);
        if (cap) {
          var text = cap[1];
          return { type: "del", raw: cap[0], text: text, tokens: [{ type: "text", raw: text, text: text }] };
        }
        return undefined;
      };
      proto.constructor = SafeTokenizer;
      return proto;
    }
    return {};
  }

  function escapeText(s) {
    return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  }
  function escapeAttr(s) {
    return String(s).replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  }

  var renderer = {
    // Fenced code becomes a copy-able, scrollable block highlighted with hljs.
    code: function (token) {
      var lang = token.lang || "";
      var body = token.text || "";
      var highlighted = null;
      try {
        if (lang && hljs.getLanguage(lang)) {
          highlighted = hljs.highlight(body, { language: lang }).value;
        } else if (!lang) {
          highlighted = hljs.highlightAuto(body).value;
        }
      } catch (e) {
        highlighted = null;
      }
      if (!highlighted) highlighted = escapeText(body);
      return '<div class="code-block"><button type="button" class="copy-btn" data-copy-code>copy</button>' +
        '<pre><code class="hljs">' + highlighted + '</code></pre></div>';
    },
    // Raw HTML is rendered literally (escaped) and can never become a live element.
    html: function (token) {
      return escapeText(token.text || token.raw || "");
    },
    link: function (token) {
      var clean = sanitizeURL(token.href);
      if (!clean) return escapeText(token.text);
      var out = '<a href="' + escapeAttr(clean) + '"';
      if (token.title) out += ' title="' + escapeAttr(token.title) + '"';
      out += ' rel="noopener noreferrer">' + token.text + '</a>';
      return out;
    },
    image: function (token) {
      var clean = sanitizeURL(token.href);
      if (!clean) return escapeText(token.text);
      var out = '<img src="' + escapeAttr(clean) + '" alt="' + escapeText(token.text) + '"';
      if (token.title) out += ' title="' + escapeAttr(token.title) + '"';
      out += ">";
      return out;
    },
    codespan: function (token) {
      return "<code>" + escapeText(token.text) + "</code>";
    },
  };

  if (marked && marked.use) {
    marked.use({ tokenizer: new SafeTokenizer(), renderer: renderer, breaks: true, gfm: true });
  }

  function render(text) {
    if (!marked || typeof marked.parse !== "function") return escapeText(String(text));
    return marked.parse(String(text));
  }

  function renderPending(root) {
    if (!root || typeof root.querySelectorAll !== "function") return;
    var nodes = root.querySelectorAll("[data-markdown]:not([data-markdown-rendered])");
    for (var i = 0; i < nodes.length; i++) {
      var el = nodes[i];
      var original = el.textContent;
      try {
        el.innerHTML = render(original);
        el.setAttribute("data-markdown-rendered", "true");
      } catch (e) {
        el.textContent = original;
        el.setAttribute("data-markdown-error", "true");
      }
    }
  }

  function highlight(code, language) {
    var body = String(code);
    try {
      if (language && hljs.getLanguage(language)) {
        return hljs.highlight(body, { language: language }).value;
      }
      return hljs.highlightAuto(body).value;
    } catch (e) {
      return escapeText(body);
    }
  }

  return { render: render, renderPending: renderPending, sanitizeURL: sanitizeURL, highlight: highlight };
});
