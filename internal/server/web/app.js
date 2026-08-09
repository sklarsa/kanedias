(function () {
  "use strict";

  /* -------- Tab switching (delegated) -------- */
  document.addEventListener("click", function (e) {
    var tab = e.target.closest(".tab");
    if (!tab) return;
    var container = tab.closest(".tabs");
    if (!container) return;
    container.querySelectorAll(".tab").forEach(function (t) {
      t.classList.remove("active");
    });
    tab.classList.add("active");
    var name = tab.getAttribute("data-tab");
    document.querySelectorAll(".tabpane").forEach(function (p) {
      p.classList.remove("active");
    });
    var pane = document.querySelector('.tabpane[data-pane="' + name + '"]');
    if (pane) pane.classList.add("active");
  });

  /* -------- Search / filter (delegated) -------- */
  document.addEventListener("input", function (e) {
    if (e.target.id !== "search") return;
    var q = e.target.value.trim().toLowerCase();
    document.querySelectorAll(".row").forEach(function (row) {
      var hay = (
        (row.getAttribute("data-session-id") || "") +
        " " +
        (row.getAttribute("data-lifecycle") || "") +
        " " +
        (row.getAttribute("data-worker-type") || "")
      ).toLowerCase();
      var match = !q || hay.indexOf(q) !== -1;
      var container =
        row.closest("details") && row.parentElement.tagName === "SUMMARY"
          ? row.closest("details")
          : row;
      if (container === row) {
        row.style.display = match ? "" : "none";
      } else {
        var anyChild = Array.prototype.some.call(
          container.querySelectorAll(".row"),
          function (cr) {
            var h = (
              (cr.getAttribute("data-session-id") || "") +
              " " +
              (cr.getAttribute("data-lifecycle") || "")
            ).toLowerCase();
            return !q || h.indexOf(q) !== -1;
          }
        );
        container.style.display = anyChild ? "" : "none";
        if (q && anyChild) container.setAttribute("open", "");
      }
    });
  });

  /* -------- Mobile slide-over (delegated) -------- */
  document.addEventListener("click", function (e) {
    var sidebar = document.getElementById("sidebar");
    var scrim = document.getElementById("scrim");
    if (!sidebar || !scrim) return;

    if (e.target.id === "menuBtn" || e.target.closest("#menuBtn")) {
      if (sidebar.classList.contains("open")) {
        closeSheet(sidebar, scrim);
      } else {
        openSheet(sidebar, scrim);
      }
      return;
    }
    if (e.target === scrim || e.target.closest("#scrim") === scrim) {
      closeSheet(sidebar, scrim);
    }
  });

  function openSheet(sidebar, scrim) {
    sidebar.classList.add("open");
    scrim.classList.add("show");
    var menuBtn = document.getElementById("menuBtn");
    if (menuBtn) menuBtn.setAttribute("aria-expanded", "true");
  }

  function closeSheet(sidebar, scrim) {
    sidebar.classList.remove("open");
    scrim.classList.remove("show");
    var menuBtn = document.getElementById("menuBtn");
    if (menuBtn) menuBtn.setAttribute("aria-expanded", "false");
  }

  function closeSheetIfMobile(sidebar, scrim) {
    if (window.matchMedia("(max-width:820px)").matches) closeSheet(sidebar, scrim);
  }

  /* -------- Enter in the deck input sends a steering message (delegated) -------- */
  document.addEventListener("keydown", function (e) {
    if (e.key !== "Enter") return;
    var input = e.target.closest(".deck-input");
    if (!input) return;
    e.preventDefault();
    var btn = document.getElementById("steerBtn");
    if (btn) btn.click();
    // Clear the just-sent command so it is not left queued in the box.
    input.value = "";
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });

  /* -------- Transcript auto-scrolls to the newest content (TUI-like) -------- */
  (function () {
    var panel = document.getElementById("activity-panel");
    if (!panel) return;
    var shouldStick = true;
    var lastTranscript = null;
    function stick() {
      var t = panel.querySelector(".transcript");
      if (!t) return;
      if (lastTranscript !== t) {
        lastTranscript = t;
        shouldStick = true;
        t.addEventListener("scroll", function () {
          shouldStick = t.scrollHeight - t.scrollTop - t.clientHeight < 40;
        });
      }
      if (shouldStick) t.scrollTop = t.scrollHeight;
    }
    // After every activity patch, render any newly-flagged Markdown before
    // measuring for auto-scroll so the rendered height is correct.
    function refresh() {
      if (window.KanediasMarkdown && window.KanediasMarkdown.renderPending) {
        window.KanediasMarkdown.renderPending(panel);
      }
      stick();
    }
    new MutationObserver(refresh).observe(panel, { childList: true, subtree: true, characterData: true });
    refresh();
  })();

  /* -------- Copy fenced code from .code-block (delegated) -------- */
  document.addEventListener("click", function (e) {
    var btn = e.target.closest("[data-copy-code]");
    if (!btn) return;
    e.stopPropagation();
    var block = btn.closest(".code-block");
    var code = block ? block.querySelector("code") : null;
    if (!code || typeof code.textContent !== "string") return;
    var text = code.textContent;
    var done = function (ok) {
      var label = ok ? "copied" : "copy failed";
      var original = btn.getAttribute("data-copy-label") || "copy";
      btn.textContent = label;
      setTimeout(function () { btn.textContent = original; }, 1200);
    };
    var finish = function (ok) { done(ok); };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(
        function () { finish(true); },
        function () { fallbackCopy(text, finish); }
      );
    } else {
      fallbackCopy(text, finish);
    }
  });

  function fallbackCopy(text, finish) {
    var ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed";
    ta.style.top = "-1000px";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    var ok = false;
    try {
      ok = document.execCommand("copy");
    } catch (e) {
      ok = false;
    }
    document.body.removeChild(ta);
    finish(ok);
  }

  /* -------- Deck controls reflect the selected session's capability -------- */
  function setDeckState(sessionID, lifecycle) {
    var canAct = !!sessionID && (lifecycle === "ready" || lifecycle === "running" ||
      lifecycle === "active" || lifecycle === "question" || lifecycle === "starting");
    var sel = function (sel) { return document.querySelector(sel); };
    var steer = sel("#steerBtn");
    var itr = sel(".dbtn.interrupt");
    var stop = sel(".dbtn.stop");
    if (steer) steer.disabled = !canAct;
    if (stop) stop.disabled = !canAct;
    if (itr) itr.disabled = !(!!sessionID && lifecycle === "running");
  }
  document.addEventListener("click", function (e) {
    var row = e.target.closest(".row");
    if (!row) return;
    setDeckState(row.getAttribute("data-session-id"), row.getAttribute("data-lifecycle"));
  });
  setDeckState("", "");

  /* -------- Alert banner jumps to first question (delegated) -------- */
  document.addEventListener("click", function (e) {
    if (!e.target.closest("#alertBanner")) return;
    var firstQ = document.querySelector(".row[data-lifecycle='question']");
    if (firstQ) {
      var d = firstQ.closest("details");
      while (d) {
        d.setAttribute("open", "");
        d = d.parentElement ? d.parentElement.closest("details") : null;
      }
      var sidebar = document.getElementById("sidebar");
      var scrim = document.getElementById("scrim");
      if (sidebar && scrim) openSheet(sidebar, scrim);
      firstQ.scrollIntoView({ block: "center", behavior: "smooth" });
    }
  });
})();
