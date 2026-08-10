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
    var emptyRow = root.querySelector("[data-repository-empty]");
    var options = Array.from(root.querySelectorAll("[data-repository-option]")).map(function (element) {
      return { element: element, value: element.getAttribute("data-value") || "" };
    });
    if (!queryInput || !committedInput || !listbox || !results || !emptyRow || !options.length) return null;

    var removers = [];
    var filtered = options.slice();
    var activeIndex = -1;
    var committedValue = null;
    var open = false;
    var pending = false;
    var destroyed = false;
    var composing = false;
    var ignoreNextKey = false;
    var compositionTimer = null;
    var touchGesture = null;

    function listen(target, type, listener, listenerOptions) {
      target.addEventListener(type, listener, listenerOptions);
      removers.push(function () { target.removeEventListener(type, listener, listenerOptions); });
    }

    function isCompositionKey(event) {
      return !!event && (event.isComposing === true || event.keyCode === 229);
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
      options.forEach(function (option) { option.element.removeAttribute("data-active"); });
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
      emptyRow.hidden = filtered.length !== 0;
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

    function clearCompositionTimer() {
      if (compositionTimer === null) return;
      clearTimeout(compositionTimer);
      compositionTimer = null;
    }

    function onCompositionStart() {
      clearCompositionTimer();
      composing = true;
      ignoreNextKey = false;
    }

    function onCompositionEnd() {
      clearCompositionTimer();
      composing = false;
      ignoreNextKey = true;
      compositionTimer = setTimeout(function () {
        compositionTimer = null;
        ignoreNextKey = false;
      }, 0);
    }

    function onKeydown(event) {
      if (pending || destroyed) return;
      if (composing || isCompositionKey(event)) return;
      if (ignoreNextKey) {
        clearCompositionTimer();
        ignoreNextKey = false;
        return;
      }
      var key = event.key;
      if (key === "ArrowDown" || key === "ArrowUp") {
        event.preventDefault();
        if (!open) show();
        if (!filtered.length) return;
        if (key === "ArrowDown") setActive(activeIndex < 0 ? 0 : Math.min(activeIndex + 1, filtered.length - 1));
        else setActive(activeIndex < 0 ? filtered.length - 1 : Math.max(activeIndex - 1, 0));
        return;
      }
      if ((key === "Home" || key === "End") && open) {
        event.preventDefault();
        if (!filtered.length) return;
        setActive(key === "Home" ? 0 : filtered.length - 1);
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
      if (touchGesture) return;
      if (event.relatedTarget && root.contains(event.relatedTarget)) return;
      close();
    }

    function onDocumentPointerdown(event) {
      if (!root.contains(event.target)) close();
    }

    function isTouchLike(event) {
      return event.pointerType === "touch" || event.pointerType === "pen";
    }

    function startTouchGesture(event, option) {
      touchGesture = {
        pointerId: event.pointerId,
        startX: typeof event.clientX === "number" ? event.clientX : 0,
        startY: typeof event.clientY === "number" ? event.clientY : 0,
        moved: false,
        option: option
      };
    }

    function matchingGesture(event) {
      return touchGesture && event.pointerId === touchGesture.pointerId;
    }

    function onDocumentPointermove(event) {
      if (!matchingGesture(event)) return;
      var x = typeof event.clientX === "number" ? event.clientX : touchGesture.startX;
      var y = typeof event.clientY === "number" ? event.clientY : touchGesture.startY;
      var deltaX = x - touchGesture.startX;
      var deltaY = y - touchGesture.startY;
      if (deltaX * deltaX + deltaY * deltaY > 64) touchGesture.moved = true;
    }

    function onDocumentPointerup(event) {
      if (!matchingGesture(event)) return;
      var gesture = touchGesture;
      touchGesture = null;
      if (!gesture.moved && gesture.option.element.contains(event.target)) commit(gesture.option);
    }

    function onDocumentPointercancel(event) {
      if (matchingGesture(event)) touchGesture = null;
    }

    listen(queryInput, "focus", onFocus);
    listen(queryInput, "input", onInput);
    listen(queryInput, "compositionstart", onCompositionStart);
    listen(queryInput, "compositionend", onCompositionEnd);
    listen(queryInput, "keydown", onKeydown);
    listen(queryInput, "blur", onBlur);
    listen(documentObject, "pointerdown", onDocumentPointerdown);
    listen(documentObject, "pointermove", onDocumentPointermove);
    listen(documentObject, "pointerup", onDocumentPointerup);
    listen(documentObject, "pointercancel", onDocumentPointercancel);
    listen(documentObject, "click", onDocumentPointerdown);
    options.forEach(function (option) {
      listen(option.element, "pointerdown", function (event) {
        if (pending || destroyed) return;
        if (isTouchLike(event)) {
          startTouchGesture(event, option);
          return;
        }
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
      value: function () { return committedValue === null ? "" : committedValue; },
      query: function () { return queryInput.value; },
      validate: function () {
        var exact = exactOption(queryInput.value);
        if (!exact || committedValue === null || exact.value !== committedValue) {
          return { valid: false, message: INVALID_MESSAGE };
        }
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
        clearCompositionTimer();
        touchGesture = null;
        close();
        removers.forEach(function (remove) { remove(); });
        removers = [];
      }
    };
  }

  return { bind: bind };
});
