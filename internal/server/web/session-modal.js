(function (root, factory) {
  "use strict";
  if (typeof module === "object" && module.exports) {
    module.exports = factory();
  } else {
    root.KanediasSessionModal = factory();
  }
})(typeof self !== "undefined" ? self : this, function () {
  "use strict";

  var BINDING_KEY = "__kanediasSessionModalBinding";
  var MAX_RESPONSE_BYTES = 64 * 1024;
  var MAX_ERROR_CHARS = 300;
  var GENERIC_ERROR = "The session could not be launched. Please try again.";

  function selectedOption(modelSelect) {
    if (!modelSelect || modelSelect.selectedIndex < 0) return null;
    return modelSelect.options[modelSelect.selectedIndex] || null;
  }

  function levelsFor(modelSelect) {
    var option = selectedOption(modelSelect);
    if (!option) return [];
    return (option.getAttribute("data-thinking-levels") || "")
      .split(",")
      .map(function (level) { return level.trim(); })
      .filter(Boolean);
  }

  function defaultThinkingFor(modelSelect) {
    var option = selectedOption(modelSelect);
    return option ? option.getAttribute("data-default-thinking") || "" : "";
  }

  function rebuildThinking(documentObject, modelSelect, thinkingSelect) {
    var levels = levelsFor(modelSelect);
    var current = thinkingSelect.value;
    var fallback = defaultThinkingFor(modelSelect);
    var choices = levels.map(function (level) {
      var option = documentObject.createElement("option");
      option.value = level;
      option.textContent = level;
      return option;
    });
    thinkingSelect.replaceChildren.apply(thinkingSelect, choices);
    if (levels.indexOf(current) !== -1) {
      thinkingSelect.value = current;
    } else if (levels.indexOf(fallback) !== -1) {
      thinkingSelect.value = fallback;
    } else if (levels.length) {
      thinkingSelect.value = levels[0];
    }
    thinkingSelect.disabled = levels.length <= 1;
    return levels.slice();
  }

  function selection(modelSelect, thinkingSelect) {
    return {
      modelType: modelSelect.value,
      thinkingLevel: thinkingSelect.value
    };
  }

  function workerSelection(row) {
    var result = selection(
      row.querySelector("[data-worker-model]"),
      row.querySelector("[data-worker-thinking]")
    );
    result.workerType = row.getAttribute("data-worker-type") || "";
    return {
      workerType: result.workerType,
      modelType: result.modelType,
      thinkingLevel: result.thinkingLevel
    };
  }

  function buildRequest(dialog) {
    return {
      root: selection(
        dialog.querySelector("[data-root-model]"),
        dialog.querySelector("[data-root-thinking]")
      ),
      workers: Array.from(dialog.querySelectorAll("[data-worker-row]")).map(workerSelection)
    };
  }

  function sanitizedError(value) {
    if (typeof value !== "string") return GENERIC_ERROR;
    var clean = value.replace(/[\u0000-\u001f\u007f]+/g, " ").replace(/\s+/g, " ").trim();
    if (!clean) return GENERIC_ERROR;
    return clean.slice(0, MAX_ERROR_CHARS);
  }

  function textBytes(text) {
    if (typeof TextEncoder !== "undefined") return new TextEncoder().encode(text).byteLength;
    if (typeof Blob !== "undefined") return new Blob([text]).size;
    return MAX_RESPONSE_BYTES + 1;
  }

  function boundedText(response) {
    var contentLength = response.headers && response.headers.get
      ? Number(response.headers.get("content-length"))
      : 0;
    if (contentLength > MAX_RESPONSE_BYTES) return Promise.reject(new Error("response too large"));

    if (response.body && typeof response.body.getReader === "function" && typeof TextDecoder !== "undefined") {
      var reader = response.body.getReader();
      var chunks = [];
      var total = 0;
      function readChunk() {
        return reader.read().then(function (result) {
          if (result.done) {
            var merged = new Uint8Array(total);
            var offset = 0;
            chunks.forEach(function (chunk) {
              merged.set(chunk, offset);
              offset += chunk.byteLength;
            });
            return new TextDecoder().decode(merged);
          }
          total += result.value.byteLength;
          if (total > MAX_RESPONSE_BYTES) {
            reader.cancel();
            throw new Error("response too large");
          }
          chunks.push(result.value);
          return readChunk();
        });
      }
      return readChunk();
    }

    if (typeof response.text !== "function") return Promise.reject(new Error("invalid response"));
    return response.text().then(function (text) {
      if (textBytes(text) > MAX_RESPONSE_BYTES) throw new Error("response too large");
      return text;
    });
  }

  function boundedJSON(response) {
    return boundedText(response).then(function (text) {
      return text ? JSON.parse(text) : {};
    });
  }

  function configuredValue(select) {
    if (!select) return "";
    for (var i = 0; i < select.options.length; i++) {
      if (select.options[i].defaultSelected) return select.options[i].value;
    }
    return typeof select.defaultValue === "string" ? select.defaultValue : select.value;
  }

  function bind(documentObject, fetchFunction) {
    if (!documentObject || typeof fetchFunction !== "function") return null;
    if (documentObject[BINDING_KEY]) return documentObject[BINDING_KEY];

    var dialog = documentObject.querySelector("#new-session-modal");
    var trigger = documentObject.querySelector("#new-session-button");
    var form = documentObject.querySelector("#new-session-form");
    if (!dialog || !trigger || !form) return null;

    var closeButton = dialog.querySelector("[data-modal-close]");
    var cancelButton = dialog.querySelector("#new-session-cancel");
    var launchButton = dialog.querySelector("#new-session-launch");
    var status = dialog.querySelector("#new-session-status");
    var rootModel = dialog.querySelector("[data-root-model]");
    var rootThinking = dialog.querySelector("[data-root-thinking]");
    var workerRows = Array.from(dialog.querySelectorAll("[data-worker-row]"));
    var modelPairs = [{ model: rootModel, thinking: rootThinking }].concat(workerRows.map(function (row) {
      return {
        model: row.querySelector("[data-worker-model]"),
        thinking: row.querySelector("[data-worker-thinking]")
      };
    }));
    modelPairs.forEach(function (pair) {
      pair.configuredModel = configuredValue(pair.model);
      pair.configuredThinking = configuredValue(pair.thinking);
    });
    var removers = [];
    var pendingSnapshot = null;
    var requestGeneration = 0;

    function listen(target, type, listener, options) {
      if (!target) return;
      target.addEventListener(type, listener, options);
      removers.push(function () { target.removeEventListener(type, listener, options); });
    }

    function rebuildAll() {
      modelPairs.forEach(function (pair) {
        if (pair.model && pair.thinking) rebuildThinking(documentObject, pair.model, pair.thinking);
      });
    }

    function setPending(pending) {
      var controls = Array.from(dialog.querySelectorAll("button, select, input, textarea"));
      if (pending) {
        pendingSnapshot = controls.map(function (control) {
          var prior = control.disabled;
          control.disabled = true;
          return { control: control, disabled: prior };
        });
        dialog.setAttribute("aria-busy", "true");
        if (launchButton) launchButton.textContent = "Launching…";
        return;
      }
      if (pendingSnapshot) {
        pendingSnapshot.forEach(function (item) { item.control.disabled = item.disabled; });
        pendingSnapshot = null;
      }
      dialog.removeAttribute("aria-busy");
      if (launchButton) launchButton.textContent = "Launch";
      rebuildAll();
    }

    function reset() {
      form.reset();
      modelPairs.forEach(function (pair) {
        pair.model.value = pair.configuredModel;
        rebuildThinking(documentObject, pair.model, pair.thinking);
        if (levelsFor(pair.model).indexOf(pair.configuredThinking) !== -1) {
          pair.thinking.value = pair.configuredThinking;
        }
      });
      if (status) status.textContent = "";
    }

    function open() {
      requestGeneration++;
      setPending(false);
      reset();
      dialog.showModal();
      if (rootModel && typeof rootModel.focus === "function") rootModel.focus();
    }

    function closeAndReset() {
      requestGeneration++;
      setPending(false);
      reset();
      if (dialog.open) dialog.close();
    }

    function onSubmit(event) {
      event.preventDefault();
      if (pendingSnapshot) return;
      var generation = ++requestGeneration;
      if (status) status.textContent = "";
      setPending(true);
      var request;
      try {
        request = buildRequest(dialog);
      } catch (_error) {
        setPending(false);
        if (status) status.textContent = GENERIC_ERROR;
        return;
      }
      var fetchResult;
      try {
        fetchResult = fetchFunction("/ui/sessions", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(request),
          credentials: "same-origin"
        });
      } catch (error) {
        fetchResult = Promise.reject(error);
      }
      Promise.resolve(fetchResult).then(function (response) {
        return boundedJSON(response).then(function (body) { return { response: response, body: body }; });
      }).then(function (result) {
        if (generation !== requestGeneration) return;
        if (result.response.status === 201) {
          closeAndReset();
          return;
        }
        setPending(false);
        if (status) status.textContent = sanitizedError(result.body && result.body.error);
      }).catch(function () {
        if (generation !== requestGeneration) return;
        setPending(false);
        if (status) status.textContent = GENERIC_ERROR;
      });
    }

    function closeClick(event) {
      event.preventDefault();
      closeAndReset();
    }

    function backdropClick(event) {
      if (event.target !== dialog) return;
      event.preventDefault();
      closeAndReset();
    }

    function nativeCancel(event) {
      event.preventDefault();
      closeAndReset();
    }

    function escapeGuard(event) {
      if (event.key !== "Escape" || !dialog.open) return;
      event.preventDefault();
      event.stopImmediatePropagation();
      closeAndReset();
    }

    listen(trigger, "click", open);
    listen(closeButton, "click", closeClick);
    listen(cancelButton, "click", closeClick);
    listen(dialog, "click", backdropClick);
    listen(dialog, "cancel", nativeCancel);
    listen(form, "submit", onSubmit);
    listen(documentObject, "keydown", escapeGuard, true);
    modelPairs.forEach(function (pair) {
      listen(pair.model, "change", function () {
        rebuildThinking(documentObject, pair.model, pair.thinking);
      });
    });

    var controller = {
      open: open,
      close: closeAndReset,
      destroy: function () {
        requestGeneration++;
        setPending(false);
        removers.forEach(function (remove) { remove(); });
        removers = [];
        if (documentObject[BINDING_KEY] === controller) delete documentObject[BINDING_KEY];
      }
    };
    documentObject[BINDING_KEY] = controller;
    return controller;
  }

  return {
    levelsFor: levelsFor,
    rebuildThinking: rebuildThinking,
    buildRequest: buildRequest,
    sanitizedError: sanitizedError,
    bind: bind
  };
});
