(function (root, factory) {
  "use strict";
  var api = factory();
  if (typeof module === "object" && module.exports) module.exports = api;
  if (root) root.KanediasImageAttachments = api;
})(typeof globalThis !== "undefined" ? globalThis : this, function () {
  "use strict";

  var LIMITS = {maxImages: 4, maxImageBytes: 3 * 1024 * 1024, maxTotalBytes: 8 * 1024 * 1024};
  var NEUTRAL_MESSAGE = "Please inspect the attached image(s).";
  var SUPPORTED_TYPES = new Set(["image/png", "image/jpeg", "image/gif", "image/webp"]);
  var UNKNOWN_DELIVERY = "Delivery is unknown. Check the session transcript before retrying.";

  function createController(options) {
    if (!options || typeof options.fetch !== "function" || typeof options.FormData !== "function" ||
        typeof options.createObjectURL !== "function" || typeof options.revokeObjectURL !== "function") {
      throw new TypeError("fetch, FormData, createObjectURL, and revokeObjectURL are required");
    }
    var state = {drafts: new Map(), selectedSessionID: "", nextAttachmentID: 1};
    return {
      selectSession: function (sessionID) { state.selectedSessionID = normalizeSessionID(sessionID); return snapshot(state, state.selectedSessionID); },
      setText: function (text) { return setDraftText(state, text, options); },
      stageFiles: function (files) { return stageDraftFiles(state, files, options); },
      removeImage: function (id) { return removeDraftImage(state, id, options); },
      reconcileSessions: function (sessionIDs) { return reconcileDrafts(state, sessionIDs, options); },
      submit: function (sessionID) { return submitDraft(state, sessionID, options); },
      draft: function (sessionID) { return snapshot(state, normalizeSessionID(sessionID)); },
      destroy: function () { return destroyDrafts(state, options); }
    };
  }

  function normalizeSessionID(sessionID) {
    return sessionID === undefined || sessionID === null ? "" : String(sessionID);
  }

  function emptyDraft() {
    return {text: "", images: [], busy: false};
  }

  function draftFor(state, sessionID) {
    var draft = state.drafts.get(sessionID);
    if (!draft) {
      draft = emptyDraft();
      state.drafts.set(sessionID, draft);
    }
    return draft;
  }

  function snapshot(state, sessionID) {
    sessionID = normalizeSessionID(sessionID);
    var draft = state.drafts.get(sessionID) || emptyDraft();
    var totalBytes = 0;
    var images = draft.images.map(function (image) {
      totalBytes += image.size;
      return Object.freeze({
        id: image.id,
        name: image.name,
        type: image.type,
        size: image.size,
        lastModified: image.lastModified,
        url: image.url
      });
    });
    Object.freeze(images);
    return Object.freeze({
      sessionID: sessionID,
      text: draft.text,
      images: images,
      totalBytes: totalBytes,
      busy: draft.busy
    });
  }

  function notifyChange(state, sessionID, options) {
    var changed = snapshot(state, sessionID);
    if (typeof options.onChange === "function") options.onChange(changed);
    return changed;
  }

  function notifyStatus(options, message, sessionID) {
    if (typeof options.onStatus === "function") options.onStatus(message, normalizeSessionID(sessionID));
  }

  function requireSelectedDraft(state, options, action) {
    if (!state.selectedSessionID) {
      notifyStatus(options, action === "send" ?
        "Select a session before sending a message." :
        "Select a session before attaching images.", "");
      return null;
    }
    return draftFor(state, state.selectedSessionID);
  }

  function setDraftText(state, text, options) {
    var draft = requireSelectedDraft(state, options, "edit");
    if (!draft) return false;
    if (draft.busy) {
      notifyStatus(options, "This draft is already being sent.", state.selectedSessionID);
      return false;
    }
    draft.text = text === undefined || text === null ? "" : String(text);
    notifyChange(state, state.selectedSessionID, options);
    return true;
  }

  function stageDraftFiles(state, files, options) {
    var draft = requireSelectedDraft(state, options, "stage");
    if (!draft) return snapshot(state, "");
    if (draft.busy) {
      notifyStatus(options, "This draft is already being sent.", state.selectedSessionID);
      return snapshot(state, state.selectedSessionID);
    }

    var list;
    try {
      list = Array.from(files || []);
    } catch (_) {
      list = [];
    }
    var totalBytes = draft.images.reduce(function (total, image) { return total + image.size; }, 0);
    var lastError = "";

    list.forEach(function (file) {
      var type = file && typeof file.type === "string" ? file.type.toLowerCase() : "";
      var size = file && typeof file.size === "number" && Number.isFinite(file.size) && file.size >= 0 ? file.size : LIMITS.maxImageBytes + 1;
      if (type && !SUPPORTED_TYPES.has(type)) {
        lastError = "Only PNG, JPEG, GIF, and WebP images are supported.";
        return;
      }
      if (draft.images.length >= LIMITS.maxImages) {
        lastError = "You can attach up to 4 images.";
        return;
      }
      if (size > LIMITS.maxImageBytes) {
        lastError = "Each image must be 3 MiB or smaller.";
        return;
      }
      if (totalBytes + size > LIMITS.maxTotalBytes) {
        lastError = "Attached images must total 8 MiB or less.";
        return;
      }

      var url;
      try {
        url = options.createObjectURL(file);
      } catch (_) {
        lastError = "That image could not be attached.";
        return;
      }
      var id = state.nextAttachmentID;
      var suppliedName = file && typeof file.name === "string" ? file.name.trim() : "";
      draft.images.push({
        id: id,
        file: file,
        name: suppliedName || "Pasted image " + id,
        type: type,
        size: size,
        lastModified: file && typeof file.lastModified === "number" ? file.lastModified : 0,
        url: String(url)
      });
      state.nextAttachmentID++;
      totalBytes += size;
    });

    var changed = notifyChange(state, state.selectedSessionID, options);
    if (lastError) notifyStatus(options, lastError, state.selectedSessionID);
    return changed;
  }

  function removeDraftImage(state, attachmentID, options) {
    var draft = requireSelectedDraft(state, options, "edit");
    if (!draft) return false;
    if (draft.busy) {
      notifyStatus(options, "This draft is already being sent.", state.selectedSessionID);
      return false;
    }
    var index = draft.images.findIndex(function (image) { return String(image.id) === String(attachmentID); });
    if (index < 0) return false;
    var removed = draft.images.splice(index, 1)[0];
    options.revokeObjectURL(removed.url);
    notifyChange(state, state.selectedSessionID, options);
    return true;
  }

  function revokeDraft(draft, options) {
    draft.images.forEach(function (image) { options.revokeObjectURL(image.url); });
    draft.images.length = 0;
  }

  function reconcileDrafts(state, sessionIDs, options) {
    var retained = new Set(Array.from(sessionIDs || [], normalizeSessionID));
    state.drafts.forEach(function (draft, sessionID) {
      if (retained.has(sessionID)) return;
      revokeDraft(draft, options);
      state.drafts.delete(sessionID);
    });
    if (state.selectedSessionID && !retained.has(state.selectedSessionID)) {
      state.selectedSessionID = "";
      return notifyChange(state, "", options);
    }
    return snapshot(state, state.selectedSessionID);
  }

  function exactResponseShape(body, expectedKeys) {
    if (!body || typeof body !== "object" || Array.isArray(body)) return false;
    var keys = Object.keys(body).sort();
    return keys.length === expectedKeys.length && keys.every(function (key, index) { return key === expectedKeys[index]; });
  }

  function classifyResponse(response, body) {
    if (response.status === 202 && exactResponseShape(body, ["accepted"]) && body.accepted === true) {
      return {outcome: "accepted"};
    }
    if (response.status >= 400 && response.status <= 599 &&
        exactResponseShape(body, ["accepted", "error"]) && body.accepted === false && typeof body.error === "string") {
      return {outcome: "rejected", error: body.error};
    }
    return {outcome: "unknown"};
  }

  async function submitDraft(state, sessionID, options) {
    var targetSessionID = sessionID === undefined ? state.selectedSessionID : normalizeSessionID(sessionID);
    if (!targetSessionID) {
      var noSession = "Select a session before sending a message.";
      notifyStatus(options, noSession, "");
      return {outcome: "rejected", error: noSession};
    }
    var draft = state.drafts.get(targetSessionID) || draftFor(state, targetSessionID);
    if (draft.busy) {
      var alreadySending = "This draft is already being sent.";
      notifyStatus(options, alreadySending, targetSessionID);
      return {outcome: "rejected", error: alreadySending};
    }
    if (!draft.text.trim() && draft.images.length === 0) {
      var emptyMessage = "Enter a message or attach an image.";
      notifyStatus(options, emptyMessage, targetSessionID);
      return {outcome: "rejected", error: emptyMessage};
    }

    draft.busy = true;
    notifyChange(state, targetSessionID, options);

    var formData;
    try {
      formData = new options.FormData();
      formData.append("message", draft.text.trim() ? draft.text : NEUTRAL_MESSAGE);
      draft.images.forEach(function (image) {
        formData.append("image", image.file, image.name);
      });

      var response = await options.fetch("/ui/sessions/" + encodeURIComponent(targetSessionID) + "/messages", {
        method: "POST",
        body: formData
      });
      var contentType = response && response.headers && typeof response.headers.get === "function" ? response.headers.get("Content-Type") : "";
      if (typeof contentType !== "string" || contentType.trim().toLowerCase() !== "application/json" || typeof response.json !== "function") {
        throw new Error("malformed response");
      }
      var body = await response.json();
      var result = classifyResponse(response, body);

      if (result.outcome === "accepted") {
        var acceptedDraftIsCurrent = state.drafts.get(targetSessionID) === draft;
        if (acceptedDraftIsCurrent) {
          revokeDraft(draft, options);
          state.drafts.delete(targetSessionID);
          notifyChange(state, targetSessionID, options);
          notifyStatus(options, "", targetSessionID);
        }
        return result;
      }
      if (result.outcome === "rejected") {
        if (state.drafts.get(targetSessionID) === draft) {
          draft.busy = false;
          notifyChange(state, targetSessionID, options);
          notifyStatus(options, result.error, targetSessionID);
        }
        return result;
      }
    } catch (_) {
      // Once fetch is attempted, a transport or response failure cannot prove
      // whether the server accepted the message. Preserve the captured draft.
    }

    if (state.drafts.get(targetSessionID) === draft) {
      draft.busy = false;
      notifyChange(state, targetSessionID, options);
      notifyStatus(options, UNKNOWN_DELIVERY, targetSessionID);
    }
    return {outcome: "unknown"};
  }

  function destroyDrafts(state, options) {
    state.drafts.forEach(function (draft) { revokeDraft(draft, options); });
    state.drafts.clear();
    state.selectedSessionID = "";
  }

  return {LIMITS: LIMITS, NEUTRAL_MESSAGE: NEUTRAL_MESSAGE, createController: createController};
});
