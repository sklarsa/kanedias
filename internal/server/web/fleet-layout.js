(function (root, factory) {
  "use strict";
  var api = factory();
  if (typeof module === "object" && module.exports) module.exports = api;
  if (root) root.KanediasFleetLayout = api;
})(typeof globalThis !== "undefined" ? globalThis : this, function () {
  "use strict";

  var MOBILE_MAX = 820;
  var MIN_WIDTH = 240;
  var MAX_WIDTH = 560;
  var WIDTH_KEY = "kanedias.fleet.width.v1";
  var COLLAPSED_KEY = "kanedias.fleet.collapsed.v1";

  function defaultWidth(viewportWidth) {
    return viewportWidth >= 1100 ? 340 : 300;
  }

  function clampWidth(preferredWidth, viewportWidth) {
    var viewportMaximum = Math.floor(viewportWidth * 0.5);
    return Math.max(MIN_WIDTH, Math.min(MAX_WIDTH, viewportMaximum, preferredWidth));
  }

  function bind(documentObject, windowObject, storageObject) {
    var app = documentObject.querySelector(".app");
    var fleetPanel = documentObject.getElementById("fleet-panel");
    var resizer = documentObject.getElementById("fleet-resizer");
    var mainStack = documentObject.getElementById("main-stack");
    var menu = documentObject.getElementById("menuBtn");
    var scrim = documentObject.getElementById("scrim");
    if (!app || !fleetPanel || !resizer || !mainStack || !menu || !scrim) {
      throw new Error("Fleet layout requires the stable application shell");
    }

    var storage = storageObject;
    if (arguments.length < 3) {
      try {
        storage = windowObject.localStorage;
      } catch (_) {
        storage = null;
      }
    }

    function readStorage(key) {
      if (!storage || typeof storage.getItem !== "function") return null;
      try {
        return storage.getItem(key);
      } catch (_) {
        return null;
      }
    }

    function writeStorage(key, value) {
      if (!storage || typeof storage.setItem !== "function") return;
      try {
        storage.setItem(key, String(value));
      } catch (_) {
        // Storage can be disabled or full. Layout remains usable in memory.
      }
    }

    var savedWidthValue = readStorage(WIDTH_KEY);
    var savedWidth = Number(savedWidthValue);
    var preferredWidth = savedWidthValue !== null && savedWidthValue.trim() !== "" && Number.isFinite(savedWidth)
      ? savedWidth
      : defaultWidth(windowObject.innerWidth);
    var effectiveWidth = clampWidth(preferredWidth, windowObject.innerWidth);
    var savedCollapsed = readStorage(COLLAPSED_KEY);
    var collapsed = savedCollapsed === "true";
    var dragging = null;
    var destroyed = false;
    var observer = null;
    var listenerCleanups = [];

    function isMobile() {
      try {
        if (typeof windowObject.matchMedia === "function") {
          return windowObject.matchMedia("(max-width:820px)").matches;
        }
      } catch (_) {
        // Fall back to the viewport value when media queries are unavailable.
      }
      return windowObject.innerWidth <= MOBILE_MAX;
    }

    function sidebar() {
      return documentObject.getElementById("sidebar");
    }

    function currentMaximum() {
      return Math.max(MIN_WIDTH, Math.min(MAX_WIDTH, Math.floor(windowObject.innerWidth * 0.5)));
    }

    function listen(target, type, listener) {
      target.addEventListener(type, listener);
      listenerCleanups.push(function () { target.removeEventListener(type, listener); });
    }

    function syncCollapseControls() {
      var controls = documentObject.querySelectorAll("[data-fleet-collapse]");
      var currentSidebar = sidebar();
      var expanded = isMobile()
        ? !!currentSidebar && currentSidebar.classList.contains("open")
        : !collapsed;
      for (var index = 0; index < controls.length; index++) {
        controls[index].setAttribute("aria-label", "Hide Fleet");
        controls[index].setAttribute("aria-controls", "fleet-panel");
        controls[index].setAttribute("aria-expanded", expanded ? "true" : "false");
      }
    }

    function closeMobileSheet() {
      var currentSidebar = sidebar();
      if (currentSidebar) currentSidebar.classList.remove("open");
      scrim.classList.remove("show");
      menu.setAttribute("aria-expanded", "false");
    }

    function sync() {
      var mobile = isMobile();
      effectiveWidth = clampWidth(preferredWidth, windowObject.innerWidth);
      app.style.setProperty("--fleet-width", effectiveWidth + "px");
      app.classList.toggle("fleet-collapsed", !mobile && collapsed);
      resizer.setAttribute("aria-valuemin", String(MIN_WIDTH));
      resizer.setAttribute("aria-valuemax", String(currentMaximum()));
      resizer.setAttribute("aria-valuenow", String(effectiveWidth));
      resizer.setAttribute("aria-disabled", mobile || collapsed ? "true" : "false");
      menu.setAttribute("aria-controls", "fleet-panel");
      if (mobile) {
        var currentSidebar = sidebar();
        var open = !!currentSidebar && currentSidebar.classList.contains("open");
        menu.setAttribute("aria-label", open ? "Close fleet tree" : "Open fleet tree");
        menu.setAttribute("aria-expanded", open ? "true" : "false");
      } else {
        menu.setAttribute("aria-label", collapsed ? "Show Fleet" : "Hide Fleet");
        menu.setAttribute("aria-expanded", collapsed ? "false" : "true");
      }
      syncCollapseControls();
    }

    function show() {
      if (isMobile()) {
        var currentSidebar = sidebar();
        if (currentSidebar) currentSidebar.classList.add("open");
        scrim.classList.add("show");
      } else {
        collapsed = false;
        writeStorage(COLLAPSED_KEY, false);
      }
      sync();
    }

    function hide() {
      if (isMobile()) {
        closeMobileSheet();
      } else {
        finishDrag();
        collapsed = true;
        writeStorage(COLLAPSED_KEY, true);
      }
      sync();
    }

    function toggle() {
      if (isMobile()) {
        var currentSidebar = sidebar();
        if (currentSidebar && currentSidebar.classList.contains("open")) hide(); else show();
      } else if (collapsed) {
        show();
      } else {
        hide();
      }
    }

    function setPreferredWidth(width) {
      preferredWidth = clampWidth(width, windowObject.innerWidth);
      effectiveWidth = preferredWidth;
      writeStorage(WIDTH_KEY, preferredWidth);
      sync();
    }

    function finishDrag(pointerId) {
      if (!dragging || (pointerId !== undefined && pointerId !== dragging.pointerId)) return;
      var activePointerId = dragging.pointerId;
      dragging = null;
      app.classList.remove("fleet-resizing");
      try {
        if (typeof resizer.hasPointerCapture !== "function" || resizer.hasPointerCapture(activePointerId)) {
          resizer.releasePointerCapture(activePointerId);
        }
      } catch (_) {
        // Capture may already have been released by the browser.
      }
    }

    function onPointerDown(event) {
      if (isMobile() || collapsed || event.pointerId === undefined) return;
      dragging = {
        pointerId: event.pointerId,
        clientX: event.clientX,
        width: effectiveWidth
      };
      app.classList.add("fleet-resizing");
      try { resizer.setPointerCapture(event.pointerId); } catch (_) {}
      if (event.preventDefault) event.preventDefault();
    }

    function onPointerMove(event) {
      if (!dragging || event.pointerId !== dragging.pointerId) return;
      setPreferredWidth(dragging.width + event.clientX - dragging.clientX);
      if (event.preventDefault) event.preventDefault();
    }

    function onPointerEnd(event) {
      if (!dragging || event.pointerId !== dragging.pointerId) return;
      finishDrag(event.pointerId);
    }

    function onKeyDown(event) {
      if (isMobile() || collapsed) return;
      var width = effectiveWidth;
      if (event.key === "ArrowLeft") width -= 16;
      else if (event.key === "ArrowRight") width += 16;
      else if (event.key === "Home") width = MIN_WIDTH;
      else if (event.key === "End") width = MAX_WIDTH;
      else return;
      if (event.preventDefault) event.preventDefault();
      setPreferredWidth(clampWidth(width, windowObject.innerWidth));
    }

    function onDocumentClick(event) {
      var target = event.target;
      if (!target || typeof target.closest !== "function") return;
      if (target.closest("#menuBtn")) {
        toggle();
        return;
      }
      if (target.closest("#scrim")) {
        if (isMobile()) hide();
        return;
      }
      if (target.closest("[data-fleet-collapse]")) hide();
    }

    function onResize() {
      if (!isMobile()) closeMobileSheet();
      finishDrag();
      sync();
    }

    function state() {
      var currentSidebar = sidebar();
      return {
        preferredWidth: preferredWidth,
        effectiveWidth: effectiveWidth,
        collapsed: collapsed,
        mobile: isMobile(),
        mobileOpen: !!currentSidebar && currentSidebar.classList.contains("open")
      };
    }

    function destroy() {
      if (destroyed) return;
      destroyed = true;
      finishDrag();
      if (observer) observer.disconnect();
      while (listenerCleanups.length) listenerCleanups.pop()();
    }

    listen(documentObject, "click", onDocumentClick);
    listen(resizer, "pointerdown", onPointerDown);
    listen(resizer, "keydown", onKeyDown);
    listen(windowObject, "pointermove", onPointerMove);
    listen(windowObject, "pointerup", onPointerEnd);
    listen(windowObject, "pointercancel", onPointerEnd);
    listen(windowObject, "resize", onResize);
    listen(windowObject, "beforeunload", destroy);

    var MutationObserverClass = windowObject.MutationObserver;
    if (MutationObserverClass) {
      observer = new MutationObserverClass(sync);
      observer.observe(fleetPanel, {childList: true, subtree: true});
    }
    sync();

    return {
      show: show,
      hide: hide,
      toggle: toggle,
      sync: sync,
      state: state,
      destroy: destroy
    };
  }

  return {
    defaultWidth: defaultWidth,
    clampWidth: clampWidth,
    bind: bind
  };
});
