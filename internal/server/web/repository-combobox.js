(function (root, factory) {
  "use strict";
  if (typeof module === "object" && module.exports) {
    module.exports = factory();
  } else {
    root.KanediasRepositoryCombobox = factory();
  }
})(typeof self !== "undefined" ? self : this, function () {
  "use strict";

  var INVALID_MESSAGE = "Choose a configured repository or clear the field to use /workspace.";

  function bind(root, documentObject) {
    if (!root || !documentObject) return null;

    var queryInput = root.querySelector("[data-repository-query]");
    var committedInput = root.querySelector("[data-start-repository]");
    var listbox = root.querySelector("[data-repository-listbox]");
    var results = root.querySelector("[data-repository-results]");
    var options = Array.from(root.querySelectorAll("[data-repository-option]")).map(function (element) {
      return { element: element, value: element.getAttribute("data-value") || "" };
    });
    if (!queryInput || !committedInput || !listbox || !results || !options.length) return null;

    var removers = [];
    var filtered = options.slice();
    var activeIndex = -1;
    var committedValue = null;
    var open = false;
    var pending = false;
    var destroyed = false;

    function listen(target, type, listener, listenerOptions) {
      target.addEventListener(type, listener, listenerOptions);
      removers.push(function () { target.removeEventListener(type, listener, listenerOptions); });
    }

    function exactOption(query) {
      var normalized = query.toLowerCase();
      for (var i = 0; i < options.length; i++) {
        if (options[i].value.toLowerCase() === normalized) return options[i];
      }
      return null;
    }

    function setSelected(value) {
      options.forEach(function (option) {
        option.element.setAttribute("aria-selected", value !== null && option.value === value ? "true" : "false");
      });
    }

    function clearActive() {
      activeIndex = -1;
      queryInput.removeAttribute("aria-activedescendant");
      options.forEach(function (option) { option.element.removeAttribute("data-active"); });
    }

    function setActive(index) {
      if (!filtered.length) {
        clearActive();
        return;
      }
      activeIndex = Math.max(0, Math.min(index, filtered.length - 1));
      var active = filtered[activeIndex].element;
      active.setAttribute("data-active", "true");
      var id = active.getAttribute("id");
      if (id) queryInput.setAttribute("aria-activedescendant", id);
      else queryInput.removeAttribute("aria-activedescendant");
      if (typeof active.scrollIntoView === "function") active.scrollIntoView({ block: "nearest" });
    }

    function announce() {
      var count = filtered.length;
      if (!count) {
        results.textContent = "No configured repositories match.";
      } else if (queryInput.value === "") {
        results.textContent = count + " configured repositories available.";
      } else {
        results.textContent = count + " matches.";
      }
    }

    function filter() {
      var needle = queryInput.value.toLowerCase();
      filtered = options.filter(function (option) {
        var visible = !needle || option.value.toLowerCase().indexOf(needle) !== -1;
        option.element.hidden = !visible;
        return visible;
      });
      clearActive();
      announce();
    }

    function close() {
      open = false;
      listbox.hidden = true;
      queryInput.setAttribute("aria-expanded", "false");
      clearActive();
    }

    function show() {
      if (pending || destroyed) return;
      filter();
      open = true;
      listbox.hidden = false;
      queryInput.setAttribute("aria-expanded", "true");
    }

    function writeCommitment(option, normalizeQuery) {
      committedValue = option ? option.value : null;
      committedInput.value = option ? option.value : "";
      if (normalizeQuery && option) queryInput.value = option.value;
      setSelected(committedValue);
    }

    function commit(option) {
      if (!option || pending || destroyed) return;
      writeCommitment(option, true);
      filter();
      close();
    }

    function onFocus() {
      show();
    }

    function onInput() {
      if (pending || destroyed) return;
      var exact = exactOption(queryInput.value);
      writeCommitment(exact, false);
      filter();
      open = true;
      listbox.hidden = false;
      queryInput.setAttribute("aria-expanded", "true");
    }

    function onKeydown(event) {
      if (pending || destroyed) return;
      var key = event.key;
      if (key === "ArrowDown" || key === "ArrowUp" || key === "Home" || key === "End") {
        event.preventDefault();
        if (!open) show();
        if (!filtered.length) return;
        if (key === "Home") setActive(0);
        else if (key === "End") setActive(filtered.length - 1);
        else if (key === "ArrowDown") setActive(activeIndex < 0 ? 0 : Math.min(activeIndex + 1, filtered.length - 1));
        else setActive(activeIndex < 0 ? filtered.length - 1 : Math.max(activeIndex - 1, 0));
        return;
      }
      if (key === "Enter") {
        var enterOption = activeIndex >= 0 ? filtered[activeIndex] :
          (filtered.length === 1 ? filtered[0] : exactOption(queryInput.value));
        if (enterOption) {
          event.preventDefault();
          commit(enterOption);
        }
        return;
      }
      if (key === "Escape" && open) {
        event.preventDefault();
        event.stopPropagation();
        close();
        return;
      }
      if (key === "Tab") {
        var tabOption = exactOption(queryInput.value);
        if (tabOption) commit(tabOption);
        else close();
      }
    }

    function onBlur(event) {
      if (event.relatedTarget && root.contains(event.relatedTarget)) return;
      close();
    }

    function onDocumentPointerdown(event) {
      if (!root.contains(event.target)) close();
    }

    listen(queryInput, "focus", onFocus);
    listen(queryInput, "input", onInput);
    listen(queryInput, "keydown", onKeydown);
    listen(queryInput, "blur", onBlur);
    listen(documentObject, "pointerdown", onDocumentPointerdown);
    listen(documentObject, "click", onDocumentPointerdown);
    options.forEach(function (option) {
      listen(option.element, "pointerdown", function (event) {
        if (pending || destroyed) return;
        event.preventDefault();
        commit(option);
      });
    });

    var initial = exactOption(committedInput.value || "");
    if (initial) {
      committedValue = initial.value;
      queryInput.value = initial.value;
      setSelected(initial.value);
    } else {
      committedInput.value = "";
      setSelected(null);
    }
    filter();
    close();
    results.textContent = "";

    return {
      value: function () { return committedInput.value; },
      query: function () { return queryInput.value; },
      validate: function () {
        var exact = exactOption(queryInput.value);
        if (!exact || committedValue === null) return { valid: false, message: INVALID_MESSAGE };
        return { valid: true, message: "" };
      },
      reset: function () {
        if (destroyed) return;
        queryInput.value = "";
        var workspace = exactOption("");
        writeCommitment(workspace, false);
        filter();
        close();
        results.textContent = "";
      },
      setPending: function (value) {
        if (destroyed) return;
        pending = Boolean(value);
        if (pending) close();
      },
      destroy: function () {
        if (destroyed) return;
        destroyed = true;
        close();
        removers.forEach(function (remove) { remove(); });
        removers = [];
      }
    };
  }

  return { bind: bind };
});
