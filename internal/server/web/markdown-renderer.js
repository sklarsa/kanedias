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

  // Allow-list for URL targets in links and images. An optional leading scheme
  // is only permitted when it is http, https, mailto, tel, or ftp; every other
  // scheme (javascript:, data:, vbscript:, …) is rejected, including control-
  // character obfuscations which are stripped first. Scheme-less URLs (relative
  // paths, fragments, plain text) are permitted, matching Pi's sanitizer.
  function sanitizeURL(value) {
    if (typeof value !== "string") return null;
    var url = value.replace(/[\u0000-\u001f\u007f-\u009f]/g, "").trim();
    if (url === "") return null;
    var scheme = /^([A-Za-z][A-Za-z0-9+.-]*):/.exec(url);
    if (scheme && !/^(https?|mailto|tel|ftp)$/i.test(scheme[1])) {
      return null;
    }
    return url;
  }

  // Strict strikethrough tokenizer matching Pi: an opening and closing double
  // tilde with non-tilde/space boundaries, lexing the inner span as inline
  // tokens so nested formatting survives.
  var strictStrikethroughRegex = /^(~~)(?=[^\s~])((?:\\.|[^\\])*?(?:\\.|[^\s~\\]))\1(?=[^~]|$)/;

  function SafeTokenizer() {
    if (marked && marked.Tokenizer) {
      var parent = marked.Tokenizer;
      var proto = Object.create(parent.prototype);
      // HTML-like input is treated as plain text (literal) so it can never
      // become a live element; this matches Pi's TUI renderer.
      proto.html = function () { return undefined; };
      proto.tag = function () { return undefined; };
      proto.del = function (src) {
        var match = strictStrikethroughRegex.exec(src);
        if (!match) return undefined;
        return {
          type: "del",
          raw: match[0],
          text: match[2],
          tokens: this.lexer.inlineTokens(match[2])
        };
      };
      proto.constructor = SafeTokenizer;
      return proto;
    }
    return {};
  }

  function escapeText(s) {
    return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  }
  // Full HTML/attribute escaper: also escapes double quotes and apostrophes so
  // text placed inside a quoted attribute can never break out of it.
  function escapeAttr(s) {
    return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;").replace(/'/g, "&#39;");
  }

  var renderer = {
    // Fenced code becomes a copy-able, scrollable block highlighted with hljs.
    // Known languages are highlighted directly; absent or unregistered languages
    // fall back to Pi-style auto-detection, with an escaped fallback on failure.
    code: function (token) {
      var lang = token.lang;
      var body = token.text || "";
      var highlighted = null;
      try {
        if (lang && hljs.getLanguage(lang)) {
          highlighted = hljs.highlight(body, { language: lang }).value;
        } else {
          highlighted = hljs.highlightAuto(body).value;
        }
      } catch (e) {
        highlighted = null;
      }
      if (!highlighted) highlighted = escapeText(body);
      return '<div class="code-block"><button type="button" class="copy-btn" data-copy-code>copy</button>' +
        '<pre><code class="hljs">' + highlighted + '</code></pre></div>';
    },
    link: function (token) {
      var clean = sanitizeURL(token.href);
      // Render the label through parsed inline tokens so nested HTML-like input
      // stays literal and can never be emitted as an active element.
      var label = this.parser.parseInline(token.tokens);
      if (!clean) return label;
      var out = '<a href="' + escapeAttr(clean) + '"';
      if (token.title) out += ' title="' + escapeAttr(token.title) + '"';
      out += ' rel="noopener noreferrer">' + label + '</a>';
      return out;
    },
    image: function (token) {
      var clean = sanitizeURL(token.href);
      if (!clean) return escapeAttr(token.text || "");
      var out = '<img src="' + escapeAttr(clean) + '" alt="' + escapeAttr(token.text || "") + '"';
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
    var nodes = root.querySelectorAll("[data-markdown]:not([data-markdown-rendered]):not([data-markdown-error])");
    for (var i = 0; i < nodes.length; i++) {
      var el = nodes[i];
      // Defense in depth: never re-process an element that already failed, so
      // the rendering failure cannot repeatedly retrigger the activity observer.
      if (el.getAttribute && el.getAttribute("data-markdown-error") === "true") continue;
      var original = el.textContent || "";
      var html;
      try {
        html = render(original);
      } catch (e) {
        markError(el, original);
        continue;
      }
      try {
        el.innerHTML = html;
      } catch (e) {
        markError(el, original);
        continue;
      }
      if (el.setAttribute) el.setAttribute("data-markdown-rendered", "true");
    }
  }

  function markError(el, original) {
    if (el.textContent !== original) el.textContent = original;
    if (el.setAttribute) el.setAttribute("data-markdown-error", "true");
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
