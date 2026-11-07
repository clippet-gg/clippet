// Clippet client (DESIGN.md §9.3). ES2020, no build step, no dependencies.
// Talks only to the §8.1 routes with same-origin credentials. Styling from
// JS goes through element.style.<prop> and classList only — never a style
// attribute string or injected markup (CSP style-src 'self').
(() => {
  "use strict";

  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

  // ---- helpers -----------------------------------------------------------

  // local() turns an absolute URL from a DTO into a same-origin path so the
  // page keeps working when it is browsed under a host other than
  // PUBLIC_BASE_URL (CSP connect-src/media-src are 'self').
  function local(u) {
    try {
      const x = new URL(u, location.href);
      return x.pathname + x.search;
    } catch {
      return u;
    }
  }

  // safeNext only ever redirects to a same-origin absolute path. A backslash
  // is rejected outright: the URL parser folds it to "/" for http(s), so any
  // value carrying one can become an authority (§20 erratum 52).
  function safeNext(v) {
    if (typeof v !== "string" || !v.startsWith("/") || v.startsWith("//") || v.includes("\\")) return "/";
    return v;
  }

  async function api(method, url, body) {
    const init = { method, credentials: "same-origin", headers: {} };
    // §9.3: every mutating call declares application/json, body or not, so
    // a bodiless DELETE / retry / logout never trips a 415 guard (§14).
    if (method !== "GET" && method !== "HEAD") init.headers["Content-Type"] = "application/json";
    if (body !== undefined) init.body = JSON.stringify(body);
    const res = await fetch(url, init);
    let data = null;
    const ct = res.headers.get("Content-Type") || "";
    if (ct.includes("application/json")) {
      try { data = await res.json(); } catch { data = null; }
    }
    return { status: res.status, ok: res.ok, data };
  }

  function messageOf(r, fallback) {
    if (r && r.data && typeof r.data.message === "string") return r.data.message;
    return fallback || `request failed (${r ? r.status : "network"})`;
  }

  // titleSuffix is what the SERVER put after a page's own name in <title>
  // ("Ace round 3 · Clippet"), read off <body data-product>. The client
  // never spells the product name itself: one spelling, one place, and a page
  // whose server title carries no suffix (a collection) gets none here either.
  function titleSuffix() {
    const p = document.body.dataset.product;
    return p ? " · " + p : "";
  }

  // A media object is served `public, max-age=3600`, so the only way to make a
  // browser fetch the NEW bytes at an address it already has is a fresh query.
  // bustQuery is that query; bustURL puts it on a URL, REPLACING whatever query
  // was there. (The GIF preview's own bust appends instead — that URL carries
  // parameters the server reads.)
  function bustQuery() { return "?t=" + Date.now(); }
  function bustURL(url) { return url.split("?")[0] + bustQuery(); }

  function show(el, text) {
    if (!el) return;
    if (text !== undefined) el.textContent = text;
    el.hidden = false;
  }
  function hide(el) { if (el) el.hidden = true; }
  function setText(el, text) { if (el) el.textContent = text; }

  function setBar(bar, p) {
    if (!bar) return;
    if (p.indeterminate) {
      bar.classList.add("indeterminate");
      return;
    }
    bar.classList.remove("indeterminate");
    const pct = Math.max(0, Math.min(100, Number(p.percent) || 0));
    bar.style.width = pct + "%";
  }

  // stageNotes is what a stage means in words — what processing is, and
  // where it happens (the owner's first look; §20 erratum
  // 224). It is the SAME list internal/web's stageNote renders into the row on
  // the server, so the first poll rewrites the line with the sentence it
  // already said rather than a different one; a Go test reads this object out
  // of the shipped script to keep the two in step.
  //
  // A stage this list does not know yields no phrase at all: a stage name is
  // the server's vocabulary, and printing a raw one would be showing the reader
  // an internal identifier — which is precisely what the old ` (encode)` was.
  const stageNotes = {
    acquire: "reading the upload",
    probe: "inspecting the video",
    encode: "encoding on the server",
    copy: "encoding on the server",
    remux: "repacking the upload",
    light: "encoding the card copy",
    verify: "checking the result",
    poster: "making the thumbnail",
    store: "uploading to storage",
    finalize: "finishing up",
    done: "finishing up",
  };

  function stageLabel(p) {
    // `queued` says everything on its own, and the server's row says it with
    // the same word.
    if (p.state === "queued") return "Queued…";
    const note = stageNotes[p.stage] ? ` · ${stageNotes[p.stage]}` : "";
    if (p.indeterminate) return `Processing…${note}`;
    return `Processing… ${Math.max(0, Math.min(100, Number(p.percent) || 0))}%${note}`;
  }

  async function copyText(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      try { await navigator.clipboard.writeText(text); return true; } catch { /* fall through */ }
    }
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "readonly");
    ta.style.position = "fixed";
    ta.style.left = "-1000px";
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand("copy"); } catch { ok = false; }
    ta.remove();
    return ok;
  }

  function flash(el, text) {
    if (!el) return;
    if (text !== undefined) el.textContent = text;
    el.hidden = false;
    setTimeout(() => { el.hidden = true; }, 1500);
  }

  // splitTags mirrors db.SplitTags: the comma-separated line a person types
  // becomes the array the API takes. Empty segments are dropped, so a trailing
  // comma is not an error and an empty box is an empty list — which is how
  // "clear the tags" is spelled on a PATCH.
  function splitTags(value) {
    return String(value || "").split(",").map((s) => s.trim()).filter((s) => s !== "");
  }

  // ---- two-click confirm (§9.4, §20 erratum 95) ---------------------------
  //
  // The ONE way this client asks "are you sure": the button relabels itself
  // and the SECOND click inside armMs does the work; anything else, or five
  // seconds of nothing, puts it back. Never confirm() — a browser dialog is
  // neither styleable nor dismissable by the page, it is the same chrome every
  // site on the machine can raise, and these pages are modal-free by design.
  // alert() is out for the same reason and one more: a failure belongs beside
  // the button that failed, where the next click already is.
  const armMs = 5000;
  const armed = new WeakMap();

  function disarm(btn) {
    const a = armed.get(btn);
    if (!a) return;
    armed.delete(btn);
    clearTimeout(a.timer);
    btn.textContent = a.label;
    hide(a.hint);
  }

  // confirmClick answers "is THIS click the confirming one?". The first click
  // arms the button (relabelled, with its hint shown) and returns false; the
  // second returns true and puts the label back, so the caller's own
  // disabled/enabled handling is all that is left to do.
  function confirmClick(btn, hint, text) {
    if (armed.has(btn)) {
      disarm(btn);
      return true;
    }
    armed.set(btn, { label: btn.textContent, hint, timer: setTimeout(() => disarm(btn), armMs) });
    btn.textContent = text || btn.dataset.confirmText || "Click again to confirm";
    show(hint);
    return false;
  }

  // ---- an expired session (§20 errata 95, 108) ----------------------------
  //
  // A session that ended under an open page turns every call into the API's
  // own "authentication required" — true, unactionable, and printed beside a
  // server-rendered header that still says you are logged in. One place
  // decides what that reads as, and it carries the way out: the login page,
  // with this page as the destination to come back to.

  // sessionEnded covers the two answers a dead credential gives: the 401 of a
  // session that expired or was logged out elsewhere, and the 403
  // token_not_allowed an owner route gives a capture token (§20 erratum 19).
  //
  // ONE 401 is not a dead credential, and the exclusion lives here rather than
  // in a caller so the rule stays in one place (§20 erratum 108):
  // `invalid_credentials` on PATCH /api/auth/password means "the current
  // password you typed is wrong" while the session that carried the request is
  // very much alive. Reporting that as "your session ended" would be false and
  // would offer a login link that bounces straight back to the page.
  function sessionEnded(r) {
    if (!r) return false;
    if (r.status === 401) return !(r.data && r.data.error === "invalid_credentials");
    return r.status === 403 && r.data && r.data.error === "token_not_allowed";
  }

  // authFailed renders that one case into the element a section already uses
  // for its failures and returns true; it returns false for every other
  // failure, so a caller reads:
  //     if (!authFailed(el, r)) show(el, messageOf(r, "…"));
  // The link is built as an element — never a markup string, which the CSP
  // and this file's own rule forbid.
  function authFailed(el, r) {
    if (!sessionEnded(r)) return false;
    if (!el) return true;
    el.textContent = "Your session ended — ";
    const a = document.createElement("a");
    a.href = "/login?next=" + encodeURIComponent(location.pathname + location.search);
    a.textContent = "log in again";
    el.appendChild(a);
    el.hidden = false;
    return true;
  }

  // ---- progress polling (§8.4): 1s, then 2s after 60s; stops on ready /
  // failed / HTTP 404 — three distinct outcomes, 404 is never "done". A
  // hidden tab pauses until it is visible again, and a server that cannot be
  // reached for maxPollFailures consecutive ticks ends the poll with the
  // "unreachable" outcome (a reload starts over). -----------------------------

  const maxPollFailures = 60;
  const unreachableText = "Could not reach the server — reload the page to keep watching.";

  // stateMoment: the mark reacts to a job's state CHANGING under a poll — into
  // the queue, onto the worker. The outcomes (ready, failed) are the done
  // handlers' to name, once, beside the reload or the failure line.
  function stateMoment(was, now) {
    if (now === was) return;
    if (now === "queued") moment("queued");
    else if (now === "processing") moment("encoding");
  }

  function poll(url, handlers) {
    const start = Date.now();
    let stopped = false;
    let failures = 0;
    const finish = (outcome, p) => {
      stopped = true;
      handlers.done(outcome, p);
    };
    const tick = async () => {
      if (stopped) return;
      if (document.visibilityState === "hidden") {
        document.addEventListener("visibilitychange", tick, { once: true });
        return;
      }
      let reached = false;
      try {
        const res = await fetch(url, { credentials: "same-origin", cache: "no-store" });
        if (res.status === 404) return finish("gone", null);
        if (res.ok) {
          const p = await res.json();
          reached = true;
          handlers.update(p);
          if (p.state === "ready" || p.state === "failed") return finish(p.state, p);
        }
        // any other status (503 etc.) is transient: keep polling
      } catch {
        // network hiccup: keep polling
      }
      failures = reached ? 0 : failures + 1;
      if (failures >= maxPollFailures) return finish("unreachable", null);
      setTimeout(tick, Date.now() - start >= 60000 ? 2000 : 1000);
    };
    tick();
    return () => { stopped = true; };
  }

  // ---- auth forms ----------------------------------------------------------

  function setupAuthForm(form) {
    const err = $("[data-error]", form);
    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      hide(err);
      const body = {
        username: form.elements.username.value.trim(),
        password: form.elements.password.value,
      };
      const button = $("button[type=submit]", form);
      if (button) button.disabled = true;
      let r;
      try {
        r = await api("POST", form.getAttribute("action"), body);
      } catch {
        r = null;
      }
      if (button) button.disabled = false;
      if (r && r.ok) {
        carry("signedin"); // the library that comes next smiles
        location.href = safeNext(form.elements.next ? form.elements.next.value : "/");
        return;
      }
      show(err, messageOf(r, "could not reach the server"));
    });
  }

  function setupLogout(button) {
    button.addEventListener("click", async () => {
      try { await api("POST", "/api/auth/logout"); } catch { /* ignore */ }
      location.href = "/login";
    });
  }

  // ---- upload (§8.3): metadata part FIRST, then the video; XHR for
  // upload.onprogress; "Saving…" between 100% sent and the 201.
  //
  // Several files, drag-and-drop, cancel and retry are §20 erratum 139. The
  // batch is uploaded SEQUENTIALLY — one request at a time keeps
  // RATE_UPLOAD_PER_HOUR, the scratch precheck and the single ffmpeg slot
  // honest, and six parallel POSTs would only make each of them slower — and
  // every file in it shares one metadata document, so the tags and the
  // lifetime are typed once for the evening's clips. ------------------------

  // maxBatch bounds the queue. It is not a server limit (there is none): it is
  // the point past which "I dropped a folder in by accident" is the likelier
  // explanation than "I meant to upload 200 files", and the message says so.
  const maxBatch = 20;

  function setupUpload(form) {
    const status = $("[data-upload-status]", form);
    const progress = $("[data-upload-progress]", form);
    const bar = $("[data-upload-bar]", form);
    const submit = $("[data-upload-submit]", form);
    const cancelBtn = $("[data-upload-cancel]", form);
    const retryBtn = $("[data-upload-retry]", form);
    const anywayBtn = $("[data-upload-anyway]", form);
    // The drop frame's readout (web/STYLE.md): the file's name in mono, the
    // amber lamp beside the stage phrase while something is in flight, and the
    // batch's size in the Upload row's state cell.
    const nameEl = $("[data-upload-name]", form);
    const lamp = $("[data-upload-lamp]", form);
    const sizeEl = $("[data-upload-size]", form);
    const input = form.elements.video;
    const list = $("[data-clip-list]");
    // The drop target is the whole upload card, not the file input: a file
    // dragged from a folder lands somewhere near the form, never on a 20-pixel
    // control.
    const zone = form.closest(".upload") || form;
    // The JS path posts the §8.3 metadata-first grammar to /api/clips, which
    // answers with the whole ClipDTO the new library row is built from. The
    // form's own `action` is the capture endpoint, whose grammar a plain
    // browser form already fits — that is the no-JS path, and it is not this
    // one (§20 erratum 139).
    const postURL = form.dataset.uploadApi || form.getAttribute("action");
    // The chunked door (§20 erratum 344): a file larger than one chunk goes up
    // through the session routes, because the edge in front of a public name
    // refuses any one request body over its plan's cap. The page carries the
    // server's chunk size; the session's own answer is what the chunks follow.
    const sessionURL = form.dataset.uploadSessions || "";
    const chunkBytes = Number(form.dataset.uploadChunk) || 0;

    // Progressive enhancement, and the reason the served HTML has no
    // `multiple`: without this script a second file would be sent and then
    // silently ignored, because the parts after the first file part are never
    // read (§20 erratum 58). With it, the extra files are uploaded one by one.
    input.multiple = true;

    let picked = [];   // the batch as chosen, in the order it will be sent
    let queue = [];    // what is left to send; queue[0] is the one in flight
    let meta = null;   // the metadata document shared by the whole batch
    let baseTitle = ""; // the typed title, before the "(2/5)" suffix
    let total = 0;     // how many files this batch started with
    let live = null;   // the XHR in flight, or null
    // The duplicate answer (§20 erratum 326): a file this account already holds,
    // byte for byte, comes back 200 with the existing clip and is not uploaded
    // again. The batch carries on; the files it skipped are named at its end
    // beside "Upload anyway", which sends exactly those again with the opt-out.
    let dupes = [];    // [{file, dto}] answered as already in the library
    let held = [];     // the files Upload anyway would send
    let anyway = false; // this batch sends the opt-out

    // filesFrom takes a FileList (a picker, a drop, a paste) and keeps the
    // batch. It also tries to reflect the choice back into the file input so
    // the control shows what was dropped; DataTransfer is not constructible
    // everywhere, and where it is not the batch still works — `picked` is the
    // source of truth, never input.files.
    function filesFrom(fileList) {
      const files = Array.from(fileList || []);
      if (!files.length) return false;
      picked = files.slice(0, maxBatch);
      try {
        const dt = new DataTransfer();
        picked.forEach((f) => dt.items.add(f));
        input.files = dt.files;
      } catch { /* the input keeps whatever it had; picked is what we send */ }
      if (files.length > maxBatch) {
        setText(status, `Taking the first ${maxBatch} of ${files.length} files.`);
      } else {
        setText(status, files.length > 1 ? `${files.length} files ready.` : "");
      }
      showPicked();
      return true;
    }

    // showPicked names what is about to go up: one file's name, or the count,
    // and the batch's size in the state cell.
    function showPicked() {
      const files = picked.length ? picked : Array.from(input.files || []);
      setName(files.length === 1 ? files[0].name : (files.length > 1 ? `${files.length} files` : ""));
      const bytes = files.reduce((n, f) => n + (f.size || 0), 0);
      setText(sizeEl, bytes > 0 ? humanBytes(bytes) : "");
      loud();
    }

    // setName writes the readout's name cell, which ellipsises (app.css): the
    // whole name rides along as its `title`, and an empty cell carries none.
    function setName(text) {
      setText(nameEl, text);
      if (!nameEl) return;
      if (text) nameEl.title = text;
      else nameEl.removeAttribute("title");
    }

    // loud keeps one solid control on the card (web/STYLE.md, §20 erratum
    // 313): Pick a file while there is nothing to send, Upload once there is.
    // Every path that fills or empties the batch ends here.
    const pickEl = $("[data-upload-pick]", form);
    function loud() {
      const has = picked.length > 0 || (input.files && input.files.length > 0);
      if (submit) submit.classList.toggle("primary", has);
      if (pickEl) pickEl.classList.toggle("primary", !has);
    }
    // A browser that restores form state on Back may hand the page a file
    // already chosen; the card starts in whichever state that is.
    loud();

    input.addEventListener("change", () => {
      picked = Array.from(input.files || []).slice(0, maxBatch);
      if (picked.length > 1) setText(status, `${picked.length} files ready.`);
      else setText(status, "");
      showPicked();
      hide(retryBtn);
    });

    // Drag and drop. dragover must be cancelled for a drop to fire at all;
    // the class is the only feedback, and it goes on a container the CSS
    // owns rather than on an inline style (CSP style-src 'self').
    ["dragenter", "dragover"].forEach((name) => {
      zone.addEventListener(name, (ev) => {
        if (!ev.dataTransfer) return;
        ev.preventDefault();
        ev.dataTransfer.dropEffect = "copy";
        zone.classList.add("dropping");
      });
    });
    ["dragleave", "dragend"].forEach((name) => {
      zone.addEventListener(name, (ev) => {
        if (ev.target === zone) zone.classList.remove("dropping");
      });
    });
    zone.addEventListener("drop", (ev) => {
      if (!ev.dataTransfer || !ev.dataTransfer.files || !ev.dataTransfer.files.length) return;
      ev.preventDefault();
      zone.classList.remove("dropping");
      if (filesFrom(ev.dataTransfer.files)) hide(retryBtn);
    });
    // Paste: a screen recorder that copies its file, or a file copied in the
    // file manager. Only when the focus is not in a text field, so pasting a
    // title never steals the paste.
    document.addEventListener("paste", (ev) => {
      const t = ev.target;
      if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA")) return;
      if (!ev.clipboardData || !ev.clipboardData.files || !ev.clipboardData.files.length) return;
      if (filesFrom(ev.clipboardData.files)) hide(retryBtn);
    });

    // uploading() flips the controls: while a batch is in flight the submit
    // button is replaced by Cancel, which is the button the roadmap's dead
    // xhr.onabort handler was always waiting for.
    function uploading(on) {
      if (submit) submit.disabled = on;
      if (cancelBtn) cancelBtn.hidden = !on;
      if (lamp) lamp.hidden = !on;
      if (on) hide(retryBtn);
    }

    // batchTitle names one file of the batch. A single upload keeps the title
    // exactly as typed (that is the contract POST /api/clips has always had);
    // several share it with a "(2/5)" suffix, trimmed so the result can never
    // exceed the API's 200-rune cap and come back a 400.
    function batchTitle(index) {
      if (!baseTitle) return "";
      if (total < 2) return baseTitle;
      const suffix = ` (${index + 1}/${total})`;
      return baseTitle.slice(0, 200 - suffix.length) + suffix;
    }

    function metaFor(index) {
      const m = Object.assign({}, meta);
      const t = batchTitle(index);
      if (t) m.title = t;
      return m;
    }

    // showDupes ends a batch that skipped files already in the library: the
    // line, a link to each existing clip (its own title), and Upload anyway.
    function showDupes() {
      setText(status, "Already in your library.");
      if (status) dupes.forEach((d) => {
        status.appendChild(document.createTextNode(" "));
        const a = document.createElement("a");
        a.href = local(d.dto.urls.page);
        a.textContent = d.dto.title;
        status.appendChild(a);
      });
      held = dupes.map((d) => d.file);
      dupes = [];
      show(anywayBtn);
    }

    function label(index, file, tail) {
      setName(file.name);
      const which = total > 1 ? `${index + 1} of ${total} · ` : "";
      return `${which}${tail}`;
    }

    // sendNext uploads queue[0] and, on success, recurses into the rest. The
    // recursion is what makes the batch sequential; nothing is concurrent and
    // nothing overlaps.
    function sendNext() {
      const index = total - queue.length;
      const file = queue[0];
      if (!file) {
        // The whole batch landed: only now is the form reset, because the
        // metadata document belongs to all of it.
        uploading(false);
        hide(progress);
        picked = [];
        form.reset();
        setText(sizeEl, "");
        loud();
        anyway = false;
        if (dupes.length) showDupes();
        else setText(status, total > 1 ? `Uploaded ${total} clips — encoding on the server` : "Uploaded — encoding on the server");
        total = 0;
        return;
      }
      const fail = (msg) => {
        live = null;
        uploading(false);
        hide(progress);
        setText(status, msg);
        // The file is still in hand, so the answer to a failure is one button
        // and not "pick it again": `queue` still starts with the file that
        // failed, and Try again resumes from exactly there.
        show(retryBtn);
      };
      // answer is the end of one file, whichever door it went through: the
      // one-shot's 201, or the session's finish, which is the same answer.
      const answer = (code, dto) => {
        live = null;
        if (code === 200 && dto && dto.id && dto.existing) {
          // Already in the library: nothing landed, no row is added, and the
          // batch carries on with the next file.
          queue.shift();
          dupes.push({ file, dto });
          sendNext();
          return;
        }
        if (code !== 201 || !dto || !dto.id) {
          // The answer is shaped for authFailed the way an api() result is, so
          // a 401 that slipped past the probe (a logout in another tab
          // mid-transfer) reads the same as every other one.
          if (authFailed(status, { status: code, data: dto })) {
            uploading(false);
            hide(progress);
            return;
          }
          const msg = dto && dto.message ? dto.message : `upload failed (${code})`;
          fail(`Upload failed: ${msg}`);
          return;
        }
        queue.shift();
        // the mark: the upload landed, then the state the row arrives in
        moment("landed");
        stateMoment("", dto.state);
        if (list) {
          const row = buildRow(dto);
          list.prepend(row);
          hide($("[data-empty]"));
          libraryWatch(row);
        }
        sendNext();
      };
      show(progress);
      bar.classList.remove("indeterminate");
      bar.style.width = "0%";
      setText(status, label(index, file, "uploading 0%"));
      // The bar and the words are the same for both doors: bytes on the wire
      // over the file's size.
      const shown = (loaded) => {
        const pct = Math.min(100, Math.floor((loaded / (file.size || 1)) * 100));
        bar.style.width = pct + "%";
        setText(status, label(index, file, pct < 100 ? `uploading ${pct}%` : "saving…"));
        return pct;
      };
      if (sessionURL && chunkBytes > 0 && file.size > chunkBytes) {
        sendChunked(file, metaFor(index), anyway, shown, fail, answer);
        return;
      }

      const fd = new FormData();
      fd.append("metadata", new Blob([JSON.stringify(metaFor(index))], { type: "application/json" }));
      // The duplicate check's opt-out sits between the metadata and the video
      // (§20 erratum 326); only Upload anyway sends it.
      if (anyway) fd.append("duplicate", "allow");
      fd.append("video", file, file.name);

      const xhr = new XMLHttpRequest();
      live = xhr;
      xhr.open("POST", postURL);
      xhr.responseType = "json";

      // sentAll marks the moment the whole body is on the wire. From there a
      // cancel is a race the client cannot win reliably — the server may
      // already have stored the original and committed the row, and it goes on
      // to answer whether or not anybody is listening — so the message says so
      // rather than claiming an undo that did not happen.
      let sentAll = false;
      xhr.upload.onprogress = (e) => {
        if (!e.lengthComputable) return;
        if (shown((e.loaded / e.total) * file.size) >= 100) sentAll = true;
      };
      xhr.onerror = () => fail(`Upload failed: network error (${file.name})`);
      xhr.onabort = () => fail(sentAll ? cancelledLate : "Upload cancelled");
      xhr.onload = () => answer(xhr.status, xhr.response);
      xhr.send(fd);
    }

    const cancelledLate = "Upload cancelled — the file had already been sent, so it may still appear in your library";

    // sendChunked is the session door (§20 erratum 344): open, the chunks in
    // order, finish. Its finish answers what the one-shot POST answers, so it
    // ends in the same `answer`. A chunk that fails is tried again — up to
    // three times in a row, each time from where the server says it is (GET
    // received) — before the upload fails; Cancel abandons the session. `live`
    // is an object with the XHR's abort(), so Cancel needs no second path.
    function sendChunked(file, m, allowDup, shown, fail, answer) {
      const open = Object.assign({}, m, { size: file.size, name: file.name, type: file.type || "" });
      if (allowDup) open.duplicate = "allow";
      let session = null;
      let xhr = null;
      let stopped = false;
      let sentAll = false;
      let tries = 0;
      const job = {
        abort() {
          if (stopped) return;
          stopped = true;
          if (xhr) xhr.abort();
          if (session) api("DELETE", `${sessionURL}/${session.id}`).catch(() => {});
          fail(sentAll ? cancelledLate : "Upload cancelled");
        },
      };
      live = job;
      // letGo abandons the session on the server (DELETE; while its finish
      // runs, the server lets it go when that returns). Every give-up calls it
      // — a refused chunk, three lost chunks in a row, a refused or never
      // answered finish — so the stored chunks never wait for the session's
      // expiry and Try again starts clean (§20 erratum 344 (k), R18-6).
      const letGo = () => {
        if (session) api("DELETE", `${sessionURL}/${session.id}`).catch(() => {});
      };
      const end = (code, dto) => {
        if (stopped) return;
        stopped = true;
        if (code !== 200 && code !== 201) letGo();
        answer(code, dto);
      };
      const lost = () => {
        if (stopped) return;
        stopped = true;
        letGo();
        fail(`Upload failed: network error (${file.name})`);
      };
      const transient = (code) => code === 0 || code === 408 || code === 429 || code >= 500;

      // put sends chunk i with its own XHR, for the bar.
      const put = (i) => new Promise((resolve) => {
        const start = i * session.chunk_bytes;
        const body = file.slice(start, Math.min(file.size, start + session.chunk_bytes));
        xhr = new XMLHttpRequest();
        xhr.open("PUT", `${sessionURL}/${session.id}/${i}`);
        xhr.responseType = "json";
        xhr.upload.onprogress = (e) => {
          if (!e.lengthComputable || stopped) return;
          if (shown(start + e.loaded) >= 100) sentAll = true;
        };
        xhr.onload = () => resolve({ status: xhr.status, data: xhr.response });
        xhr.onerror = () => resolve({ status: 0, data: null });
        xhr.onabort = () => resolve({ status: 0, data: null });
        xhr.send(body);
      });
      const pause = (ms) => new Promise((r) => setTimeout(r, ms));
      // where asks the server where the session stands.
      const where = async () => {
        try { return await api("GET", `${sessionURL}/${session.id}`); } catch { return { status: 0, data: null }; }
      };
      const finish = async () => {
        try { return await api("POST", `${sessionURL}/${session.id}/finish`); } catch { return { status: 0, data: null }; }
      };

      (async () => {
        let r;
        try { r = await api("POST", sessionURL, open); } catch { r = { status: 0, data: null }; }
        if (stopped) return;
        if (r.status === 0) { lost(); return; }
        if (r.status !== 201 || !r.data || !r.data.id) { end(r.status, r.data); return; }
        session = r.data;
        let next = session.received || 0;
        const chunks = Math.ceil(file.size / session.chunk_bytes);
        while (next < chunks) {
          const got = await put(next);
          if (stopped) return;
          if (got.status === 200 && got.data) {
            tries = 0;
            next = got.data.received;
            continue;
          }
          if (got.status === 409 && got.data && Number.isInteger(got.data.received)) {
            next = got.data.received;
            continue;
          }
          if (!transient(got.status)) { end(got.status, got.data); return; }
          tries++;
          if (tries > 3) { lost(); return; }
          await pause(1000 * tries);
          if (stopped) return;
          const at = await where();
          if (stopped) return;
          if (at.status === 200 && at.data) next = at.data.received;
          else if (!transient(at.status)) { end(at.status, at.data); return; }
        }
        sentAll = true;
        shown(file.size);
        // The finish. Its work can outlast the edge's wait for an answer (the
        // edge then answers in the server's place); the server carries on
        // regardless, so a lost answer is asked for again by GET until the
        // session says it made the clip.
        r = await finish();
        for (let n = 0; transient(r.status) && n < 200; n++) {
          if (stopped) return;
          await pause(3000);
          if (stopped) return;
          const at = await where();
          if (at.status === 200 && at.data && at.data.clip) {
            r = { status: at.data.clip.existing ? 200 : 201, data: at.data.clip };
            break;
          }
          // Nothing is running and nothing was made: finish again.
          if (at.status === 200 && at.data && !at.data.finishing) r = await finish();
          else if (at.status !== 200 && !transient(at.status)) r = at;
        }
        if (stopped) return;
        // A refused or never-answered finish leaves the session's bytes on the
        // server; Try again starts a fresh one, so both let this one go
        // (`lost` and `end` call letGo).
        if (r.status === 0) { lost(); return; }
        end(r.status, r.data);
      })();
    }

    if (cancelBtn) {
      cancelBtn.addEventListener("click", () => {
        // Nothing in flight: do nothing at all. The button is hidden then, and
        // a click that RACED the last 201 must not overwrite "Uploaded" with
        // "Upload cancelled".
        if (!live && !queue.length) return;
        // The rest of the batch is dropped — a cancel means stop, not "carry
        // on with the others" — while the file that was interrupted stays at
        // the head of the queue, so Try again re-sends THAT one without asking
        // for it again. Both happen before the abort, because abort() fires
        // its event synchronously and `fail` reads what is left.
        queue = queue.slice(0, 1);
        total = queue.length;
        if (live) {
          live.abort(); // → onabort → fail("Upload cancelled") → Try again
          return;
        }
        uploading(false);
        hide(progress);
        setText(status, "Upload cancelled");
        show(retryBtn);
      });
    }

    if (anywayBtn) {
      anywayBtn.addEventListener("click", () => {
        if (!held.length || live) return;
        hide(anywayBtn);
        queue = held;
        held = [];
        total = queue.length;
        anyway = true;
        uploading(true);
        sendNext();
      });
    }

    if (retryBtn) {
      retryBtn.addEventListener("click", () => {
        if (!queue.length) return;
        hide(retryBtn);
        uploading(true);
        sendNext();
      });
    }

    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      if (submit && submit.disabled) return;
      const files = picked.length ? picked : Array.from(input.files || []).slice(0, maxBatch);
      if (!files.length) return;
      uploading(true);
      hide(anywayBtn);
      dupes = [];
      held = [];
      anyway = false;

      meta = {
        description: form.elements.description.value.trim(),
        unlisted: form.elements.unlisted.checked,
      };
      baseTitle = form.elements.title.value.trim();
      // Tags travel with the upload, so a clip is filed the moment it exists.
      // An empty box sends nothing at all rather than an empty array: absent
      // and [] mean the same thing on a new clip, and not sending the key
      // keeps the metadata document minimal.
      if (form.elements.tags) {
        const tags = splitTags(form.elements.tags.value);
        if (tags.length) meta.tags = tags;
      }
      const ttl = form.elements.ttl_days.value;
      if (ttl !== "") meta.ttl_days = Number(ttl); // "" = absent → server default; "0" = never
      // The link grammar of this batch (§20 erratum 250). A RadioNodeList's
      // `.value` is the checked radio's, and one is always checked — the server
      // rendered the account's own default as checked — so this sends the same
      // answer silence would have produced, explicitly.
      const style = form.elements.link_style;
      if (style && style.value) meta.link_style = style.value;

      // A session that died while this page sat open turns the whole transfer
      // into a 401 AFTER the last byte is on the wire — a gigabyte of upstream
      // spent to be told to log in. One bodiless GET decides it first, so a
      // dead session costs nothing (§20 erratum 95). A probe that cannot be
      // REACHED proves nothing and is not allowed to block the upload: the
      // XHR reports a network failure perfectly well on its own.
      let me;
      try { me = await api("GET", "/api/auth/me"); } catch { me = null; }
      if (me && !me.ok) {
        uploading(false);
        if (!authFailed(status, me)) setText(status, "Upload failed: " + messageOf(me));
        return;
      }

      queue = files.slice();
      total = queue.length;
      sendNext();
    });
  }

  // buildRow mirrors the server-rendered "cliprow" template for a fresh 201
  // ClipDTO (the uploader is always the owner, so the view count is shown).
  function buildRow(dto) {
    const li = document.createElement("li");
    li.className = "clip-row";
    li.dataset.clipId = dto.id;
    li.dataset.state = dto.state;
    li.dataset.progressUrl = local(dto.urls.progress);
    // The uploader owns it, so it is selectable like every other own row
    // (§20 erratum 117). The checkbox is styled away until the bulk bar is
    // live, exactly as the template's is.
    li.dataset.own = "";
    const pick = document.createElement("label");
    pick.className = "pick";
    const box = document.createElement("input");
    box.type = "checkbox";
    box.dataset.pick = "";
    box.value = dto.id;
    pick.appendChild(box);
    const pickLabel = document.createElement("span");
    pickLabel.className = "sr-only";
    pickLabel.textContent = "Select " + dto.title;
    pick.appendChild(pickLabel);
    li.appendChild(pick);

    const thumbBox = document.createElement("span");
    thumbBox.className = "thumb";
    const thumb = document.createElement("a");
    thumb.className = "frame empty";
    thumb.href = local(dto.urls.page);
    thumb.dataset.thumb = "";
    const badge = document.createElement("span");
    badge.className = "badge badge-" + dto.state;
    badge.dataset.badge = "";
    badge.textContent = dto.state;
    thumb.appendChild(badge);
    thumbBox.appendChild(thumb);
    // The rail under the thumbnail: dashed while queued, amber while working
    // (web/STYLE.md). rowUpdate swaps the class and setBar writes the fill.
    const rail = document.createElement("div");
    rail.className = "rail " + (dto.state === "queued" ? "queued" : "working");
    rail.dataset.rail = "";
    const bar = document.createElement("i");
    bar.dataset.progressBar = "";
    rail.appendChild(bar);
    thumbBox.appendChild(rail);
    li.appendChild(thumbBox);

    const info = document.createElement("div");
    info.className = "info";
    const title = document.createElement("a");
    title.className = "title";
    title.href = local(dto.urls.page);
    title.dataset.title = "";
    title.title = dto.title; // the row clips with an ellipsis; the tooltip is the whole name
    title.textContent = dto.title;
    info.appendChild(title);

    const sub = document.createElement("div");
    sub.className = "sub";
    const owner = document.createElement("span");
    owner.textContent = "by " + (dto.owner && dto.owner.username ? dto.owner.username : "you");
    sub.appendChild(owner);
    const uploaded = document.createElement("span");
    uploaded.textContent = fmtDate(dto.created_at);
    sub.appendChild(uploaded);
    const expiry = document.createElement("span");
    expiry.dataset.expiry = "";
    expiry.textContent = expiryText(dto);
    sub.appendChild(expiry);
    if (dto.unlisted) {
      const flag = document.createElement("span");
      flag.className = "flag";
      flag.textContent = "unlisted";
      sub.appendChild(flag);
    }
    const views = document.createElement("span");
    const n = typeof dto.views === "number" ? dto.views : 0;
    views.dataset.views = String(n);
    views.textContent = `${n} view${n === 1 ? "" : "s"}`;
    sub.appendChild(views);
    info.appendChild(sub);

    // The tag chips, exactly as "cliprow" renders them: links back into the
    // library filter, no counts.
    if (Array.isArray(dto.tags) && dto.tags.length) {
      const tags = document.createElement("div");
      tags.className = "tags";
      tags.dataset.tags = "";
      dto.tags.forEach((t) => {
        const a = document.createElement("a");
        a.className = "tag";
        a.href = "/?tag=" + encodeURIComponent(t);
        a.textContent = t;
        tags.appendChild(a);
      });
      info.appendChild(tags);
    }

    const status = document.createElement("p");
    status.className = "status";
    status.dataset.status = "";
    status.textContent = "Queued…";
    info.appendChild(status);

    li.appendChild(info);
    // A queued clip has no measurements yet, so this normally writes nothing;
    // it is here so the ONE function that paints a row's badges is the one
    // that paints them (a 201 from a route that ever carried them would be
    // rendered, not ignored).
    applyRowFacts(li, dto);
    return li;
  }

  // expiryText mirrors the server's web.expiryText for a ClipDTO the page
  // just received, so a lifetime updated in place reads exactly like one
  // rendered by the template. (The server's third case — an expiry already
  // past, "pending deletion" — cannot follow a touch or a keep-forever: both
  // leave the clip either well in the future or with no expiry at all.)
  function expiryText(dto) {
    return dto.expires_at ? "expires " + fmtDate(dto.expires_at) : "kept forever";
  }

  // ---- local time and a live countdown (ROADMAP #8, §20 erratum 133) -----
  //
  // The server renders one absolute UTC sentence per timestamp and that does
  // not change: it is what a reader with no JavaScript sees, what a crawler
  // reads, and what stays on the page if any of this fails. Each of them is
  // now wrapped in <time datetime="{RFC3339}">, which is the machine-readable
  // half, so this file can say the same instant in the viewer's OWN locale
  // and — for an expiry — how long is left, which is the one number a
  // per-clip-TTL product exists to communicate.
  //
  // Three rules. The countdown appears only under a DAY: above that the date
  // is the useful answer and "in 27 days" is noise. It re-renders once a
  // minute and only while the tab is visible, because a background tab
  // burning a timer to rewrite text nobody is reading is rude. And a <time>
  // whose datetime this browser cannot parse is LEFT ALONE, so a bad value
  // degrades to the server's UTC string rather than to an empty line.

  const timeTickMs = 60000;
  const dayMs = 86400000;
  let timeTimer = null;

  function fmtLocalTime(d) {
    try {
      return d.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
    } catch {
      return fmtDate(d.toISOString());
    }
  }

  // countdownText is the phrase appended to an expiry, or "" when the moment
  // is a day or more off. Minutes below an hour, hours below a day: the
  // resolution people actually act on.
  //
  // The boundary is `>=`, not `>`: the documented rule (DESIGN §20 erratum 133
  // and the comment above) is "only UNDER a day", and at exactly 86400000 ms a
  // strict `>` let the phrase through as "in 24 hours" — a countdown saying
  // the same thing the date beside it already says. Every step FLOORS for the
  // same reason: Math.round turned everything from 23 h 30 m upward into "in
  // 24 hours", so the phrase the boundary exists to prevent came back through
  // the arithmetic. Flooring can only ever understate the time left, which is
  // the safe direction for a deadline.
  function countdownText(ms) {
    if (!Number.isFinite(ms) || ms >= dayMs) return "";
    if (ms <= 0) return " (expiring now)";
    const mins = Math.floor(ms / 60000);
    if (mins < 1) return " (in under a minute)";
    if (mins < 60) return ` (in ${mins} minute${mins === 1 ? "" : "s"})`;
    const hours = Math.floor(mins / 60);
    return ` (in ${hours} hour${hours === 1 ? "" : "s"})`;
  }

  function decorateTimes(root) {
    let wantsTicker = false;
    $$("time[data-local]", root || document).forEach((el) => {
      const iso = el.getAttribute("datetime");
      const d = iso ? new Date(iso) : null;
      if (!d || Number.isNaN(d.getTime())) return; // keep the server's own text
      let rel = "";
      if (el.hasAttribute("data-countdown")) {
        rel = countdownText(d.getTime() - Date.now());
        wantsTicker = true;
      }
      el.textContent = fmtLocalTime(d) + rel;
    });
    if (wantsTicker) startTimeTicker();
  }

  function startTimeTicker() {
    if (timeTimer !== null || document.visibilityState === "hidden") return;
    timeTimer = setInterval(() => decorateTimes(document), timeTickMs);
  }

  function stopTimeTicker() {
    if (timeTimer === null) return;
    clearInterval(timeTimer);
    timeTimer = null;
  }

  function setupTimes() {
    if (!$("time[data-local]")) return;
    document.addEventListener("visibilitychange", () => {
      if (document.visibilityState === "hidden") {
        stopTimeTicker();
        return;
      }
      decorateTimes(document); // catch up on whatever the hidden tab missed
    });
    decorateTimes(document);
  }

  function fmtDate(iso) {
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return "";
    const p = (n) => String(n).padStart(2, "0");
    return `${d.getUTCFullYear()}-${p(d.getUTCMonth() + 1)}-${p(d.getUTCDate())} ${p(d.getUTCHours())}:${p(d.getUTCMinutes())} UTC`;
  }

  // durationText mirrors the server's web.formatDuration so a badge this file
  // writes reads exactly like one the template rendered (ROADMAP #30).
  function durationText(ms) {
    const total = Math.round(Number(ms) / 1000);
    if (!Number.isFinite(total) || total < 0) return "";
    const h = Math.floor(total / 3600);
    const m = Math.floor((total % 3600) / 60);
    const s = total % 60;
    const p = (n) => String(n).padStart(2, "0");
    return h > 0 ? `${h}:${p(m)}:${p(s)}` : `${m}:${p(s)}`;
  }

  // applyRowFacts writes the two measured badges of a library row — the
  // duration over the thumbnail, the resolution beside the date — from a
  // ClipDTO. It is idempotent and it writes nothing it was not given, so the
  // row built for a fresh upload (no measurements yet) and the same row a
  // minute later (all of them) go through one function.
  function applyRowFacts(row, dto) {
    if (!dto) return;
    const thumb = $("[data-thumb]", row);
    const text = dto.duration_ms ? durationText(dto.duration_ms) : "";
    if (thumb && text) {
      let dur = $("[data-duration]", row);
      if (!dur) {
        dur = document.createElement("span");
        dur.className = "duration";
        dur.dataset.duration = "";
        thumb.appendChild(dur);
      }
      dur.textContent = text;
    }
    const sub = $(".sub", row);
    if (!sub || !dto.width || !dto.height) return;
    let res = $("[data-resolution]", row);
    if (!res) {
      res = document.createElement("span");
      res.dataset.resolution = "";
      const after = $("[data-expiry]", row);
      sub.insertBefore(res, after ? after.nextSibling : null);
    }
    res.textContent = `${dto.width}×${dto.height}`;
  }

  // fillRowFacts asks for the clip once, at the moment it becomes ready. A
  // progress poll carries a JOB's progress and never a clip's dimensions, so
  // a row that has just finished encoding has no measurements in the page;
  // one GET fills them in, and the clip uploaded a minute ago then reads like
  // every other row without a reload. One request per clip, once.
  async function fillRowFacts(row) {
    let r;
    try { r = await api("GET", "/api/clips/" + encodeURIComponent(row.dataset.clipId)); } catch { return; }
    if (r && r.ok && r.data) applyRowFacts(row, r.data);
  }

  // rowView is the handful of elements one library row's progress writes to.
  function rowView(row) {
    return {
      row,
      bar: $("[data-progress-bar]", row),
      rail: $("[data-rail]", row),
      status: $("[data-status]", row),
      badge: $("[data-badge]", row),
    };
  }

  function rowUpdate(v, p) {
    stateMoment(v.row.dataset.state, p.state);
    v.row.dataset.state = p.state;
    if (v.badge) {
      v.badge.textContent = p.state;
      v.badge.className = "badge badge-" + p.state;
    }
    setRail(v.rail, p);
    setBar(v.bar, p);
    setText(v.status, stageLabel(p));
  }

  // setRail flips a rail between the two states a job can be in: dashed while
  // nothing has claimed it, amber while the server works (web/STYLE.md).
  function setRail(rail, p) {
    if (!rail) return;
    const queued = p.state === "queued";
    rail.classList.toggle("queued", queued);
    rail.classList.toggle("working", !queued);
  }

  function rowDone(v, outcome, p) {
    const progress = v.bar ? v.bar.parentElement : null;
    if (progress) progress.remove();
    v.row.removeAttribute("data-progress-url");
    if (outcome === "ready") {
      moment("ready");
      if (v.status) v.status.remove();
      v.row.dataset.state = "ready";
      const thumb = $("[data-thumb]", v.row);
      if (thumb) {
        const img = document.createElement("img");
        img.alt = "";
        img.loading = "lazy";
        img.src = `/api/clips/${encodeURIComponent(v.row.dataset.clipId)}/poster.jpg`;
        thumb.replaceChildren(img);
        thumb.classList.remove("empty");
      }
      fillRowFacts(v.row);
    } else if (outcome === "failed") {
      moment("failed");
      setText(v.status, "Processing failed: " + ((p && p.error) || "unknown error"));
    } else if (outcome === "unreachable") {
      setText(v.status, unreachableText);
    } else {
      setText(v.status, "This clip no longer exists.");
    }
  }

  // ---- library: ONE poll for the whole page (§20 erratum 116) -------------
  //
  // Every unfinished row used to start a poll of its own, so a ShareX
  // watch-folder drop of forty files made one open tab issue ~40 requests a
  // second at a box that was simultaneously running ffmpeg. There is now one
  // timer for the page and one request per tick naming every row still
  // working — and no timer at all when none is: rows leave the set as they
  // finish, and the last one to leave stops the loop.
  //
  // The timing rules are `poll`'s, because they are the same rules: 1 s, then
  // 2 s after a minute; a hidden tab waits for visibilitychange instead of
  // polling into the void; maxPollFailures consecutive unreachable ticks end
  // the loop with the "unreachable" outcome for every row still in it.
  const libraryPollMax = 100; // = db.LibraryPageSize (pinned by internal/web's static test)

  function watchLibrary(list) {
    const rows = new Map(); // clip id → rowView
    let ticking = false;
    let start = 0;
    let failures = 0;

    const finish = (id, outcome, p) => {
      const v = rows.get(id);
      rows.delete(id);
      if (v) rowDone(v, outcome, p);
    };

    const tick = async () => {
      if (!rows.size) { ticking = false; return; }
      if (document.visibilityState === "hidden") {
        document.addEventListener("visibilitychange", tick, { once: true });
        return;
      }
      const ids = Array.from(rows.keys()).slice(0, libraryPollMax);
      let reached = false;
      try {
        const url = "/api/clips/progress?ids=" + ids.map(encodeURIComponent).join(",");
        const res = await fetch(url, { credentials: "same-origin", cache: "no-store" });
        if (res.ok) {
          const body = await res.json();
          const map = (body && body.progress) || {};
          reached = true;
          ids.forEach((id) => {
            const p = map[id];
            // A MISSING key is the batch spelling of the single route's 404:
            // this clip is gone. It is never read as "done" (§8.4).
            if (!p) return finish(id, "gone", null);
            const v = rows.get(id);
            if (v) rowUpdate(v, p);
            if (p.state === "ready" || p.state === "failed") finish(id, p.state, p);
          });
        }
        // any other status (503, 429) is transient: keep polling
      } catch {
        // network hiccup: keep polling
      }
      failures = reached ? 0 : failures + 1;
      if (failures >= maxPollFailures) {
        Array.from(rows.keys()).forEach((id) => finish(id, "unreachable", null));
        ticking = false;
        return;
      }
      if (!rows.size) { ticking = false; return; }
      setTimeout(tick, Date.now() - start >= 60000 ? 2000 : 1000);
    };

    const watch = (row) => {
      const id = row.dataset.clipId;
      if (!id || !row.dataset.progressUrl || rows.has(id)) return;
      rows.set(id, rowView(row));
      if (ticking) return;
      ticking = true;
      failures = 0;
      start = Date.now();
      tick();
    };
    $$(".clip-row[data-progress-url]", list).forEach(watch);
    return watch;
  }

  // libraryWatch is how the upload form hands a freshly built row to the
  // page's single poller. It stays a no-op on every page with no library.
  let libraryWatch = () => {};

  // The upload form's Link row: its state cell says "your default" only while
  // the picked option IS the account default (web/STYLE.md: the state cell must
  // never read false). No client: the cell keeps the server's word.
  function setupLinkState(root) {
    const state = $("[data-link-state]", root);
    if (!state) return;
    const radios = $$("input[name=link_style]", root);
    const show = () => {
      const picked = (radios.find((r) => r.checked) || {}).value;
      state.textContent = picked === state.dataset.linkDefault ? "your default" : "";
    };
    radios.forEach((r) => r.addEventListener("change", show));
    show();
  }

  function setupLibrary(library) {
    setupLibrarySearch(library);
    setupLinkState(document);
    const list = $("[data-clip-list]", library);
    if (!list) return;
    libraryWatch = watchLibrary(list);
    setupBulk(library, list);
  }

  // "Mine only" submits the search form by itself (§20 erratum 223 — the owner's
  // first look: a separate search click after ticking Mine only was not
  // intuitive). It is progressive enhancement and nothing else: the form is a
  // plain GET whose Search button works with no client at all, and this only
  // saves the second click for a browser that has one.
  //
  // requestSubmit is preferred over submit so the browser runs the form's own
  // validation and fires the submit event, exactly as the button does; submit()
  // is the fallback for a browser without it.
  function setupLibrarySearch(library) {
    const form = $("[data-library-search]", library);
    if (!form) return;
    // With a client the box submits on Enter and the switch submits itself,
    // so the Search button is one button too many (web/STYLE.md).
    hide($("[data-search-go]", form));
    const mine = $("[data-search-mine]", form);
    if (!mine) return;
    mine.addEventListener("change", () => {
      if (typeof form.requestSubmit === "function") form.requestSubmit();
      else form.submit();
    });
  }

  // ---- bulk actions (§20 errata 117-119) ---------------------------------
  //
  // Multi-select in the library, then ONE request per action. The controls are
  // server-rendered but invisible until this runs: without a client none of
  // them can do anything, and a checkbox that cannot act is worse than no
  // checkbox — every one of these actions is still available one clip at a
  // time on that clip's own page.
  function setupBulk(library, list) {
    const bar = $("[data-bulk]", library);
    if (!bar) return;
    const count = $("[data-bulk-count]", bar);
    const status = $("[data-bulk-status]", bar);
    const err = $("[data-bulk-error]", bar);
    const hint = $("[data-bulk-hint]", bar);
    const buttons = $$("[data-bulk-action]", bar);

    const picked = () => $$("[data-pick]", list).filter((c) => c.checked);
    // The bar appears with the FIRST selection and goes away with the last
    // (§20 erratum 222, amending erratum 117 — the owner's first look: the bulk bar
    // should appear once a clip is selected). The checkboxes are still
    // revealed together with the client, so nothing on screen can be ticked
    // without there being something to do with it; what changed is that a bar of
    // buttons about "the selected clips" is not shown while none is selected.
    //
    // The one exception is a bar that still has something to SAY. A bulk delete
    // removes the very rows whose checkboxes were the selection, so the moment
    // it succeeds nothing is selected — and hiding the bar there would take
    // "deleted 3 clips" off the screen with it. So the bar stays while its
    // status or error line stands, and the next change of selection clears both
    // (a summary of the last set is not true of this one).
    const hasMessage = () => (status && status.textContent !== "") || (err && !err.hidden);
    const refresh = () => {
      const boxes = picked();
      const n = boxes.length;
      setText(count, String(n));
      bar.hidden = n === 0 && !hasMessage();
      $$("[data-pick]", list).forEach((c) => {
        const row = c.closest(".clip-row");
        if (row) row.classList.toggle("picked", c.checked);
      });
    };
    // The bar's own `select all` / `clear` are selection changes too, and they
    // do the change listener's whole job (§20 erratum 226(a)): setting
    // `c.checked` from script fires NO change event, so without these two lines
    // `clear` would leave the last action's summary standing — which both keeps
    // the bar on screen over "0 selected" and leaves a sentence about a set of
    // clips above a different set after `select all`.
    const setAll = (on) => {
      $$("[data-pick]", list).forEach((c) => { c.checked = on; });
      buttons.forEach(disarm);
      setText(status, "");
      hide(err);
      refresh();
    };

    list.addEventListener("change", (ev) => {
      if (!ev.target.matches("[data-pick]")) return;
      // Changing the selection disarms a primed Delete: the second click must
      // mean the set the first one was about.
      buttons.forEach(disarm);
      // …and it drops the last action's summary, which was about a different
      // set of clips (§20 erratum 222).
      setText(status, "");
      hide(err);
      refresh();
    });
    const all = $("[data-bulk-all]", bar);
    if (all) all.addEventListener("click", () => setAll(true));
    const none = $("[data-bulk-none]", bar);
    if (none) none.addEventListener("click", () => setAll(false));

    buttons.forEach((btn) => {
      btn.addEventListener("click", async () => {
        const action = btn.dataset.bulkAction;
        hide(err);
        setText(status, "");
        const boxes = picked();
        if (!boxes.length) {
          show(err, "Tick at least one clip first.");
          return;
        }
        const body = { action, ids: boxes.map((c) => c.value) };
        if (action === "tag") {
          const field = $("[data-bulk-tag]", bar);
          const tag = field ? field.value.trim() : "";
          if (!tag) {
            show(err, "Type a tag first.");
            return;
          }
          body.tag = tag;
        }
        if (action === "ttl") {
          const field = $("[data-bulk-ttl]", bar);
          // The select rests on "no change" (value ""), and Number("") is 0 —
          // which is NEVER on the wire. The no-op is skipped, never sent.
          if (!field || field.value === "") {
            show(err, "Choose when to delete first.");
            return;
          }
          body.ttl_days = Number(field.value);
        }
        if (action === "collection") {
          const field = $("[data-bulk-collection]", bar);
          if (!field || !field.value) {
            show(err, "Choose a collection first.");
            return;
          }
          body.collection_id = field.value;
        }
        // Delete is the one that cannot be undone, so it is the one that asks
        // twice — the same two-click confirm the clip page and the token page
        // use, never a confirm() dialog.
        if (action === "delete" && !confirmClick(btn, hint)) return;
        buttons.forEach((b) => { b.disabled = true; });
        let r;
        try { r = await api("POST", "/api/clips/bulk", body); } catch { r = null; }
        buttons.forEach((b) => { b.disabled = false; });
        if (!r || !r.ok || !r.data || !Array.isArray(r.data.results)) {
          if (!authFailed(err, r)) show(err, "Bulk action failed: " + messageOf(r));
          return;
        }
        applyBulk(list, body, r.data);
        if (action === "delete" && r.data.ok > 0) moment("deleted");
        setText(status, bulkSummary(action, r.data));
        refresh();
      });
    });
    // The page has a client: reveal the CHECKBOXES. The bar itself stays hidden
    // until the first tick — refresh() owns that from here on — so a library
    // nobody has selected anything in shows no controls about a selection (§20
    // erratum 222). The checkboxes remain the thing that cannot appear without a
    // client, which is all erratum 117 was ever protecting.
    library.dataset.bulkReady = "";
    refresh();
  }

  // applyBulk re-renders what the answer proves about each row. Only the
  // outcomes this client can KNOW are painted: a deleted row goes, a tagged
  // row gains its chip, a re-dated row gets its new lifetime line. A
  // collection membership is not visible on a library row at all, so nothing
  // there is invented — the summary line says what happened.
  function applyBulk(list, req, data) {
    data.results.forEach((res) => {
      if (res.error) return;
      const row = $(`.clip-row[data-clip-id="${cssEscape(res.id)}"]`, list);
      if (!row) return;
      if (req.action === "delete") {
        row.remove();
        return;
      }
      if (req.action === "tag") addRowTag(row, req.tag);
      if (req.action === "ttl") setRowExpiry(row, req.ttl_days);
    });
    if (!$(".clip-row", list)) show($("[data-empty]"));
  }

  // cssEscape quotes an id for an attribute selector. Clip ids are base62 by
  // construction (§7), so this can only ever be belt and braces — but the
  // value comes back over the wire, and building a selector out of a wire
  // value without escaping it is the habit that eventually is not safe.
  function cssEscape(v) {
    const s = String(v);
    if (window.CSS && typeof window.CSS.escape === "function") return window.CSS.escape(s);
    return s.replace(/[^A-Za-z0-9_-]/g, "\\$&");
  }

  function addRowTag(row, tag) {
    const info = $(".info", row);
    if (!info || !tag) return;
    let tags = $("[data-tags]", row);
    if (!tags) {
      tags = document.createElement("div");
      tags.className = "tags";
      tags.dataset.tags = "";
      const sub = $(".sub", row);
      info.insertBefore(tags, sub ? sub.nextSibling : null);
    }
    // The server normalises, and this is the same normalisation db.CleanTag
    // performs on the way in — so a chip written here matches the one a
    // reload renders.
    const clean = tag.trim().toLowerCase();
    if ($$(".tag", tags).some((a) => a.textContent === clean)) return;
    const a = document.createElement("a");
    a.className = "tag";
    a.href = "/?tag=" + encodeURIComponent(clean);
    a.textContent = clean;
    tags.appendChild(a);
  }

  // setRowExpiry re-renders the lifetime line after a bulk TTL change. The
  // server resolved `expires_at` from its own clock at the moment the request
  // landed; this is the same arithmetic a second or so later, which is below
  // the minute this line is rendered to.
  function setRowExpiry(row, days) {
    const el = $("[data-expiry]", row);
    if (!el) return;
    if (!days) {
      setText(el, "kept forever");
      return;
    }
    setText(el, "expires " + fmtDate(new Date(Date.now() + days * 86400000).toISOString()));
  }

  function bulkSummary(action, data) {
    const verb = { delete: "Deleted", ttl: "Updated", tag: "Tagged", collection: "Added" }[action] || "Done";
    let msg = `${verb} ${data.ok} of ${data.results.length}.`;
    if (data.failed) {
      const counts = {};
      data.results.forEach((res) => {
        if (res.error) counts[res.error] = (counts[res.error] || 0) + 1;
      });
      msg += " " + Object.keys(counts).sort().map((c) => `${counts[c]} ${bulkReason(c)}`).join(", ") + ".";
    }
    return msg;
  }

  function bulkReason(code) {
    switch (code) {
      case "forbidden": return "not yours";
      case "not_found": return "no longer there";
      case "invalid_tags": return "already carry ten tags";
      case "too_many_clips": return "did not fit in the collection";
      default: return `failed (${code})`;
    }
  }

  // ---- share page / embed ------------------------------------------------

  // touchIfExpiring is the keep-alive half of "Copy link" (§8.1
  // POST …/touch): the owner handing the link out again is the clearest
  // signal the clip is still wanted, so copying restarts its TTL. A clip
  // kept forever has no countdown, so nothing is sent. A failure is silent
  // by design — the copy already happened, the clip keeps the expiry it had,
  // and the owner can reset it by copying again.
  // A Keep forever that lands while a touch is still in flight wins: it made
  // the clip permanent and removed data-expires, so the reply now in hand
  // describes a lifetime the server no longer has. Re-check the flag before
  // applying it, or the straggler paints an expiry back onto a clip that is
  // already kept forever.
  async function touchIfExpiring(owner) {
    if (!owner || !owner.hasAttribute("data-expires") || !owner.dataset.touchUrl) return;
    let r;
    try { r = await api("POST", owner.dataset.touchUrl); } catch { r = null; }
    if (r && r.ok && r.data && owner.hasAttribute("data-expires")) applyLifetime(owner, r.data);
  }

  // applyLifetime re-renders everything a returned ClipDTO's lifetime
  // governs, without a reload: the expiry line, and the two controls whose
  // meaning depends on whether a countdown exists at all.
  //
  // The three elements are looked up on the PAGE, not inside `owner`: the
  // share row sits above the owner block now that every viewer gets one (§20
  // erratum 98), so "Keep forever" and the copy note are the owner's markup
  // living outside the owner section. `data-expires` itself stays ON that
  // section, which is what touchIfExpiring reads and what the tests pin.
  // renderLifetime rewrites the Lifetime line from a ClipDTO in exactly the
  // shape the template renders it (§20 erratum 133): the English outside the
  // <time>, the instant inside it, and no <time> at all for a clip kept
  // forever — which is also what stops a countdown appearing on one. It is
  // built with createElement and textContent — never a markup string — so
  // escaping and the CSP are both the DOM's problem and not this file's.
  function renderLifetime(dd, dto) {
    if (!dd) return;
    dd.textContent = "";
    if (!dto.expires_at) {
      dd.textContent = "kept forever";
      return;
    }
    const d = new Date(dto.expires_at);
    if (Number.isNaN(d.getTime())) {
      dd.textContent = expiryText(dto);
      return;
    }
    dd.appendChild(document.createTextNode("expires "));
    const t = document.createElement("time");
    t.setAttribute("datetime", dto.expires_at);
    t.dataset.local = "";
    t.dataset.countdown = "";
    t.textContent = fmtDate(dto.expires_at);
    dd.appendChild(t);
    decorateTimes(dd);
  }

  function applyLifetime(owner, dto) {
    if (!owner) return;
    const page = owner.closest("[data-clip-page]") || document;
    renderLifetime($("[data-expiry]", page), dto);
    const expiring = !!dto.expires_at;
    if (expiring) {
      owner.setAttribute("data-expires", "");
    } else {
      owner.removeAttribute("data-expires");
    }
    const keep = $("[data-keep-forever]", page);
    if (keep) keep.hidden = !expiring;
    const note = $("[data-copy-note]", page);
    if (note) note.hidden = !expiring;
    paintLifetime(page);
  }

  // paintLifetime draws the Lifetime row's rail and its state cell from the two
  // instants the facts list already carries (web/STYLE.md): amber, draining
  // from the upload towards the expiry, while a link is ageing; green and full
  // once it is kept forever. The rail ships hidden and is revealed here, so a
  // browser with no client sees no bar that would otherwise stay empty.
  function paintLifetime(page) {
    const rail = $("[data-life]", page);
    const state = $("[data-life-state]", page);
    if (!rail) return;
    const fill = $("i", rail);
    const upEl = $("dt + dd time[data-local]:not([data-countdown])", page);
    const expEl = $("time[data-countdown]", page);
    const up = upEl ? new Date(upEl.getAttribute("datetime")).getTime() : NaN;
    const exp = expEl ? new Date(expEl.getAttribute("datetime")).getTime() : NaN;
    rail.hidden = false;
    if (!Number.isFinite(exp)) {
      rail.classList.remove("left");
      rail.classList.add("full");
      if (state) { state.textContent = "kept forever"; state.classList.remove("time"); state.classList.add("ok"); }
      return;
    }
    rail.classList.remove("full");
    rail.classList.add("left");
    const now = Date.now();
    const span = Number.isFinite(up) && exp > up ? exp - up : 0;
    const frac = span > 0 ? Math.max(0, Math.min(1, (exp - now) / span)) : 0;
    if (fill) fill.style.width = (frac * 100).toFixed(1) + "%";
    if (state) {
      state.textContent = leftText(exp - now);
      state.classList.remove("ok");
      state.classList.add("time");
    }
  }

  // leftText is the state cell's phrase: days above a day, then hours, then
  // minutes — the resolution people act on, and the same floors countdownText
  // uses so the two never disagree by a unit.
  function leftText(ms) {
    if (!Number.isFinite(ms) || ms <= 0) return "expiring now";
    if (ms >= dayMs) {
      const days = Math.floor(ms / dayMs);
      return `${days} day${days === 1 ? "" : "s"} left`;
    }
    const mins = Math.floor(ms / 60000);
    if (mins < 1) return "under a minute left";
    if (mins < 60) return `${mins} minute${mins === 1 ? "" : "s"} left`;
    const hours = Math.floor(mins / 60);
    return `${hours} hour${hours === 1 ? "" : "s"} left`;
  }

  // ---- server-side trim (§8.1 POST/GET …/trim, §17 #5) --------------------
  //
  // Owner-only, and a job rather than an edit: POST queues it, GET reports
  // it, and the clip stays playable on its old rendition until the swap
  // lands. Four rules are load-bearing.
  //   1. The button is disabled while a trim is live — including on a page
  //      loaded WHILE one is running, which is what the initial GET is for.
  //      Two trims of one clip cannot overlap (the server answers 409), and
  //      a button that looks clickable but always fails is worse than one
  //      that says why.
  //   2. Every window is on the ORIGINAL's timeline (§20 erratum 68), so
  //      after a trim the inputs are re-filled with the cut the clip now
  //      HAS — not with 0..what-is-left — and stay bounded by the original.
  //      Footage a cut removed is still reachable, and the box says so.
  //   3. On success the <video> is reloaded with a cache-busting query. The
  //      URL is deliberately stable (§9) and the media is served
  //      `public, max-age=3600`, so without the query the owner's own
  //      browser would keep playing the pre-trim bytes for up to an hour —
  //      exactly the delay everyone ELSE (and any shared cache in between)
  //      is meant to see, shown to the one person who just asked for it.
  //   4. Every poll has a terminal. `done`, `failed`, `cancelled`, a 404, a
  //      401/403 and a run of failed requests all STOP the loop: a spinner
  //      that never ends beside a button that never re-enables is the worst
  //      of the three ways this can go wrong.
  //   5. Nothing here reloads the page: a reload would drop the cache-busted
  //      source and put the stale one back.

  const trimPollMs = 2000;
  // minTrimMS is the API's own floor (httpapi.minTrimDurationMS): the
  // scrubber refuses to compose a window shorter than the server accepts, so
  // a drag never produces a 400 the owner did not ask for.
  const minTrimMS = 500;
  // How many consecutive unusable answers (a network error, a 5xx) to ride
  // out before giving up: ~1 minute at the poll interval, which covers a
  // restart and stops well short of "for ever".
  const trimMaxHiccups = 30;

  function secText(ms) {
    return (Math.floor(Math.max(0, Number(ms) || 0) / 100) / 10).toFixed(1);
  }

  // timecode renders a moment the way the handle tags show it: "00:02.4".
  function timecode(ms) {
    const total = Math.max(0, Number(ms) || 0);
    const m = Math.floor(total / 60000);
    const sec = (total - m * 60000) / 1000;
    return String(m).padStart(2, "0") + ":" + (sec < 10 ? "0" : "") + sec.toFixed(1);
  }

  // clock renders milliseconds the way the facts line does ("1:07").
  function clock(ms) {
    const total = Math.round(Math.max(0, Number(ms) || 0) / 1000);
    const h = Math.floor(total / 3600), m = Math.floor((total % 3600) / 60), s = total % 60;
    const mm = h > 0 ? String(m).padStart(2, "0") : String(m);
    return (h > 0 ? h + ":" : "") + mm + ":" + String(s).padStart(2, "0");
  }

  // Unlike its siblings this takes the PAGE and no owner block: the trim
  // control now sits with the player rather than inside [data-owner-controls]
  // (§20 erratum 221), and the template renders it for the owner alone, so the
  // page is both the right scope and the only one.
  function setupTrim(root) {
    const box = $("[data-trim]", root);
    if (!box) return;
    const url = box.dataset.trimUrl;
    const apiUrl = box.dataset.apiUrl;
    const startEl = $("[data-trim-start]", box);
    const endEl = $("[data-trim-end]", box);
    const btn = $("[data-trim-go]", box);
    const status = $("[data-trim-status]", box);
    const note = $("[data-trim-note]", box);
    if (!url || !startEl || !endEl || !btn) return;
    let live = false;

    const setBusy = (on) => { live = on; btn.disabled = on; };

    // watch polls until the job is terminal. It is started by a fresh POST
    // and by the page-load check below, so a reload during a trim rejoins it.
    async function watch() {
      setBusy(true);
      let hiccups = 0;
      for (;;) {
        await new Promise((r) => setTimeout(r, trimPollMs));
        let r;
        try { r = await api("GET", url); } catch { r = null; }
        if (r && (r.status === 401 || r.status === 403)) {
          // The session went away under the page. Polling on is pointless
          // and the answer will never change without a login.
          setBusy(false);
          show(status, "Your session ended; reload the page to see the result.");
          return;
        }
        if (r && r.status === 404) {
          // The clip (or its trim) is gone. Not an error to shout about.
          setBusy(false);
          hide(status);
          return;
        }
        if (!r || !r.ok || !r.data) {
          // A network hiccup or a 5xx: ride out a few, then stop rather than
          // leave the owner watching a status line that cannot change.
          if (++hiccups >= trimMaxHiccups) {
            setBusy(false);
            show(status, "Lost contact with the server; reload the page to see the result.");
            return;
          }
          continue;
        }
        hiccups = 0;
        const p = r.data;
        if (p.state === "done") {
          setBusy(false);
          applyTrimmed(root, p);
          return;
        }
        if (p.state === "failed") {
          setBusy(false);
          show(status, "Trim failed: " + (p.error || "unknown error"));
          return;
        }
        if (p.state === "cancelled") {
          setBusy(false);
          show(status, "The trim was cancelled.");
          return;
        }
        show(status, `Trimming… ${Math.max(0, Math.min(100, Number(p.percent) || 0))}%`);
      }
    }

    // applyTrimmed swaps the player onto the new rendition and re-renders the
    // control from the CLIP, which is the only place that knows both the
    // original's length and the window that was applied. The trim status is
    // the fallback: it carries the applied window too, and a clip fetch that
    // fails must not leave the page lying about what happened.
    async function applyTrimmed(page, p) {
      let clip = null;
      if (apiUrl) {
        try {
          const r = await api("GET", apiUrl);
          if (r && r.ok && r.data) clip = r.data;
        } catch { clip = null; }
      }
      const startMS = Number(p.applied_start_ms ?? p.start_ms) || 0;
      const endMS = Number(p.applied_end_ms ?? p.end_ms) || 0;
      const win = clip && clip.trim ? clip.trim : { start_ms: startMS, end_ms: endMS };
      const source = clip && clip.source_duration_ms != null
        ? Number(clip.source_duration_ms)
        : Number(box.dataset.sourceDurationMs) || 0;
      // The filmstrip goes FIRST, before anything repaints: the sheet described
      // the rendition this cut replaced, and the server has already dropped it
      // (§20 erratum 220). Taking it down here rather than afterwards is what
      // stops a stale tile from flashing under a handle at its new position.
      resetFilm();
      renderWindow(win, source);
      renderFacts(page, clip ? clip.duration_ms : endMS - startMS);
      // The card-image control turns the player's clock into the ORIGINAL's
      // timeline by adding where the cut begins, and the cut has just moved.
      // Without this the next "Use this frame" on a page that trimmed in
      // place names a moment measured from the OLD cut — a refusal at best
      // and the wrong frame at worst.
      const pbox = $("[data-poster]", page);
      if (pbox) pbox.dataset.cutStartMs = String(Number(win.start_ms) || 0);
      bustPlayer(page);
      resetGIF(page);
      show(status, "Trimmed.");
      // The card image's NOTE, last, because it is the only thing here that
      // costs a request and nothing on screen should wait for it (§20 erratum
      // 228). The applied window goes with it: it is what settles the cases the
      // status route cannot.
      if (pbox) await refreshChosen(pbox, page, win);
    }

    // refreshChosen re-reads what card image the clip now carries, and is only
    // called when the page is claiming a chosen one.
    //
    // A cut can retire that choice, and the page cannot work out whether it did:
    // the obvious test — is the chosen moment still inside the window — misses
    // the case where the moment IS inside it and the single-frame extraction
    // failed anyway, which makes the server fall back to the automatic frame and
    // drop the choice (internal/worker/trim.go, §20 erratum 130). The clip DTO
    // deliberately carries no poster_seconds (§20 erratum 128), so the primary
    // answer is the owner-only status route the control already holds the address
    // of, and the window test stays as the FALLBACK for the answers that route
    // cannot give (both halves are erratum 228(a), amended).
    //
    // What each answer means:
    //   404 — no poster job at all, so nothing was ever chosen: automatic.
    //   done + applied_seconds null — the choice is gone (outside the cut, or an
    //     extraction that failed inside it): automatic.
    //   done + a number — that is the moment the clip carries; re-render the note
    //     at it, because a cut in place moves nothing but may confirm it.
    //   queued / running / failed, or an unreadable answer — the status route has
    //     not decided, so the WINDOW decides what it can: a chosen moment now
    //     outside the applied cut is certainly gone (the server re-takes the
    //     frame from the cut or drops the choice), and that case is cleared.
    //     Inside the cut nothing is said, because a wrong note is worse than a
    //     stale one; the next page load renders the truth from the server.
    //
    // That last branch is the union of this route's answer and erratum 130's
    // window test, and it is strictly better than either alone. The route knows
    // about extractions that failed INSIDE the window, which the window cannot
    // see; the window settles the outside case with certainty and no request,
    // which the route cannot when the latest poster job is `failed` — it then
    // carries no `applied_seconds` at all, however true or false the note is, and
    // a failed poster job never touched the clip, so a trim that dropped the
    // choice afterwards left the page asserting a card image the clip no longer
    // has, with nothing to heal it but a reload.
    async function refreshChosen(pbox, page, win) {
      if (!pbox.dataset.posterSeconds) return;      // nothing claimed: nothing to correct
      const noteEl = $("[data-poster-note]", page);
      const automatic = () => {
        delete pbox.dataset.posterSeconds;
        setText(noteEl, "Card image: automatic.");
      };
      // Both numbers are on the ORIGINAL's timeline, which is the whole reason
      // the choice is stored that way. A window that does not read as one — no
      // end after its start, which is what a clip fetch and a trim status that
      // both said nothing usable would leave — proves nothing and clears nothing.
      const outsideCut = () => {
        const ms = Number(pbox.dataset.posterSeconds) * 1000;
        const lo = Number(win && win.start_ms) || 0;
        const hi = Number(win && win.end_ms) || 0;
        return hi > lo && Number.isFinite(ms) && (ms < lo || ms >= hi);
      };
      const statusURL = pbox.dataset.posterStatusUrl;
      let r = null;
      if (statusURL) {
        try { r = await api("GET", statusURL); } catch { r = null; }
      }
      if (r && r.status === 404) { automatic(); return; }
      if (!r || !r.ok || !r.data || r.data.state !== "done") {
        if (outsideCut()) automatic();
        return;
      }
      const applied = r.data.applied_seconds != null ? Number(r.data.applied_seconds) : NaN;
      if (!Number.isFinite(applied)) { automatic(); return; }
      pbox.dataset.posterSeconds = applied.toFixed(3);
      setText(noteEl, "Card image: the frame at " + clock(applied * 1000) + " of the original.");
    }

    // renderWindow re-fills and re-bounds the inputs from the ORIGINAL.
    function renderWindow(win, sourceMS) {
      const start = Number(win.start_ms) || 0;
      const end = Number(win.end_ms) || 0;
      box.dataset.trimStartMs = String(start);
      box.dataset.trimEndMs = String(end);
      startEl.value = secText(start);
      endEl.value = secText(end);
      if (sourceMS > 0) {
        box.dataset.sourceDurationMs = String(sourceMS);
        const bound = secText(sourceMS);
        startEl.max = bound; endEl.max = bound;
        setText(note, `Seconds on the original upload (0:00–${clock(sourceMS)}); the cut is ` +
          (start === 0 && end === sourceMS ? "the whole of it." : `${clock(start)}–${clock(end)}.`));
      }
      // The scale, the window and what the player can show have all just
      // changed; the handles follow the boxes they write into.
      paintScrubber();
    }

    // renderFacts updates the duration in the shared facts list. The <dd>
    // carries data-duration-ms for every viewer — it is the same number the
    // line already prints, so it says nothing an anonymous visitor could not
    // read — and the duration is always its last segment.
    function renderFacts(page, durationMS) {
      const dd = $("dd[data-duration-ms]", page);
      if (!dd || !(Number(durationMS) > 0)) return;
      dd.dataset.durationMs = String(Math.round(Number(durationMS)));
      const parts = dd.textContent.split(" · ");
      parts[parts.length - 1] = clock(durationMS);
      dd.textContent = parts.join(" · ");
    }

    function bustPlayer(page) {
      const video = $("video, audio", page);
      if (!video) return;
      const src = video.getAttribute("src");
      const poster = video.getAttribute("poster");
      if (poster) video.setAttribute("poster", bustURL(poster));
      if (src) {
        video.setAttribute("src", bustURL(src));
        video.load();
      }
    }

    btn.addEventListener("click", async () => {
      if (live) return;
      hide(status);
      const start = Math.round(Number(startEl.value) * 1000);
      const end = Math.round(Number(endEl.value) * 1000);
      if (!Number.isFinite(start) || !Number.isFinite(end) || end <= start) {
        show(status, "The end must be after the start.");
        return;
      }
      setBusy(true);
      show(status, "Queued…");
      let r;
      try { r = await api("POST", url, { start_ms: start, end_ms: end }); } catch { r = null; }
      if (!r || !r.ok) {
        setBusy(false);
        // The POST is not the watch: nothing has started, so a dead session
        // here is the ordinary "log in again", not the watcher's "reload to
        // see the result of a trim that is still running".
        if (!authFailed(status, r)) show(status, messageOf(r, "could not start the trim"));
        return;
      }
      watch();
    });

    // ---- the visual scrubber (ROADMAP #6, §20 erratum 132) --------------
    //
    // A track whose full width is the ORIGINAL's timeline — the same
    // coordinate system the number boxes and the API already speak (§20
    // erratum 68) — with a draggable handle at each end of the window. The
    // number boxes STAY: they are the API's truth, the accessible fallback
    // and the only half of this control that works with scripting off. The
    // handles do nothing but write into them.
    //
    // Four rules are load-bearing.
    //   1. Nothing appears without a client. The markup is server-rendered
    //      `hidden` and revealed here, and a clip whose ORIGINAL was never
    //      measured is never revealed at all: a track with no scale cannot be
    //      a scrubber, and the boxes already carry no maximum for it.
    //   2. Every pixel is written through element.style.left / .width, never
    //      a style attribute string — CSP style-src is 'self'.
    //   3. Dragging SEEKS the player while the moment is inside the current
    //      cut, and greys the picture when it is not. The rendition holds
    //      only the cut, so a handle out past its edge is naming footage this
    //      player cannot show; saying so is better than leaving the wrong
    //      frame on screen as if it were the right one.
    //   4. The handles are real buttons with role="slider", so the control is
    //      reachable and operable from the keyboard: arrows move a tenth of a
    //      second, PageUp/PageDown a second, Home/End the ends.
    const scrub = $("[data-scrubber]", box);
    const track = scrub ? $("[data-scrub-track]", scrub) : null;
    const chosen = scrub ? $("[data-scrub-range]", scrub) : null;
    // The trimmed-away ends are dimmed into the trough and the kept span is
    // outlined (web/STYLE.md); both are positioned here from the same
    // percentages as the handles, never by a style attribute.
    const cuts = {
      start: scrub ? $('[data-scrub-cut="start"]', scrub) : null,
      end: scrub ? $('[data-scrub-cut="end"]', scrub) : null,
    };
    const keptEl = $("[data-trim-kept]", box);
    const handles = {
      start: scrub ? $('[data-scrub-handle="start"]', scrub) : null,
      end: scrub ? $('[data-scrub-handle="end"]', scrub) : null,
    };
    const scrubSteps = { ArrowLeft: -100, ArrowRight: 100, ArrowDown: -100, ArrowUp: 100, PageDown: -1000, PageUp: 1000 };
    let dragging = null;

    // How close to an end is "too close for a centred tag": half a tag over the
    // track's own width, MEASURED rather than the pair of percentages this was
    // once tuned to at 390 px. Read once, on the first paint that has text in
    // the tags, and again only when the track itself resizes — never inside a
    // pointermove. The band keeps a freak measurement sane.
    let edgePct = 8, edgeMeasured = false;
    function measureEdge() {
      if (!track || !handles.start) return;
      const w = track.getBoundingClientRect().width;
      const tag = $(".tc", handles.start);
      if (!w || !tag) return;
      const half = tag.getBoundingClientRect().width / 2;
      if (!half) return;
      edgePct = Math.min(Math.max((half / w) * 100, 4), 20);
      edgeMeasured = true;
    }
    if (track && typeof ResizeObserver === "function") {
      new ResizeObserver(() => { edgeMeasured = false; paintScrubber(); }).observe(track);
    } else if (track) {
      window.addEventListener("resize", () => { edgeMeasured = false; paintScrubber(); });
    }

    // sourceMS is read from the DOM every time: a trim rewrites it, and a
    // scrubber holding the pre-trim scale would put every handle in the
    // wrong place.
    const sourceMS = () => Math.max(0, Math.round(Number(box.dataset.sourceDurationMs) || 0));
    const valueMS = (el) => Math.round((Number(el.value) || 0) * 1000);

    // insideCut asks the CLIP, not the inputs: what the player can show is
    // the window the current rendition was cut from.
    function insideCut(ms) {
      const cs = Number(box.dataset.trimStartMs) || 0;
      const ce = Number(box.dataset.trimEndMs) || 0;
      return ce > cs ? ms >= cs && ms <= ce : true;
    }

    function seekTo(ms) {
      const video = $("video", root);
      if (!video) return;
      if (!insideCut(ms)) {
        video.classList.add("out-of-cut");
        return;
      }
      video.classList.remove("out-of-cut");
      const t = (ms - (Number(box.dataset.trimStartMs) || 0)) / 1000;
      if (!Number.isFinite(t) || t < 0) return;
      try { video.currentTime = t; } catch { /* a player that has not loaded yet */ }
    }

    function paintScrubber() {
      const total = sourceMS();
      if (!scrub || scrub.hidden || !total) return;
      const pct = (ms) => (Math.min(Math.max(ms, 0), total) / total) * 100;
      const s = Math.min(Math.max(valueMS(startEl), 0), total);
      const e = Math.min(Math.max(valueMS(endEl), 0), total);
      const lo = Math.min(s, e), hi = Math.max(s, e);
      handles.start.style.left = pct(s) + "%";
      handles.end.style.left = pct(e) + "%";
      chosen.style.left = pct(lo) + "%";
      chosen.style.width = (pct(hi) - pct(lo)) + "%";
      if (cuts.start) { cuts.start.style.left = "0%"; cuts.start.style.width = pct(lo) + "%"; }
      if (cuts.end) { cuts.end.style.left = pct(hi) + "%"; cuts.end.style.width = (100 - pct(hi)) + "%"; }
      if (keptEl) {
        keptEl.textContent = `keeps ${((hi - lo) / 1000).toFixed(1)} s of ${(total / 1000).toFixed(1)}`;
        keptEl.hidden = false;
      }
      [["start", s], ["end", e]].forEach(([which, ms]) => {
        const h = handles[which];
        h.setAttribute("aria-valuemax", String(total));
        h.setAttribute("aria-valuenow", String(ms));
        h.setAttribute("aria-valuetext", clock(ms));
        setText($("[data-scrub-time]", h), timecode(ms));
        // The tag rides above its handle, centred, except near either end of the
        // track, where centred would hang off the page (a 390 px phone).
        if (!edgeMeasured) measureEdge();
        h.classList.toggle("at-start", pct(ms) < edgePct);
        h.classList.toggle("at-end", pct(ms) > 100 - edgePct);
        h.classList.toggle("outside", !insideCut(ms));
        // …and the frame that moment actually shows, when there is a filmstrip
        // to take it from (§20 erratum 220).
        paintTile(which, ms);
      });
    }

    // setHandle writes one end of the window, keeping the pair at least the
    // API's own 500 ms apart so a drag can never compose a request the server
    // is bound to refuse.
    function setHandle(which, ms) {
      const total = sourceMS();
      if (!total) return;
      ms = Math.min(Math.max(Math.round(ms), 0), total);
      if (which === "start") {
        const other = valueMS(endEl);
        if (ms > other - minTrimMS) ms = Math.max(0, other - minTrimMS);
        startEl.value = secText(ms);
      } else {
        const other = valueMS(startEl);
        if (ms < other + minTrimMS) ms = Math.min(total, other + minTrimMS);
        endEl.value = secText(ms);
      }
      paintScrubber();
      seekTo(valueMS(which === "start" ? startEl : endEl));
    }

    function msAtClientX(x) {
      const r = track.getBoundingClientRect();
      if (!(r.width > 0)) return 0;
      const frac = Math.min(Math.max((x - r.left) / r.width, 0), 1);
      return Math.round(frac * sourceMS());
    }

    function bindHandle(which) {
      const h = handles[which];
      h.addEventListener("pointerdown", (ev) => {
        ev.preventDefault();
        h.focus();
        dragging = which;
        // The class is what keeps the handle's filmstrip tile on screen for the
        // whole drag on a device with no hover (§20 erratum 220); app.css also
        // reveals it on hover and on keyboard focus.
        h.classList.add("dragging");
        try { h.setPointerCapture(ev.pointerId); } catch { /* a browser without capture still drags */ }
        setHandle(which, msAtClientX(ev.clientX));
      });
      h.addEventListener("pointermove", (ev) => {
        if (dragging !== which) return;
        setHandle(which, msAtClientX(ev.clientX));
      });
      const stop = (ev) => {
        if (dragging !== which) return;
        dragging = null;
        h.classList.remove("dragging");
        try { h.releasePointerCapture(ev.pointerId); } catch { /* already released */ }
      };
      h.addEventListener("pointerup", stop);
      h.addEventListener("pointercancel", stop);
      h.addEventListener("keydown", (ev) => {
        if (ev.ctrlKey || ev.altKey || ev.metaKey) return;
        if (ev.key === "Home") { ev.preventDefault(); setHandle(which, 0); return; }
        if (ev.key === "End") { ev.preventDefault(); setHandle(which, sourceMS()); return; }
        const step = scrubSteps[ev.key];
        if (step === undefined) return;
        ev.preventDefault();
        setHandle(which, valueMS(which === "start" ? startEl : endEl) + step);
      });
    }

    // ---- the filmstrip (§20 erratum 220) ---------------------------------
    //
    // One JPEG of ~24 thumbnails laid out in a row, spanning the CURRENT
    // rendition. It does two jobs, and the second is the one the owner asked for:
    // the track gets a picture of the clip so the whole timeline is readable at
    // a glance, and each handle gets the single tile of the moment it names, at
    // the sheet's own scale, so dragging shows the frame instead of a number.
    //
    // Four rules, and each is a decision:
    //   1. Nothing is requested that is not needed. When the page shipped the
    //      sheet's geometry the strip paints before the first frame with no
    //      request of its own; only a clip with NO sheet asks the route, which
    //      answers 202 and makes one. A page with no client asks for nothing.
    //   2. The sheet spans the CUT, not the track. The track's width is the
    //      ORIGINAL's timeline (the coordinate system the API speaks) while the
    //      tiles are frames of the rendition, so the strip is inset to the
    //      applied window and a handle outside it gets no tile at all — the
    //      same fact the greyed player and the dashed handle already state.
    //   3. It is an <img>, not a CSS background. `img-src 'self'` covers it,
    //      nothing has to build a url() out of an attribute value, and both
    //      handle tiles reuse the one object out of the browser's cache.
    //   4. A missing sheet is never an error on screen. The scrubber worked
    //      before this existed and works identically without it.
    const strip = scrub ? $("[data-scrub-strip]", scrub) : null;
    const stripImg = strip ? $("[data-scrub-strip-img]", strip) : null;
    const tileBox = {
      start: handles.start ? $("[data-scrub-tile]", handles.start) : null,
      end: handles.end ? $("[data-scrub-tile]", handles.end) : null,
    };
    // The poll pace is the route's own Retry-After (5 s, filmstripRetryAfter in
    // internal/httpapi/filmstrip.go); the cap is three minutes, which OUTLIVES
    // the sheet job's own 180 s ffmpeg kill, so a sheet that can still arrive is
    // still being waited for. A job queued behind another encode can outlast
    // even that: the track then keeps the plain look it has always had, and the
    // next load of the page renders the sheet.
    const filmPollMs = 5000;
    const filmMaxPolls = 36;
    let film = null;
    // One poll loop at a time: two trims in two minutes would otherwise leave
    // two of them asking for the same sheet.
    let filmAsking = false;
    // The cache-buster for the sheet, EMPTY until a trim lands. The address is
    // stable for the life of the clip while the bytes behind it are per
    // rendition, and the served answer is `public, max-age=3600` — so after a
    // cut the browser would happily repaint the retired rendition's frames out
    // of its own cache for up to an hour, which is precisely what resetFilm
    // exists to prevent. bustPlayer does the same thing to the video and the
    // poster two doors down, and for the same reason.
    //
    // It stays empty on the FIRST paint deliberately: an ordinary page load of
    // a clip that already has a sheet must come straight out of the cache,
    // which is the whole point of the public directive.
    let filmBust = "";

    // readFilm is the sheet's geometry as the DOM holds it — server-rendered at
    // first, rewritten after a poll. All four numbers or nothing: a partial
    // layout would place tiles wrongly, which is worse than placing none.
    //
    // The url it returns carries the current bust, so the strip and both handle
    // tiles ask for ONE address and share one cache entry.
    function readFilm() {
      const d = box.dataset;
      const n = Number(d.filmstripTiles) || 0;
      const w = Number(d.filmstripTileW) || 0;
      const h = Number(d.filmstripTileH) || 0;
      const iv = Number(d.filmstripIntervalMs) || 0;
      if (!d.filmstripUrl || n <= 0 || w <= 0 || h <= 0 || iv <= 0) return null;
      return { url: d.filmstripUrl + filmBust, tiles: n, tileW: w, tileH: h, intervalMS: iv };
    }

    // paintFilm insets the strip to the applied cut and reveals it. The sheet
    // is stretched to the strip's box (app.css `object-fit: fill`), which
    // squashes each tile horizontally on a narrow track and is the right trade:
    // the strip's job is to be TIME-ACCURATE at a glance, and the handle tiles
    // below show the same frames undistorted.
    function paintFilm() {
      if (!strip || !stripImg || !film) return;
      const total = sourceMS();
      if (!total) return;
      const cs = Number(box.dataset.trimStartMs) || 0;
      const ce = Number(box.dataset.trimEndMs) || 0;
      const lo = ce > cs ? cs : 0;
      const hi = ce > cs ? ce : total;
      strip.style.left = (Math.max(0, Math.min(lo, total)) / total) * 100 + "%";
      strip.style.width = (Math.max(0, Math.min(hi - lo, total)) / total) * 100 + "%";
      if (stripImg.getAttribute("src") !== film.url) stripImg.src = film.url;
      strip.hidden = false;
      scrub.classList.add("has-filmstrip");
    }

    // paintTile puts the tile of one handle's moment above it. The index is on
    // the RENDITION's timeline, so the cut's start comes off the handle's value
    // first — erratum 68's coordinate system, subtracted exactly as the chosen
    // card frame's is.
    function paintTile(which, ms) {
      const boxEl = tileBox[which];
      if (!boxEl) return;
      const img = $("[data-scrub-tile-img]", boxEl);
      if (!film || !img || !insideCut(ms)) {
        boxEl.hidden = true;
        return;
      }
      const cs = Number(box.dataset.trimStartMs) || 0;
      const idx = Math.max(0, Math.min(Math.floor((ms - cs) / film.intervalMS), film.tiles - 1));
      boxEl.style.width = film.tileW + "px";
      boxEl.style.height = film.tileH + "px";
      img.style.width = film.tiles * film.tileW + "px";
      img.style.height = film.tileH + "px";
      img.style.left = -(idx * film.tileW) + "px";
      if (img.getAttribute("src") !== film.url) img.src = film.url;
      boxEl.hidden = false;
    }

    // applyFilm re-reads the geometry and repaints everything that shows it.
    function applyFilm() {
      film = readFilm();
      if (!film) return false;
      paintFilm();
      paintScrubber();
      return true;
    }

    // askFilm requests a sheet for a clip that has none and applies the first
    // one that lands. The first call is what ENQUEUES the job (the route
    // answers 202 and makes one), and HEAD is used for the poll for the preview
    // GIF's reason: the answer wanted is the STATUS, and the sheet itself is
    // fetched by the <img> that is about to want it anyway.
    //
    // Only the OWNER's session may make a sheet (§20 erratum 227), which is why
    // the fetch below carries `credentials: "same-origin"` — that is the whole
    // credential this route needs, and this control only exists on the owner's
    // own page.
    //
    // 404 (not this caller's clip to make a sheet for — in practice a session
    // that ended, or a clip that went away), 409 (`not_ready`) and 503
    // (`filmstrip_failed`) are terminal and silent: the scrubber has never needed
    // a filmstrip to work, and an owner reading a message about a thumbnail sheet
    // they never asked for would be noise.
    async function askFilm() {
      const url = box.dataset.filmstripUrl;
      if (!url || film || filmAsking) return;
      filmAsking = true;
      try { await pollFilm(url); } finally { filmAsking = false; }
    }

    async function pollFilm(url) {
      for (let i = 0; i < filmMaxPolls; i++) {
        let res = null;
        try { res = await fetch(url, { method: "HEAD", credentials: "same-origin", cache: "no-store" }); } catch { res = null; }
        if (res && res.status === 200) {
          // The sheet exists — 200 EXACTLY, never `res.ok`, which is true of the
          // 202 this route answers while the sheet is still being made and would
          // spend a clip-DTO fetch on every tick of the poll (§20 erratum
          // 226(b)). A 202 falls through to the sleep below, which is the whole
          // of what it means.
          //
          // The geometry lives on the clip, so one GET of the DTO settles the
          // layout. A body that does not carry it is treated as "not yet"
          // rather than as an error: the next tick asks again.
          let r = null;
          try { r = await api("GET", box.dataset.apiUrl); } catch { r = null; }
          const fs = r && r.ok && r.data ? r.data.filmstrip : null;
          if (fs) {
            box.dataset.filmstripTiles = String(fs.tiles);
            box.dataset.filmstripTileW = String(fs.tile_width);
            box.dataset.filmstripTileH = String(fs.tile_height);
            box.dataset.filmstripIntervalMs = String(fs.interval_ms);
            if (applyFilm()) return;
          }
        }
        if (res && (res.status === 404 || res.status === 409 || res.status === 503)) return;
        await new Promise((r) => setTimeout(r, filmPollMs));
      }
    }

    // resetFilm is what a landed trim does to the sheet: the rendition it
    // described is gone, the server has already NULLed the columns and deleted
    // the object, so the strip comes down and a fresh one is asked for.
    //
    // A fresh bust goes with it: the NEXT sheet lives at the same address as the
    // one just retired, and taking the <img> down does not empty the HTTP cache
    // behind it (§20 erratum 220's client paragraph, amended).
    function resetFilm() {
      film = null;
      filmBust = bustQuery();
      delete box.dataset.filmstripTiles;
      delete box.dataset.filmstripTileW;
      delete box.dataset.filmstripTileH;
      delete box.dataset.filmstripIntervalMs;
      if (strip) strip.hidden = true;
      if (stripImg) stripImg.removeAttribute("src");
      Object.keys(tileBox).forEach((which) => {
        const el = tileBox[which];
        if (!el) return;
        el.hidden = true;
        const img = $("[data-scrub-tile-img]", el);
        if (img) img.removeAttribute("src");
      });
      if (scrub) scrub.classList.remove("has-filmstrip");
      askFilm();
    }

    function initScrubber() {
      if (!scrub || !track || !chosen || !handles.start || !handles.end || !sourceMS()) return;
      scrub.hidden = false;
      handles.start.setAttribute("aria-valuemin", "0");
      handles.end.setAttribute("aria-valuemin", "0");
      bindHandle("start");
      bindHandle("end");
      // Typing in a box moves the handle: one control, two ways into it.
      [startEl, endEl].forEach((el) => el.addEventListener("input", paintScrubber));
      // "From = now" / "To = now": the player's clock runs on the RENDITION,
      // so the cut's start is added to land on the original's timeline.
      $$("[data-scrub-set]", box).forEach((b) => {
        b.hidden = false;
        b.addEventListener("click", () => {
          const video = $("video", root);
          if (!video || !Number.isFinite(Number(video.currentTime))) return;
          setHandle(b.dataset.scrubSet, (Number(box.dataset.trimStartMs) || 0) + Math.round(Number(video.currentTime) * 1000));
        });
      });
      paintScrubber();
      // The filmstrip, last: a sheet the page already knows about paints now,
      // and only a clip without one costs a request.
      if (!applyFilm()) askFilm();
    }
    initScrubber();

    // A trim already running when this page loaded: rejoin it rather than
    // offering a button whose only possible answer is 409. The SERVER says
    // whether there is one -- `data-trim-live` on the control -- so a clip
    // that has never been trimmed costs no request at all. Asking
    // unconditionally meant a GET …/trim on every single owner view, and
    // "this clip has no trim" is a 404: one 404 in the access log per view of
    // a feature the owner had not used (§20 erratum 87).
    if (box.hasAttribute("data-trim-live")) {
      show(status, "Trimming…");
      watch();
    }
  }

  // ---- GIF export (§8.1 GET …/preview.gif, POST …/gif, §17 #6) -----------
  //
  // Owner-only, and on demand: the preview does not exist until somebody
  // asks, so the button's first click is a GET that either brings the GIF
  // back (200) or starts one (202) and polls. Three rules:
  //   1. A 202 is not an error and not a failure — it is the normal first
  //      answer — so the status line says "making it", not "wait".
  //   2. The <img> is cache-busted ONLY when the bytes may have just changed
  //      -- i.e. after a poll watched one being made. The URL is stable and
  //      the media is cached for an hour, so after a regenerate the owner's
  //      own browser would otherwise show the preview they just replaced; but
  //      busting it on a page load, or on a click that found the preview
  //      already there, only guarantees a fresh download of a file the cache
  //      already holds.
  //   3. A 503 (`gif_failed`) is terminal for the GET: the server will not
  //      retry a doomed encode for every visitor. The next click POSTs
  //      instead, which is the explicit "try again" the route documents.

  // The poll interval is the server's own Retry-After on the 202 (5 s,
  // gifRetryAfter in internal/httpapi/gif.go): a client that ignores the pace
  // it was asked for and picks a faster one of its own is just noise.
  const gifPollMs = 5000;
  const gifMaxPolls = 24; // ~2 minutes, then say so rather than poll forever

  function setupGIF(root) {
    const box = $("[data-gif]", root);
    if (!box) return;
    const url = box.dataset.gifUrl;
    const postURL = box.dataset.gifPostUrl;
    const btn = $("[data-gif-go]", box);
    const status = $("[data-gif-status]", box);
    const dl = $("[data-gif-download]", box);
    const img = $("[data-gif-img]", root);
    if (!url || !btn) return;
    let retry = false; // the last answer was a failure: the next click regenerates
    let live = false;

    const bust = () => url + (url.includes("?") ? "&" : "?") + "t=" + Date.now();
    // fresh=true only where the preview was just (re)made; see rule 2.
    function reveal(fresh) {
      const src = fresh ? bust() : url;
      if (img) { img.src = src; img.hidden = false; }
      // The route is …/preview.gif for every clip, so a bare `download` saved
      // every one of them as "preview.gif" and a second one silently became
      // "preview (1).gif". Name the file after the clip it came from.
      if (dl) { dl.href = src; dl.download = "clippet-" + (root.dataset.clipId || "preview") + ".gif"; dl.hidden = false; }
      hide(status);
    }
    function failed(msg) {
      retry = true;
      show(status, msg);
    }

    // The poll asks with HEAD. A ready preview answers the GET with the whole
    // GIF, and downloading it here only to hand the <img> a cache-busted URL
    // downloaded the same picture twice. HEAD carries the identical status
    // and Retry-After and no body at all, so the ONE download is the <img>'s.
    // The statuses that need a MESSAGE are re-asked with GET: they are
    // terminal and rare, and their bodies are JSON envelopes, never a GIF.
    async function watch() {
      for (let i = 0; i < gifMaxPolls; i++) {
        await new Promise((r) => setTimeout(r, gifPollMs));
        let r;
        try { r = await api("HEAD", url); } catch { r = null; }
        if (!r) continue;                       // network hiccup: keep watching
        if (r.status === 200) { reveal(true); return; }
        if (r.status === 202) { show(status, "Making the GIF…"); continue; }
        if (r.status === 404 || r.status === 409 || r.status === 503) {
          let g;
          try { g = await api("GET", url); } catch { g = null; }
          if (!g) continue;                     // it went away mid-answer: keep watching
          if (g.status === 200) { reveal(true); return; }
          if (g.status === 503 && g.data && g.data.error === "gif_failed") {
            failed(messageOf(g, "the GIF could not be made"));
            return;
          }
          if (g.status === 404 || g.status === 409) {
            failed(messageOf(g, "the GIF is not available"));
            return;
          }
          continue;                             // a real 503: transient, keep watching
        }
        // any other status (a 429) is transient: keep watching
      }
      show(status, "Still making the GIF — try again in a moment.");
    }

    btn.addEventListener("click", async () => {
      if (live) return;
      live = true;
      btn.disabled = true;
      show(status, "Making the GIF…");
      let r;
      // The first ask is a HEAD, for the reason the poll is one: a preview
      // that already exists answers a GET with the whole GIF, and the ONE
      // download belongs to the <img>. HEAD queues the job exactly as GET
      // does (the router maps it onto the same handler), so the 202 path is
      // unchanged — and nothing here depends on an unread body warming a
      // cache. A retry regenerates, which is a POST and has no such choice.
      const probed = !retry;
      try {
        r = probed ? await api("HEAD", url) : await api("POST", postURL);
      } catch {
        r = null;
      }
      retry = false;
      if (r && r.status === 200) {
        // Already there: the <img> asks for the plain URL, which is the one
        // download of it this click makes.
        reveal(false);
      } else if (r && (r.status === 202 || r.status === 409)) {
        await watch();
      } else {
        // A HEAD answer has no JSON envelope to read a message out of, so a
        // failure is re-asked with GET — terminal, rare, and never a GIF
        // body; the poll makes the same trade. (If the preview appeared in
        // between, that GET is the download and the plain URL is cached.)
        let g = r;
        if (probed && r) {
          try { g = await api("GET", url); } catch { g = r; }
        }
        if (g && g.status === 200) {
          reveal(false);
        } else {
          failed(messageOf(g, "could not make the GIF"));
        }
      }
      btn.disabled = false;
      live = false;
    });

    // A preview that already exists is shown without asking for it: the page
    // said so in the markup, so this costs one CACHED image (the plain URL,
    // never the buster) and no request to the API at all.
    if (box.hasAttribute("data-gif-ready")) reveal();
  }

  // resetGIF puts the preview control back to "there is none" (§20 erratum
  // 95). A trim REPLACES the rendition and takes the preview with it —
  // internal/db/trim.go NULLs gif_key and supersedes the object — so what is
  // left on screen is a picture of footage that no longer exists beside a
  // Download link to a route that now answers 202. Clicking it saved the
  // 19-byte JSON envelope under a .gif name. The hint below the row says a
  // trim replaces the GIF whether or not one just happened; this says it about
  // the trim that did.
  function resetGIF(root) {
    const box = $("[data-gif]", root);
    if (!box) return;
    box.removeAttribute("data-gif-ready");
    const img = $("[data-gif-img]", root);
    if (img) { img.hidden = true; img.removeAttribute("src"); }
    const dl = $("[data-gif-download]", box);
    if (dl) { dl.hidden = true; dl.removeAttribute("href"); }
    show($("[data-gif-status]", box), "removed by the trim — make it again");
  }

  // ---- the owner's chosen card image (ROADMAP #5, §20 errata 127-130) ----
  //
  // "Use this frame" is exactly what it says: the player's current time,
  // translated onto the ORIGINAL's timeline (the API's coordinate system, so
  // the cut's start is added), POSTed, and then polled until the job is
  // terminal. The trim's rules apply here verbatim and for its reasons:
  //   1. The button is disabled while a job is live, INCLUDING one that was
  //      already running when the page loaded — the server says so in the
  //      markup (`data-poster-live`), so a clip nobody has chosen a frame for
  //      costs no request at all.
  //   2. Every poll has a terminal. done, failed, cancelled, a 404, a
  //      401/403 and a run of unusable answers all stop the loop.
  //   3. On success the <video poster> is cache-busted. The URL is stable and
  //      the image is served `public, max-age=3600`, so without the query the
  //      one person who just asked for the change would be the last to see
  //      it.
  //   4. Nothing here reloads the page: a reload would throw away the
  //      cache-busted poster and put the old one back.

  const posterPollMs = 2000;
  const posterMaxHiccups = 30;
  // posterEdgeGuardMS is the API's own edge guard (httpapi.posterEdgeGuardMS,
  // §20 erratum 128): the last tenth of a second of the rendition is not
  // choosable, because a seek at or past the last picture makes ffmpeg write
  // no file at all. The button clamps into it for the reason minTrimMS exists
  // above — a control must not compose a request the server is bound to
  // refuse — and here the refusal is ordinary rather than exotic: a player
  // left to run to its end sits EXACTLY on the guarded moment, with the frame
  // in question on screen, so answering "seconds must be a moment this clip
  // actually shows" would be the page arguing with its own picture.
  // httpapi.TestPosterEdgeGuardIsMirroredInTheClient keeps the two numbers
  // the same number.
  const posterEdgeGuardMS = 100;

  // bustPoster re-points the <video poster> at the same object with a fresh
  // query, which is the only way past an hour of `public, max-age=3600`.
  function bustPoster(page) {
    const video = $("video", page);
    if (!video) return;
    const poster = video.getAttribute("poster");
    if (poster) video.setAttribute("poster", bustURL(poster));
  }

  function setupPoster(root) {
    const box = $("[data-poster]", root);
    if (!box) return;
    const url = box.dataset.posterUrl;
    const statusURL = box.dataset.posterStatusUrl || url;
    const btn = $("[data-poster-go]", box);
    const status = $("[data-poster-status]", box);
    const note = $("[data-poster-note]", box);
    if (!url || !btn) return;
    let live = false;
    const setBusy = (on) => { live = on; btn.disabled = on; };

    // applied is the moment the CLIP now carries, which the server reads from
    // the clip rather than echoing back — so this renders what HAPPENED.
    //
    // A job can end `done` having published nothing: a trim that lands while
    // the frame is being taken replaces the very frames it came from, and the
    // commit refuses rather than putting a picture of footage this clip no
    // longer has on the card (§20 erratum 130). The clip then still carries
    // whatever it carried before, so `done` on its own does not mean "changed".
    //
    // What DOES say so is the job's STAGE, and the route already sends it: a lost
    // race resolves done + `skipped` (internal/worker/poster.go's `skipped`), a
    // real swap resolves done + `done`. Reading the not-changed case off the job
    // row is what the REJOIN path needed (§20 erratum 228). It has no asked-for
    // value to compare against — the page loaded after the request was made — so
    // it used to compare applied_seconds against the moment the page was RENDERED
    // with, which reported a job that re-took the SAME moment as "not changed"
    // even though it had published a fresh object.
    //
    // The asked-vs-applied comparison stays, and it belongs to the CLICK path
    // alone: a job that published a different moment from the one this click sent
    // is not this click's success, whatever its stage says.
    function applyChosen(asked, applied, stage) {
      if (stage === "skipped" || !Number.isFinite(applied) ||
          (Number.isFinite(asked) && Math.abs(applied - asked) > 0.0005)) {
        show(status, "Not changed — the clip was edited meanwhile. Try again.");
        return;
      }
      box.dataset.posterSeconds = applied.toFixed(3);
      setText(note, "Card image: the frame at " + clock(applied * 1000) + " of the original.");
      bustPoster(root);
      show(status, "Card image updated. Make the GIF again to match.");
    }

    async function watch(asked) {
      setBusy(true);
      let hiccups = 0;
      for (;;) {
        await new Promise((r) => setTimeout(r, posterPollMs));
        let r;
        try { r = await api("GET", statusURL); } catch { r = null; }
        if (r && (r.status === 401 || r.status === 403)) {
          setBusy(false);
          show(status, "Your session ended; reload the page to see the result.");
          return;
        }
        if (r && r.status === 404) {
          // The clip (or its poster job) is gone. Not an error to shout about.
          setBusy(false);
          hide(status);
          return;
        }
        if (!r || !r.ok || !r.data) {
          if (++hiccups >= posterMaxHiccups) {
            setBusy(false);
            show(status, "Lost contact with the server; reload the page to see the result.");
            return;
          }
          continue;
        }
        hiccups = 0;
        const p = r.data;
        if (p.state === "done") {
          setBusy(false);
          applyChosen(asked, p.applied_seconds != null ? Number(p.applied_seconds) : NaN, p.stage);
          return;
        }
        if (p.state === "failed") {
          setBusy(false);
          show(status, "Not changed: " + (p.error || "unknown error"));
          return;
        }
        if (p.state === "cancelled") {
          setBusy(false);
          show(status, "The card image job was cancelled.");
          return;
        }
        show(status, "Taking that frame…");
      }
    }

    btn.addEventListener("click", async () => {
      if (live) return;
      const video = $("video", root);
      const at = video ? Number(video.currentTime) : NaN;
      if (!Number.isFinite(at)) {
        show(status, "Move the player to the frame you want first.");
        return;
      }
      // The player's clock runs on the RENDITION; the API speaks the
      // ORIGINAL's timeline, which is what lets a later trim keep the choice.
      const cutStartMs = Number(box.dataset.cutStartMs) || 0;
      let seconds = Math.round((at + cutStartMs / 1000) * 1000) / 1000;
      // That moment is then clamped into the range the API will accept. The
      // rendition's length is the number the facts list already carries for
      // every viewer, and a trim rewrites it there (`renderFacts`), so it is
      // read fresh from the DOM rather than frozen at page load. The bound is
      // built with the same operations in the same order as
      // httpapi.posterChoiceRange, so the two are the SAME double and the
      // moment sent from the end of a clip is accepted rather than refused by
      // a rounding hair.
      const dd = $("dd[data-duration-ms]", root);
      const durationMs = dd ? Math.round(Number(dd.dataset.durationMs) || 0) : 0;
      if (durationMs > posterEdgeGuardMS) {
        const last = cutStartMs / 1000 + (durationMs - posterEdgeGuardMS) / 1000;
        if (seconds > last) seconds = last;
      }
      hide(status);
      setBusy(true);
      show(status, "Queued…");
      let r;
      try { r = await api("POST", url, { seconds }); } catch { r = null; }
      if (!r || !r.ok) {
        setBusy(false);
        if (!authFailed(status, r)) show(status, messageOf(r, "could not use that frame"));
        return;
      }
      watch(r.data && typeof r.data.seconds === "number" ? r.data.seconds : seconds);
    });

    // A job already running when this page loaded: rejoin it rather than
    // offering a button whose only possible answer is 409. The SERVER says
    // whether there is one (`data-poster-live`), so a clip nobody has chosen
    // a frame for costs no request at all. There is no asked-for value to judge
    // the result against — the page was rendered before it was known — and none
    // is needed: the job's own stage says whether it swapped anything (§20
    // erratum 228).
    if (box.hasAttribute("data-poster-live")) {
      show(status, "Taking that frame…");
      watch(NaN);
    }
  }

  // ---- watch-page player: shortcuts, and loop/mute memory (ROADMAP #29) ---
  //
  // A native `<video controls>` reacts to keys only while the player itself
  // has focus, so a share page ignored Space and the arrows until somebody
  // clicked the picture. The map below lives on the DOCUMENT instead, which
  // is exactly why it must refuse to fire inside a control: the owner block
  // under the player is full of text fields, and the scrubber's own handles
  // are buttons that want the arrow keys for themselves.
  //
  // Two preferences are remembered — whether the viewer mutes, and whether
  // they loop — in localStorage and NOWHERE else. Nothing is sent anywhere: a
  // taste is not telemetry, and this file has no route to tell. Every read
  // and write is wrapped, because a private window, blocked site data or a
  // full quota must not stop the player working.
  //
  // SHARE PAGE ONLY. The embed's autoplay/muted/loop are the EMBEDDER's
  // choice, expressed in the URL (§20 erratum 77); a viewer preference that
  // overrode them would silently change a contract other people's pages
  // depend on.

  const prefMuted = "clippet.muted";
  const prefLoop = "clippet.loop";
  // The remembered QUALITY is a tier NAME ("original" | "full" | "light") and
  // never a URL: a URL belongs to one clip, a tier is a preference (§20 erratum
  // 211). localStorage only, like the two above — which bitrate somebody
  // prefers is not a fact about the clip and never reaches the server.
  const prefQuality = "clippet.quality";

  function readPref(key) {
    try { return localStorage.getItem(key); } catch { return null; }
  }
  function writePref(key, on) {
    try { localStorage.setItem(key, on ? "1" : "0"); } catch { /* private window, blocked storage, full quota */ }
  }
  function writePrefValue(key, value) {
    try { localStorage.setItem(key, value); } catch { /* private window, blocked storage, full quota */ }
  }

  // typingInto keeps every shortcut out of anything that already means
  // something to the key: fields, selects, contenteditable, and buttons —
  // where Space is "press me" and the arrows belong to the scrubber.
  function typingInto(el) {
    if (!el || el === document || el === document.body) return false;
    if (el.isContentEditable) return true;
    const tag = el.tagName;
    return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || tag === "BUTTON" || tag === "OPTION";
  }

  function seekBy(video, delta) {
    const t = Number(video.currentTime) + delta;
    if (!Number.isFinite(t)) return;
    const d = Number(video.duration);
    video.currentTime = Math.max(0, Number.isFinite(d) && d > 0 ? Math.min(t, d) : t);
  }

  function toggleFullscreen(video) {
    try {
      if (document.fullscreenElement) {
        const p = document.exitFullscreen();
        if (p && p.catch) p.catch(() => {});
        return;
      }
      if (video.requestFullscreen) {
        const p = video.requestFullscreen();
        if (p && p.catch) p.catch(() => {});
        return;
      }
      if (video.webkitEnterFullscreen) video.webkitEnterFullscreen(); // iOS Safari
    } catch { /* a browser that refuses is not a failure of the page */ }
  }

  // An audio clip's own progress (§20 erratum 329). The poster above the
  // <audio> IS the waveform, in the unplayed colour; this lays the same bars
  // over it in amber — drawn from the row's peaks (`data-peaks`, two hex digits
  // a bar) at the geometry the server drew them with (`data-wave`) — and clips
  // that layer to what has been heard: the card's audiogram, on the page, from
  // the same numbers, with no library and nothing fetched. Then the Media
  // Session (title and the poster on the lock screen), which is local
  // JavaScript and calls nobody.
  function setupWaveform(root) {
    const audio = $("audio", root);
    const wrap = $("[data-wave]", root);
    if (!audio) return;
    if ("mediaSession" in navigator && typeof window.MediaMetadata === "function") {
      try {
        const art = audio.dataset.artwork;
        navigator.mediaSession.metadata = new window.MediaMetadata({
          title: audio.dataset.mediaTitle || document.title,
          artwork: art ? [{ src: new URL(art, location.href).href, sizes: "1280x720", type: "image/png" }] : [],
        });
      } catch (_) { /* a browser that refuses the metadata still plays */ }
    }
    if (!wrap) return;
    const hex = wrap.dataset.peaks || "";
    const g = (wrap.dataset.wave || "").split(",").map(Number);
    if (!/^([0-9a-f]{2})+$/.test(hex) || g.length !== 9 || g.some((n) => !Number.isFinite(n))) return;
    const [fw, fh, bx, by, bw, bh, barW, gap, minH] = g;
    const ns = "http:" + "//www.w3.org/2000/svg"; // split: the URL tripwire reads every line
    const svg = document.createElementNS(ns, "svg");
    svg.setAttribute("viewBox", "0 0 " + fw + " " + fh);
    svg.setAttribute("preserveAspectRatio", "none");
    svg.setAttribute("aria-hidden", "true");
    svg.classList.add("wave-played");
    const n = hex.length / 2;
    const offset = (bw - (n * (barW + gap) - gap)) / 2;
    for (let i = 0; i < n; i++) {
      const v = parseInt(hex.substr(2 * i, 2), 16);
      const h = Math.max(minH, (v / 255) * bh);
      const r = document.createElementNS(ns, "rect");
      r.setAttribute("x", String(bx + offset + i * (barW + gap)));
      r.setAttribute("y", String(by + (bh - h) / 2));
      r.setAttribute("width", String(barW));
      r.setAttribute("height", String(h));
      r.setAttribute("rx", String(barW / 2));
      svg.appendChild(r);
    }
    wrap.appendChild(svg);
    // The played edge runs across the band exactly as the audiogram's does:
    // from the band's left edge at 0 to its right edge at the end.
    const paint = () => {
      const d = Number(audio.duration);
      const t = Number(audio.currentTime);
      const f = Number.isFinite(d) && d > 0 ? Math.min(1, Math.max(0, t / d)) : 0;
      const edge = t > 0 ? (bx + bw * f) / fw : 0;
      svg.style.setProperty("clip-path", "inset(0 " + (100 - edge * 100).toFixed(3) + "% 0 0)");
    };
    ["timeupdate", "seeked", "loadedmetadata", "ended", "emptied"].forEach((e) => audio.addEventListener(e, paint));
    paint();
  }

  function setupPlayer(root) {
    // An audio clip's page holds an <audio> where a video's holds a <video> (§20
    // erratum 328); the keys, the loop and the remembered mute are the same.
    const video = $("video, audio", root);
    if (!video) return;
    const extras = $("[data-player-extras]", root);
    const loopBtn = extras ? $("[data-loop]", extras) : null;

    // The remembered preferences, applied before the first frame is fetched.
    if (readPref(prefMuted) === "1") video.muted = true;
    video.loop = readPref(prefLoop) === "1";
    if (loopBtn) loopBtn.setAttribute("aria-pressed", video.loop ? "true" : "false");
    // The row is server-rendered `hidden`: a Loop button that cannot be wired
    // is a button that does nothing, so it appears only where there is a
    // client to make it work.
    if (extras) extras.hidden = false;

    video.addEventListener("volumechange", () => writePref(prefMuted, video.muted));
    if (loopBtn) {
      loopBtn.addEventListener("click", () => {
        video.loop = !video.loop;
        loopBtn.setAttribute("aria-pressed", video.loop ? "true" : "false");
        writePref(prefLoop, video.loop);
      });
    }

    document.addEventListener("keydown", (ev) => {
      if (ev.ctrlKey || ev.altKey || ev.metaKey || ev.defaultPrevented) return;
      if (typingInto(ev.target)) return;
      const k = ev.key;
      switch (k) {
        case " ":
        case "k":
        case "K":
          if (video.paused) {
            const p = video.play();
            if (p && p.catch) p.catch(() => {});
          } else {
            video.pause();
          }
          break;
        case "j": case "J": seekBy(video, -10); break;
        case "l": case "L": seekBy(video, 10); break;
        case "ArrowLeft": seekBy(video, -5); break;
        case "ArrowRight": seekBy(video, 5); break;
        case "ArrowUp": video.volume = Math.min(1, video.volume + 0.1); break;
        case "ArrowDown": video.volume = Math.max(0, video.volume - 0.1); break;
        case "m": case "M": video.muted = !video.muted; break;
        case "f": case "F": toggleFullscreen(video); break;
        default: {
          const d = Number(video.duration);
          if (k >= "0" && k <= "9" && Number.isFinite(d) && d > 0) {
            video.currentTime = d * (Number(k) / 10);
            break;
          }
          return; // not ours: leave the key to the page and the browser
        }
      }
      ev.preventDefault();
    });
  }

  // ---- the quality selector (§20 errata 205, 211) --------------------------
  //
  // The tiers are SERVER-RENDERED as <option>s, so a browser with no JavaScript
  // shows which one it is playing and a crawler sees one <video> with one src.
  // This turns the change event into a src swap that keeps the position, and
  // remembers the viewer's pick between clips.
  //
  // While a better tier is still encoding the page keeps a slow poll on
  // GET …/progress — the ONE case where a READY clip polls at all — and rebuilds
  // the <select> when the tier lands, so nobody has to reload to get the good
  // copy. The labels it rebuilds are formatted the same way the server formats
  // them (internal/web/tiers.go); they are two implementations of one sentence,
  // and the E2E pins the server's.

  const tierWords = { original: "Original", full: "Full", light: "Light" };
  const byteUnits = ["KiB", "MiB", "GiB", "TiB", "PiB", "EiB"];

  // humanBytes mirrors internal/web/format.go: one decimal, binary units.
  function humanBytes(n) {
    if (!Number.isFinite(n) || n < 1024) return String(Math.max(0, Math.round(n || 0))) + " bytes";
    let div = 1024, exp = 0;
    for (let m = Math.floor(n / 1024); m >= 1024 && exp < byteUnits.length - 1; m = Math.floor(m / 1024)) {
      div *= 1024;
      exp++;
    }
    return (n / div).toFixed(1) + " " + byteUnits[exp];
  }

  // fpsText mirrors internal/web/tiers.go: "60fps" for an exact rate, "29.97fps"
  // for a fractional one, so a 30000/1001 clip is never labelled "30fps".
  function fpsText(fps) {
    if (typeof fps !== "string") return "";
    const parts = fps.split("/");
    const num = Number(parts[0]), den = Number(parts[1] === undefined ? 1 : parts[1]);
    if (!Number.isFinite(num) || !Number.isFinite(den) || num <= 0 || den <= 0) return "";
    return num % den === 0 ? String(num / den) + "fps" : (num / den).toFixed(2) + "fps";
  }

  function tierLabel(t) {
    // An audio tier carries its own words (§20 erratum 328): `Full · FLAC 24-bit`.
    if (t.label) return t.label + (t.bytes > 0 ? " · " + humanBytes(t.bytes) : "");
    let label = tierWords[t.tier] || t.tier;
    if (t.width > 0 && t.height > 0) label += " " + t.width + "×" + t.height;
    const fps = fpsText(t.fps);
    if (fps) label += " " + fps;
    if (t.bytes > 0) label += " · " + humanBytes(t.bytes);
    return label;
  }

  // switchQuality swaps the source and puts the viewer back where they were. A
  // <video> reloads from zero on a src change, so the position and the play
  // state are captured first and restored once the new file has metadata.
  function switchQuality(video, url) {
    if (!url || video.getAttribute("src") === url) return;
    const at = Number(video.currentTime) || 0;
    const wasPlaying = !video.paused && !video.ended;
    const resume = () => {
      video.removeEventListener("loadedmetadata", resume);
      try { if (at > 0) video.currentTime = at; } catch { /* a source that cannot seek there */ }
      if (wasPlaying) {
        const p = video.play();
        if (p && p.catch) p.catch(() => {});
      }
    };
    video.addEventListener("loadedmetadata", resume);
    video.setAttribute("src", url);
    video.load();
  }

  function setupQuality(root) {
    const video = $("video, audio", root);
    if (!video) return;
    const box = $("[data-quality]", root);
    const select = box ? $("[data-quality-select]", box) : null;
    if (select) {
      // The remembered tier, applied only if this clip HAS it: a viewer who
      // prefers Original still gets Light on a clip whose upload cannot be
      // played, rather than an empty player.
      const want = readPref(prefQuality);
      if (want) {
        for (const opt of select.options) {
          if (opt.dataset.tier === want) {
            select.value = opt.value;
            switchQuality(video, opt.value);
            break;
          }
        }
      }
      select.addEventListener("change", () => {
        const opt = select.options[select.selectedIndex];
        if (!opt) return;
        writePrefValue(prefQuality, opt.dataset.tier || "");
        switchQuality(video, opt.value);
      });
    }
    watchTiers(root, video, box);
  }

  // The tier watch: slow, bounded, and only where something is actually coming.
  // Five seconds for the first minute, then fifteen: a Full takes tens of seconds
  // to minutes, the first minute is when a viewer is still looking at the page,
  // and past it the poll costs a fifth of the requests. That matters because this
  // is the ONE poll a READY clip makes, so it is made by every VIEWER of a share
  // link and not by the owner's own tab (§20 erratum 211, amended). The ceiling is
  // twenty minutes of wall clock, past which a reload is the honest answer.
  const tierPollFastMs = 5000;
  const tierPollSlowMs = 15000;
  const tierPollFastForMs = 60000;
  const tierPollMaxMs = 20 * 60 * 1000;

  function watchTiers(root, video, box) {
    if (root.dataset.state !== "ready") return;
    const url = root.dataset.progressUrl;
    const api = root.dataset.apiUrl;
    if (!url || !api) return;
    const started = Date.now();
    // The next poll, or none at all once the ceiling is past. A hidden tab does
    // not spend a poll: it waits for the tab to come back and picks up there.
    const again = () => {
      const elapsed = Date.now() - started;
      if (elapsed >= tierPollMaxMs) return;
      setTimeout(tick, elapsed < tierPollFastForMs ? tierPollFastMs : tierPollSlowMs);
    };
    const tick = async () => {
      if (document.visibilityState === "hidden") {
        document.addEventListener("visibilitychange", tick, { once: true });
        return;
      }
      try {
        const res = await fetch(url, { credentials: "same-origin", cache: "no-store" });
        if (res.status === 404) return; // the clip is gone; nothing to refresh
        if (res.ok) {
          const p = await res.json();
          const encoding = Array.isArray(p.tiers) && p.tiers.some((t) => t.state === "encoding");
          if (!encoding) {
            await refreshTiers(root, video, box, api);
            return;
          }
        }
      } catch { /* a network hiccup is not the end of the poll */ }
      again();
    };
    again();
  }

  // refreshTiers rebuilds the selector from the clip's own DTO — the authority on
  // what exists and what it measures — WITHOUT touching the player: whatever the
  // viewer is watching goes on playing, and the better tier is now an option they
  // can take. A viewer whose remembered preference has just become available is
  // the one exception, because that is what the preference means.
  async function refreshTiers(root, video, box, api) {
    let clip;
    try {
      const res = await fetch(api, { credentials: "same-origin", cache: "no-store" });
      if (!res.ok) return;
      clip = await res.json();
    } catch {
      return;
    }
    const tiers = (clip.tiers || []).filter((t) => t && t.state === "ready" && t.url);
    if (!box || tiers.length === 0) return;
    let select = $("[data-quality-select]", box);
    if (!select) {
      // The page was rendered with a single tier and therefore no control at all.
      const label = document.createElement("label");
      label.appendChild(document.createTextNode("Quality "));
      select = document.createElement("select");
      select.setAttribute("name", "quality");
      select.setAttribute("data-quality-select", "");
      label.appendChild(select);
      box.insertBefore(label, box.firstChild);
      select.addEventListener("change", () => {
        const opt = select.options[select.selectedIndex];
        if (!opt) return;
        writePrefValue(prefQuality, opt.dataset.tier || "");
        switchQuality(video, opt.value);
      });
    }
    const playing = video.getAttribute("src");
    const want = readPref(prefQuality);
    select.textContent = "";
    let wanted = "";
    for (const t of tiers) {
      const opt = document.createElement("option");
      opt.value = t.url;
      opt.dataset.tier = t.tier;
      opt.textContent = tierLabel(t);
      if (t.url === playing || (want && t.tier === want)) opt.selected = true;
      if (want && t.tier === want) wanted = t.url;
      select.appendChild(opt);
    }
    const note = $("[data-tier-note]", box);
    if (note) note.remove();
    // Nothing is encoding any more, so the CARD's copy has landed (or was
    // skipped, or failed into the fallback): the line saying the Discord card
    // is still coming is now false wherever it is still on the page.
    const cardNote = $("[data-card-note]", root);
    if (cardNote) cardNote.remove();
    if (wanted) switchQuality(video, wanted);
  }

  // ---- collections (§8.1 /api/collections, §17 #9) ------------------------
  //
  // A collection is an ordered list of links and the controls are that plain:
  // rename, replace the membership, delete. Two rules are load-bearing.
  //   1. Delete is TWO CLICKS, never a confirm() dialog — confirmClick above.
  //   2. The membership PUT sends the ticked boxes in DOCUMENT order, which is
  //      the order the server stores and the page renders. The template puts
  //      the current members first, so saving an unchanged set is a no-op
  //      rather than a reshuffle. It is a CHECKBOX LIST and not a
  //      <select multiple> because an unmodified click on a multi-select
  //      REPLACES the selection: one stray click used to drop every other
  //      member, silently, with no diff and no undo (§20 erratum 95).
  //   3. Taking members OUT therefore takes a second click too. Adding does
  //      not: nothing is lost by it.

  function setupCollectionNew(form) {
    const err = $("[data-error]", form);
    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      hide(err);
      const title = form.elements.title.value.trim();
      if (!title) return;
      const button = $("button[type=submit]", form);
      if (button) button.disabled = true;
      let r;
      try { r = await api("POST", form.getAttribute("action"), { title }); } catch { r = null; }
      if (button) button.disabled = false;
      if (r && r.status === 201 && r.data && r.data.id) {
        // NOT a reload: this form shares the library page with the upload
        // form, and reloading threw away a chosen file, a typed title and
        // typed tags for the sake of re-rendering one <li> (§20 erratum 95).
        addCollectionRow(r.data);
        form.elements.title.value = ""; // only the field this submit spent
        return;
      }
      if (!authFailed(err, r)) show(err, messageOf(r, "could not create the collection"));
    });
  }

  // addCollectionRow mirrors index.html's Collections <li> for a fresh 201
  // CollectionDTO. The server lists collections newest first, so a new one
  // goes on the front.
  function addCollectionRow(dto) {
    const list = $("[data-collection-list]");
    if (!list) return;
    const li = document.createElement("li");
    li.dataset.collectionId = dto.id;
    const a = document.createElement("a");
    a.className = "colcard";
    a.href = local(dto.url);
    const stack = document.createElement("span");
    stack.className = "stack";
    stack.setAttribute("aria-hidden", "true");
    for (let i = 0; i < 3; i++) stack.appendChild(document.createElement("i"));
    a.appendChild(stack);
    a.appendChild(document.createTextNode(dto.title));
    const sub = document.createElement("span");
    sub.className = "sub";
    const n = typeof dto.clip_count === "number" ? dto.clip_count : 0;
    sub.textContent = `${n} clip${n === 1 ? "" : "s"}`;
    a.appendChild(sub);
    li.appendChild(a);
    list.prepend(li);
    hide($("[data-collections-empty]"));
  }

  // setupCollectionRows gives each member row its own controls (ROADMAP #17):
  // Remove, and the two arrows that reorder the list. They are revealed here
  // — the markup ships `hidden` — so a browser with no JavaScript sees the
  // list and the whole-membership picker below it, and no dead buttons.
  //
  // The two actions use two different routes ON PURPOSE. Remove is the
  // single-clip DELETE, which touches one row and cannot lose a concurrent
  // edit. A MOVE has no single-clip spelling — order is a property of the
  // whole list — so it PUTs the membership the page is showing, and the page
  // says so by re-rendering from the answer rather than assuming it won.
  function setupCollectionRows(root, clipsURL) {
    const list = $(".collection-clips", root);
    if (!list) return;
    const status = $("[data-members-row-status]", root);
    const rows = () => $$("li.clip-row", list);
    const ids = () => rows().map((li) => li.dataset.clipId);

    $$("[data-row-actions]", list).forEach((el) => { el.hidden = false; });

    const busy = (on) => {
      $$("[data-row-actions] button", list).forEach((b) => { b.disabled = on; });
    };

    list.addEventListener("click", async (ev) => {
      const move = ev.target.closest("[data-member-move]");
      const remove = ev.target.closest("[data-member-remove]");
      if (!move && !remove) return;
      const li = ev.target.closest("li.clip-row");
      if (!li) return;
      hide(status);
      if (remove) {
        // One row, one request. The row goes only after the server says it
        // did — a list that removed it optimistically would be showing a
        // membership the next reload disagrees with.
        busy(true);
        let r;
        try { r = await api("DELETE", remove.dataset.memberUrl); } catch { r = null; }
        busy(false);
        if (r && (r.status === 204 || r.status === 404)) {
          li.remove();
          show(status, "Removed from this collection. The clip itself is untouched.");
          return;
        }
        if (!authFailed(status, r)) show(status, messageOf(r, "could not remove the clip"));
        return;
      }
      const sibling = move.dataset.memberMove === "up" ? li.previousElementSibling : li.nextElementSibling;
      if (!sibling || !sibling.classList.contains("clip-row")) return;
      if (move.dataset.memberMove === "up") list.insertBefore(li, sibling);
      else list.insertBefore(sibling, li);
      busy(true);
      let r;
      try { r = await api("PUT", clipsURL, { clip_ids: ids() }); } catch { r = null; }
      busy(false);
      if (r && r.ok) {
        show(status, "Order saved.");
        moment("saved");
        move.focus();
        return;
      }
      // Put the row back where the server still has it.
      if (move.dataset.memberMove === "up") list.insertBefore(sibling, li);
      else list.insertBefore(li, sibling);
      if (!authFailed(status, r)) show(status, messageOf(r, "could not save the order"));
    });
  }

  function setupCollectionPage(root) {
    const owner = $("[data-collection-controls]", root);
    const apiURL = root.dataset.apiUrl;
    const clipsURL = root.dataset.clipsUrl;

    const copyBtn = $("[data-copy-link]", root);
    if (copyBtn) {
      copyBtn.addEventListener("click", async () => {
        if (await copyText(root.dataset.pageUrl)) { flash($("[data-copied]", root)); copied(); }
      });
    }
    if (!owner) return;
    setupCollectionRows(root, clipsURL);

    const rename = $("[data-collection-rename]", owner);
    if (rename) {
      const err = $("[data-error]", rename);
      rename.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        hide(err);
        const title = rename.elements.title.value.trim();
        let r;
        try { r = await api("PATCH", apiURL, { title }); } catch { r = null; }
        if (r && r.ok && r.data) {
          setText($("[data-collection-title]", root), r.data.title);
          // No suffix, because the server renders this page's <title> as the
          // collection's own name and nothing else (pages.go, collectionPage).
          document.title = r.data.title;
          return;
        }
        if (!authFailed(err, r)) show(err, messageOf(r, "could not rename the collection"));
      });
    }

    const members = $("[data-collection-members]", owner);
    if (members) {
      const err = $("[data-error]", members);
      const status = $("[data-members-status]", members);
      const hint = $("[data-members-hint]", members);
      const save = $("button[type=submit]", members);
      const boxes = () => $$("input[name=clip_ids]", members);
      // What the SERVER rendered: a save is diffed against it, so "removes N"
      // counts what this save takes out of the collection and nothing else.
      const wasMember = new Set(boxes().filter((b) => b.defaultChecked).map((b) => b.value));
      // Re-ticking a box after the button armed itself makes the count it is
      // showing untrue, so any change puts the button back.
      members.addEventListener("change", () => { if (save) disarm(save); });
      members.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        hide(err);
        hide(status);
        const ids = boxes().filter((b) => b.checked).map((b) => b.value); // document order
        const kept = new Set(ids);
        const removed = [...wasMember].filter((id) => !kept.has(id)).length;
        // The PUT replaces the whole membership, so a save that drops members
        // is destructive and has no undo. It says how many and waits for a
        // second click; a save that only ADDS goes straight through.
        if (removed > 0 && save && !confirmClick(save, hint, `Save clips (removes ${removed})`)) return;
        if (save) save.disabled = true;
        let r;
        try { r = await api("PUT", clipsURL, { clip_ids: ids }); } catch { r = null; }
        if (save) save.disabled = false;
        if (r && r.ok) {
          carry("saved");
          location.reload(); // the list above is server-rendered; re-render it
          return;
        }
        if (!authFailed(err, r)) show(err, messageOf(r, "could not save the clips"));
      });
    }

    const del = $("[data-collection-delete]", owner);
    if (del) {
      const hint = $("[data-delete-hint]", owner);
      del.addEventListener("click", async () => {
        if (!confirmClick(del, hint)) return;
        del.disabled = true;
        let r;
        try { r = await api("DELETE", apiURL); } catch { r = null; }
        if (r && (r.status === 204 || r.status === 404)) {
          carry("deleted");
          location.href = "/";
          return;
        }
        del.disabled = false;
        const status = $("[data-members-status]", owner);
        if (!authFailed(status, r)) show(status, messageOf(r, "could not delete the collection"));
      });
    }
  }

  // dirtyOwnerFields answers the only question the ready-transition reload has
  // to ask: is there work on this page that a reload would throw away? Every
  // control in the owner block is compared against the value the SERVER
  // rendered it with, which is exactly what a reload would put back.
  //
  // The SELECT rule is not `selected !== defaultSelected` (§20 erratum 95(b)).
  // `defaultSelected` mirrors the `selected` ATTRIBUTE, and a single-select
  // whose markup carries none — which is every select this page renders, the
  // TTL one included, pinned by web.TestSharePageOwnerSelectHasNoDefault —
  // still has an option SELECTED, because the browser must select something.
  // So that comparison is true on an untouched page and no owner ever got the
  // automatic reload. What a reload would restore is the option the browser
  // would pick again, and defaultIndex below is the HTML reset algorithm's own
  // answer to that (§20 erratum 103 corrected it): the LAST option carrying
  // the attribute, not the first, and — when none does — the first option that
  // is not `disabled`, not index 0. Both corrections are latent today, since
  // the markup carries neither a `selected` nor a `disabled` option and the
  // test above pins that; they are here so that adding either cannot quietly
  // reintroduce the bug this rule exists to fix.
  // A `multiple` select has no such forced pick and keeps the per-option
  // comparison (none is rendered today; the branch is there for the same
  // reason).
  // defaultIndex is the option a browser selects in a single-select with no
  // user interaction — the HTML "ask for a reset" algorithm, not an
  // approximation of it. Two rules, in this order:
  //   1. If any option carries the `selected` attribute, the LAST such option
  //      wins (the parser sets each one selected in turn, and the reset step
  //      then unselects all but the last).
  //   2. Otherwise the first option that is NOT `disabled` is selected — an
  //      untouched select whose option 0 is disabled sits on index 1.
  // With every option disabled and none selected, nothing is selected at all
  // and `selectedIndex` is -1, which is what the -1 this returns compares
  // equal to.
  function defaultIndex(opts) {
    let last = -1;
    for (let i = 0; i < opts.length; i++) {
      if (opts[i].defaultSelected) last = i;
    }
    if (last >= 0) return last;
    return opts.findIndex((o) => !o.disabled);
  }

  function dirtyOwnerFields(root) {
    const owner = $("[data-owner-controls]", root);
    if (!owner) return false;
    return $$("input, textarea, select", owner).some((el) => {
      if (el.type === "checkbox" || el.type === "radio") return el.checked !== el.defaultChecked;
      if (el.tagName === "SELECT") {
        const opts = $$("option", el);
        if (el.multiple) return opts.some((o) => o.selected !== o.defaultSelected);
        if (opts.length === 0) return false;
        return el.selectedIndex !== defaultIndex(opts);
      }
      return el.value !== el.defaultValue;
    });
  }

  // offerReload replaces the state line with the news and a real link, for the
  // one case where reloading is right but doing it FOR the reader would cost
  // them what they had typed. An empty href is this page.
  function offerReload(el) {
    if (!el) return;
    el.textContent = "Processing finished — ";
    const a = document.createElement("a");
    a.href = "";
    a.textContent = "reload to watch";
    el.appendChild(a);
    el.hidden = false;
  }

  // ---- replace the file (§8.1 POST/GET …/replace, ROADMAP #31) -----------
  //
  // Owner-only, and the one control on this page that changes the clip's
  // BYTES rather than its metadata. Two requests: an XHR POST of the new file
  // (so the transfer has a percentage, like the upload form's), then a poll of
  // the same URL until the job is terminal — the trim's shape exactly, because
  // it is the same kind of job: the clip stays `ready`, the page keeps playing
  // the old file, and the new one appears when the swap commits.
  //
  // A landed replacement reloads the page rather than patching it. Everything
  // on it describes the file: the duration, the dimensions, the poster, the
  // facts line, the trim control's bounds and the GIF row. Re-deriving all of
  // that in the client would be a second renderer of the same page.
  const replacePollMs = 2000;
  const replaceMaxHiccups = 30;

  function setupReplace(root) {
    const form = $("[data-replace]", root);
    if (!form) return;
    const url = form.dataset.replaceUrl;
    const input = $("[data-replace-file]", form);
    const btn = $("[data-replace-go]", form);
    const cancelBtn = $("[data-replace-cancel]", form);
    const status = $("[data-replace-status]", form);
    if (!url || !input || !btn) return;
    let live = false;
    // The in-flight upload XHR, or null. It is kept so the Cancel button has
    // something to abort: the abort plumbing (xhr.onabort) was already here
    // and nothing ever reached the object to call it.
    let upload = null;

    const setBusy = (on) => { live = on; btn.disabled = on; };
    // The label around the file input is the control (web/STYLE.md): picking
    // a file IS the request, so the Replace button — the half a browser with
    // no client needs — goes.
    hide(btn);
    input.addEventListener("change", () => {
      if (!input.files || !input.files[0] || live) return;
      if (typeof form.requestSubmit === "function") form.requestSubmit();
      else form.dispatchEvent(new Event("submit", { cancelable: true }));
    });
    // Cancel is offered for the UPLOAD only. Once the server has the file and
    // answers 202 the job is the queue's, and stopping it would need a route
    // that marks it cancelled and lets replaceRun's cleanup drop the uploaded
    // original — a new §8.1 row, which is a design change and not a nit
    // (N091). Hiding the button at that moment is the honest half: an owner is
    // never shown a control that cannot do what it says.
    const offerCancel = (on) => { if (cancelBtn) cancelBtn.hidden = !on; };
    const finishUpload = () => { upload = null; offerCancel(false); };

    if (cancelBtn) {
      cancelBtn.addEventListener("click", () => {
        if (upload) upload.abort();
      });
    }

    // watch polls GET …/replace until the job is terminal. `done` is the swap:
    // from that moment every URL on this page serves the new file.
    async function watch() {
      let hiccups = 0;
      for (;;) {
        await new Promise((r) => setTimeout(r, replacePollMs));
        let r;
        try { r = await api("GET", url); } catch { r = null; }
        if (r && (r.status === 401 || r.status === 403)) {
          setBusy(false);
          show(status, "Your session ended; reload the page to see the result.");
          return;
        }
        if (r && r.status === 404) {
          setBusy(false);
          hide(status);
          return;
        }
        if (!r || !r.ok || !r.data) {
          if (++hiccups >= replaceMaxHiccups) {
            setBusy(false);
            show(status, "Lost contact with the server; reload the page to see the result.");
            return;
          }
          continue;
        }
        hiccups = 0;
        const p = r.data;
        if (p.state === "done") {
          setBusy(false);
          form.reset(); // the chosen file is spent: it must not read as unsaved work
          show(status, "Replaced.");
          if (!dirtyOwnerFields(root)) {
            location.reload();
            return;
          }
          offerReplaceReload(status);
          return;
        }
        if (p.state === "failed") {
          setBusy(false);
          show(status, "Replace failed: " + (p.error || "unknown error") + " — the clip is unchanged.");
          return;
        }
        if (p.state === "cancelled") {
          setBusy(false);
          show(status, "The replacement was cancelled; the clip is unchanged.");
          return;
        }
        show(status, `Encoding… ${Math.max(0, Math.min(100, Number(p.percent) || 0))}%`);
      }
    }

    form.addEventListener("submit", (ev) => {
      ev.preventDefault();
      if (live) return;
      const file = input.files && input.files[0];
      if (!file) {
        show(status, "Choose a video first.");
        return;
      }
      setBusy(true);
      show(status, "Uploading… 0%");
      const fd = new FormData();
      fd.append("file", file, file.name);
      const xhr = new XMLHttpRequest();
      upload = xhr;
      offerCancel(true);
      xhr.open("POST", url);
      xhr.responseType = "json";
      xhr.upload.onprogress = (e) => {
        if (!e.lengthComputable) return;
        const pct = Math.floor((e.loaded / e.total) * 100);
        show(status, pct < 100 ? `Uploading… ${pct}%` : "Saving…");
      };
      xhr.onerror = () => {
        finishUpload();
        setBusy(false);
        show(status, "Replace failed: network error");
      };
      xhr.onabort = () => {
        // The clip is untouched: an aborted transfer never reached the point
        // where a job exists, and the server's own disconnect handling drops
        // the spooled bytes.
        finishUpload();
        setBusy(false);
        show(status, "Replace cancelled — the clip is unchanged.");
      };
      xhr.onload = () => {
        finishUpload();
        const dto = xhr.response;
        if (xhr.status !== 202 || !dto) {
          setBusy(false);
          if (authFailed(status, { status: xhr.status, data: dto })) return;
          show(status, "Replace failed: " + (dto && dto.message ? dto.message : `(${xhr.status})`));
          return;
        }
        show(status, "Encoding the new file — the clip keeps playing until it lands");
        watch();
      };
      xhr.send(fd);
    });

    // A replacement already running when this page loaded: rejoin it rather
    // than offering a control whose only answer would be 409. The SERVER says
    // whether there is one — `data-replace-live`, the same attribute the trim
    // and the card image carry — so a clip nobody has replaced costs no
    // request and no 404 in the console (§20 erratum 240; it used to ask
    // GET …/replace on every owner view).
    if (form.hasAttribute("data-replace-live")) {
      setBusy(true);
      show(status, "A replacement is still encoding — the clip keeps playing until it lands");
      watch();
    }
  }

  // offerReplaceReload is offerReload's sibling for a landed replacement: the
  // owner has typed something unsaved, so the page is not reloaded under them.
  function offerReplaceReload(el) {
    if (!el) return;
    el.textContent = "Replaced — ";
    const a = document.createElement("a");
    a.href = "";
    a.textContent = "reload to see the new clip";
    el.appendChild(a);
    el.hidden = false;
  }
  function setupClipPage(root) {
    const isEmbed = root.hasAttribute("data-embed");
    // A READY clip can also carry a progress URL now — a better quality tier is
    // still encoding (§20 erratum 211) — and that poll is watchTiers', not this
    // one: the encode poll below reloads the page when it sees `ready`, which is
    // exactly the wrong thing to do to somebody who is already watching.
    const url = root.dataset.state === "ready" ? "" : root.dataset.progressUrl;
    const bar = $("[data-progress-bar]", root);
    const status = $("[data-status]", root);
    const hint = $("[data-copy-hint]", root);
    // The one line a clip with no card yet carries (§20 erratum 213). The ready
    // branch below normally RELOADS, which takes it away with everything else;
    // this reference is for the one path that cannot reload (an owner with typed
    // text to lose), where the sentence would otherwise outlive the card it is
    // about.
    const cardNote = $("[data-card-note]", root);

    const rail = $("[data-rail]", root);
    if (url) {
      let seen = root.dataset.state;
      poll(url, {
        update(p) {
          stateMoment(seen, p.state);
          seen = p.state;
          setRail(rail, p);
          setBar(bar, p);
          setText(status, stageLabel(p));
        },
        done(outcome, p) {
          if (outcome === "ready") {
            // A reload is how a processing page becomes a watch page — the
            // <video> is server-rendered — and it is also how typed text
            // disappears. The share page renders the WHOLE owner block while a
            // clip encodes, so an owner who clicked through from the upload and
            // started writing a title is inside a window of ten seconds to
            // several minutes (§20 erratum 95). Reload only when there is
            // nothing to lose; when there is, say the encode landed and hand
            // the reload over.
            if (!dirtyOwnerFields(root)) {
              // Once, after the mark's smile: the reloaded page renders a ready
              // row. The wait is up to a few seconds, which is long enough to
              // start typing a title in — so the fields are asked AGAIN when it
              // ends, and a title begun inside the window hands the reload
              // over exactly as one begun before it does (§20 erratum 95).
              afterMoment("ready", () => {
                if (!dirtyOwnerFields(root)) { location.reload(); return; }
                const progressEl = bar ? bar.parentElement : null;
                if (progressEl) progressEl.remove();
                if (hint) hint.remove();
                offerReload(status);
              });
              return;
            }
            moment("ready");
            const progressEl = bar ? bar.parentElement : null;
            if (progressEl) progressEl.remove();
            offerReload(status);
            if (hint) hint.remove(); // the card is a real player now
            // …but the CARD is ready only when the copy it fetches is. A Light
            // copy queued behind the flip leaves this note true, so it goes only
            // when the payload says no light tier is still encoding. When one is,
            // the note stays true and stays put: this branch has just offered the
            // reload, and a reloaded page renders the note the server decides.
            const lightComing = Array.isArray(p && p.tiers)
              && p.tiers.some((t) => t && t.tier === "light" && t.state === "encoding");
            if (cardNote && !lightComing) cardNote.remove();
            const lamp = $("[data-copy-lamp]", root);
            if (lamp) lamp.classList.remove("off"); // shareable: the lamp goes green
            return;
          }
          const progress = bar ? bar.parentElement : null;
          if (progress) progress.remove();
          if (outcome === "failed") {
            moment("failed");
            setText(status, "Processing failed: " + ((p && p.error) || "unknown error"));
            if (status) status.classList.add("failed");
          } else if (outcome === "unreachable") {
            setText(status, unreachableText);
          } else {
            setText(status, "This clip no longer exists.");
            if (hint) hint.remove();
          }
        },
      });
    }

    // View beacon: once per page, on the first play (crawlers never count).
    const video = $("video[data-view-url], audio[data-view-url]", root);
    if (video) {
      let sent = false;
      video.addEventListener("play", () => {
        if (sent) return;
        sent = true;
        try {
          fetch(video.dataset.viewUrl, {
            method: "POST",
            credentials: "same-origin",
            keepalive: true,
            headers: { "Content-Type": "application/json" },
          }).catch(() => {});
        } catch { /* ignore */ }
      });
    }
    if (isEmbed) {
      // The wordmark on the player (§20 erratum 246): on the poster and while
      // paused, gone while playing. The video's own events drive one attribute
      // on the body and app.css does the rest — no style attribute, no class
      // written from a timer. An autoplaying embed may already be playing by
      // the time this runs (the script is deferred), so the attribute starts
      // from the player's real state rather than from "poster".
      if (video) {
        const playing = (on) => {
          if (on) root.setAttribute("data-playing", "");
          else root.removeAttribute("data-playing");
        };
        video.addEventListener("play", () => playing(true));
        video.addEventListener("pause", () => playing(false));
        video.addEventListener("ended", () => playing(false));
        playing(!video.paused && !video.ended);
      }
      return;
    }

    // Everything below is the SHARE page's, the player's own keys included:
    // an embed's autoplay/muted/loop belong to whoever wrote the URL, and a
    // viewer preference must not override them (§20 erratum 77).
    setupPlayer(root);
    setupWaveform(root);
    // The quality selector, and the one poll a ready clip makes (§20 erratum
    // 211). It is EVERY viewer's control, not the owner's, so it is wired here
    // beside the player and not inside the owner block below.
    setupQuality(root);

    // Owner controls. `owner` also carries the clip's lifetime state: the
    // keep-alive URL, and data-expires while there is a countdown to reset.
    const owner = $("[data-owner-controls]", root);
    const apiURL = root.dataset.apiUrl;
    // The share block's own failure line: keep-forever and retry sit in a row
    // of buttons with no form and therefore no [data-error] of their own. It
    // is rendered beside that row, above the owner section, so it is looked up
    // on the page rather than inside `owner`.
    const ownerErr = $("[data-owner-error]", root);

    const copyBtn = $("[data-copy-link]", root);
    if (copyBtn) {
      copyBtn.addEventListener("click", async () => {
        // Copy FIRST and independently: the clipboard is what the click is
        // for, and a keep-alive that cannot reach the server must never cost
        // the owner the link.
        if (await copyText(root.dataset.pageUrl)) { flash($("[data-copied]", root)); copied(); }
        // …and only the OWNER touches. `owner` is null on a visitor's page
        // (nothing renders it), so a visitor copying a link resets nobody's
        // countdown — the keep-alive is the owner's signal, not a stranger's
        // (§20 erratum 62).
        await touchIfExpiring(owner);
      });
    }

    // "Copy embed": the server-rendered <iframe> for this clip, sized from the
    // rendition so the box matches the aspect. It is a clipboard write and
    // nothing else — no share targets, no third-party anything (§9.1).
    const embedBtn = $("[data-copy-embed]", root);
    if (embedBtn) {
      embedBtn.addEventListener("click", async () => {
        if (await copyText(embedBtn.dataset.embedCode || "")) { flash($("[data-copied]", root)); copied(); }
      });
    }

    const keep = $("[data-keep-forever]", root);
    if (keep) {
      keep.addEventListener("click", async () => {
        keep.disabled = true;
        hide(ownerErr);
        let r;
        try { r = await api("PATCH", apiURL, { ttl_days: 0 }); } catch { r = null; }
        keep.disabled = false;
        if (r && r.ok && r.data) {
          applyLifetime(owner, r.data);
          return;
        }
        if (!authFailed(ownerErr, r)) show(ownerErr, "Could not keep this clip: " + messageOf(r));
      });
    }

    setupTrim(root);
    setupGIF(root);
    setupPoster(root);
    setupReplace(root);
    paintLifetime(root);

    const edit = $("[data-edit]", root);
    if (edit) setupDetails(root, owner, edit, apiURL);
    const del = $("[data-delete]", root);
    if (del) {
      const hint = $("[data-delete-hint]", owner || root);
      const delErr = $("[data-delete-error]", owner || root);
      del.addEventListener("click", async () => {
        // Two clicks, not a confirm() dialog — the same pattern the collection
        // delete uses, and what the dialog said now sits in the hint beside it.
        if (!confirmClick(del, hint)) return;
        hide(delErr);
        del.disabled = true;
        let r;
        try { r = await api("DELETE", apiURL); } catch { r = null; }
        if (r && (r.status === 204 || r.status === 202)) {
          carry("deleted"); // the library that comes next gasps
          location.href = "/";
          return;
        }
        del.disabled = false;
        if (!authFailed(delErr, r)) show(delErr, "Delete failed: " + messageOf(r));
      });
    }
    const retry = $("[data-retry]", root);
    if (retry) {
      retry.addEventListener("click", async () => {
        retry.disabled = true;
        hide(ownerErr);
        let r;
        try { r = await api("POST", apiURL + "/retry"); } catch { r = null; }
        if (r && r.ok) {
          location.reload();
          return;
        }
        retry.disabled = false;
        if (!authFailed(ownerErr, r)) show(ownerErr, "Retry failed: " + messageOf(r));
      });
    }
  }

  // ---- the Details panel: ONE Save (web/STYLE.md; §20 erratum 240) ----------
  //
  // Title, description, lifetime and unlisted are one PATCH of the clip; the
  // tags are the tags PATCH; each collection box that changed is one request
  // against the single-clip membership route. Save fans out to the three and
  // says `saved` only once every one of them landed. Nothing reloads: what the
  // page shows — the title, the description, the lifetime line and rail, the
  // unlisted fact — is re-rendered from the answers, and every field's default
  // is moved to its new value so the ready-transition and replace reloads still
  // know there is nothing unsaved.
  //
  // A failed collection box goes BACK to what the server holds: a box that
  // stayed ticked after the server refused it would be the page lying about
  // what is stored, and the next reload would silently disagree with it.
  function setupDetails(root, owner, edit, apiURL) {
    const err = $("[data-error]", edit);
    const saved = $("[data-saved]", edit);
    const save = $("button[type=submit]", edit);
    const tagsInput = $("[data-tags-input]", edit);
    const tagsStatus = $("[data-tags-status]", edit);
    const colStatus = $("[data-collections-status]", edit);
    const unlistedState = $("[data-unlisted-state]", edit);
    const tagsDefault = () => splitTags(tagsInput ? tagsInput.defaultValue : "").join(",");

    edit.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      hide(err); hide(saved);
      if (save) save.disabled = true;
      const failures = [];

      // 1. the clip itself
      const body = {
        title: edit.elements.title.value.trim(),
        description: edit.elements.description.value.trim(),
        unlisted: edit.elements.unlisted.checked,
      };
      const ttl = edit.elements.ttl_days.value;
      if (ttl !== "") body.ttl_days = Number(ttl); // absent = unchanged; 0 = never
      let r;
      try { r = await api("PATCH", apiURL, body); } catch { r = null; }
      if (r && r.ok && r.data) {
        applyClipEdit(root, owner, edit, r.data);
      } else if (authFailed(err, r)) {
        if (save) save.disabled = false;
        return;
      } else {
        failures.push(messageOf(r, "could not save the details"));
      }

      // 2. the tags, only when the line changed
      if (tagsInput && splitTags(tagsInput.value).join(",") !== tagsDefault()) {
        let t;
        try { t = await api("PATCH", apiURL, { tags: splitTags(tagsInput.value) }); } catch { t = null; }
        if (t && t.ok && t.data && Array.isArray(t.data.tags)) {
          tagsInput.value = t.data.tags.join(", ");
          tagsInput.defaultValue = tagsInput.value;
        } else {
          failures.push(messageOf(t, "could not save the tags"));
        }
      }

      // 3. every collection box that changed, one request each
      const boxes = $$("[data-collection-toggle]", edit).filter((cb) => cb.checked !== cb.defaultChecked);
      for (const cb of boxes) {
        const want = cb.checked;
        let c;
        try { c = await api(want ? "POST" : "DELETE", cb.dataset.memberUrl); } catch { c = null; }
        if (c && c.ok) {
          cb.defaultChecked = want;
        } else {
          cb.checked = !want;
          failures.push(messageOf(c, want ? "could not add to the collection" : "could not remove from the collection"));
        }
      }

      if (save) save.disabled = false;
      if (failures.length) {
        show(err, failures[0]);
        return;
      }
      if (tagsStatus) setText(tagsStatus, "up to ten · seen by every account here · never on the watch page");
      if (colStatus) setText(colStatus, "does not change who can watch");
      if (unlistedState) setText(unlistedState, edit.elements.unlisted.checked ? "not in the library" : "listed in the library");
      flash(saved);
      moment("saved");
    });
  }

  // applyClipEdit re-renders what a clip PATCH changed, from the DTO the
  // server answered with, and moves every field's default to it.
  function applyClipEdit(root, owner, edit, dto) {
    const h1 = $("h1", root);
    if (h1 && typeof dto.title === "string") {
      h1.textContent = dto.title;
      document.title = dto.title + titleSuffix();
    }
    let desc = $(".description", root);
    const text = typeof dto.description === "string" ? dto.description : "";
    if (text) {
      if (!desc) {
        desc = document.createElement("p");
        desc.className = "description";
        const block = $(".titleblock", root) || root;
        block.appendChild(desc);
      }
      desc.textContent = text;
    } else if (desc) {
      desc.remove();
    }
    applyLifetime(owner, dto);
    // The Visibility fact, both ways: gone when the clip is listed again, added
    // when it has just become unlisted (the template renders it for one only).
    const vis = $("[data-visibility]", root);
    if (vis && !dto.unlisted) {
      vis.parentElement.remove();
    } else if (!vis && dto.unlisted) {
      const facts = $(".facts", root);
      if (facts) {
        const div = document.createElement("div");
        const dt = document.createElement("dt");
        dt.textContent = "Visibility";
        const dd = document.createElement("dd");
        dd.dataset.visibility = "";
        dd.textContent = "unlisted";
        div.appendChild(dt);
        div.appendChild(dd);
        // Where the template puts it: after Video and before the owner's Views.
        // Erratum 251's `Link` row used to sit between the two; §20 erratum 264
        // withdrew it, so Views is the only anchor left — and a null insert
        // (append) stays the answer when there is none.
        const views = $("[data-views]", facts);
        facts.insertBefore(div, views ? views.closest("div") : null);
      }
    }
    // No `Link — Memorable links are easier to guess.` line to show or hide here
    // (§20 erratum 264 withdrew erratum 251's row). `link_style` stays on the
    // DTO, which is the id's own grammar and is read by other clients; this page
    // spends it on nothing.
    ["title", "description"].forEach((name) => {
      const el = edit.elements[name];
      if (el) el.defaultValue = el.value;
    });
    edit.elements.unlisted.defaultChecked = edit.elements.unlisted.checked;
    edit.elements.ttl_days.value = ""; // the lifetime was applied; "no change" again
  }

  // ---- settings: password (§20 erratum 108) ---------------------------------

  // The form is submitted as the PATCH the API takes, so the page can report
  // the count of sessions it just ended. A success CLEARS the fields: leaving
  // a new password sitting in a form on a shared machine is exactly the shape
  // this page exists to fix.
  function setupPasswordForm(form) {
    const err = $("[data-password-error]", form);
    const ok = $("[data-password-ok]", form);
    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      hide(err); hide(ok);
      const button = $("button[type=submit]", form);
      if (button) button.disabled = true;
      let r;
      try {
        r = await api("PATCH", form.getAttribute("action"), {
          current_password: form.elements.current_password.value,
          new_password: form.elements.new_password.value,
        });
      } catch { r = null; }
      if (button) button.disabled = false;
      if (!r || !r.ok) {
        if (!authFailed(err, r)) show(err, messageOf(r, "could not reach the server"));
        return;
      }
      form.reset();
      show(ok, "Password changed. " + revokedText(r));
      moment("saved");
      // The rows the server just deleted are still on screen; drop them so
      // the list is not a lie until the next reload.
      keepOnlyCurrentSession();
    });
  }

  // ---- settings: sessions (§20 erratum 107) ---------------------------------

  // revokedText turns a {sessions_revoked} answer into the sentence both
  // revoking paths print, so "log out everywhere else" and the password
  // change cannot describe the same outcome two different ways.
  function revokedText(r) {
    const n = r && r.data && typeof r.data.sessions_revoked === "number" ? r.data.sessions_revoked : 0;
    if (n === 0) return "No other browser was signed in.";
    return n + (n === 1 ? " other browser was" : " other browsers were") + " signed out.";
  }

  // keepOnlyCurrentSession removes every session row except the one marked as
  // this browser. Both revoking paths do this to the DOM rather than
  // re-fetching: the server has already said what happened, and a second GET
  // would only add a round trip and a way to disagree with it.
  function keepOnlyCurrentSession() {
    const list = $("[data-session-list]");
    if (!list) return;
    $$("tr", list).forEach((row) => { if (!row.classList.contains("current")) row.remove(); });
  }

  function setupLogoutOthers(button) {
    const hint = $("[data-logout-others-hint]");
    const err = $("[data-sessions-error]");
    const ok = $("[data-sessions-ok]");
    button.addEventListener("click", async () => {
      if (!confirmClick(button, hint)) return;
      hide(err); hide(ok);
      button.disabled = true;
      let r;
      try { r = await api("DELETE", "/api/auth/sessions"); } catch { r = null; }
      button.disabled = false;
      if (!r || !r.ok) {
        if (!authFailed(err, r)) show(err, messageOf(r, "could not reach the server"));
        return;
      }
      show(ok, revokedText(r));
      keepOnlyCurrentSession();
    });
  }

  // ---- settings: Sign in with Discord (ROADMAP #44, §20 erratum 302) --------

  // setupDiscordLink POSTs the password and, on 200, NAVIGATES to the
  // authorize URL the server minted: a script navigation rather than a form
  // whose action is Discord, because CSP's form-action 'self' is enforced on
  // a form's redirect chain. The page makes no request off this origin.
  function setupDiscordLink(form) {
    const err = $("[data-discord-error]");
    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      hide(err);
      const button = $("button[type=submit]", form);
      if (button) button.disabled = true;
      let r;
      try {
        r = await api("POST", form.getAttribute("action"), {
          current_password: form.elements.current_password.value,
        });
      } catch { r = null; }
      if (r && r.ok && r.data && typeof r.data.url === "string") {
        location.href = r.data.url;
        return;
      }
      if (button) button.disabled = false;
      if (!authFailed(err, r)) show(err, messageOf(r, "could not reach the server"));
    });
  }

  // setupDiscordUnlink is a data-confirm-text button like Revoke; on 204 the
  // page reloads, and the panel comes back as the not-linked form.
  function setupDiscordUnlink(button) {
    const hint = $("[data-discord-unlink-hint]");
    const err = $("[data-discord-error]");
    button.addEventListener("click", async () => {
      if (!confirmClick(button, hint)) return;
      hide(err);
      button.disabled = true;
      let r;
      try { r = await api("DELETE", "/api/auth/discord"); } catch { r = null; }
      if (r && (r.ok || r.status === 404)) {
        location.reload();
        return;
      }
      button.disabled = false;
      if (!authFailed(err, r)) show(err, messageOf(r, "could not reach the server"));
    });
  }

  // ---- settings: API tokens -------------------------------------------------

  // offerSxcu turns the `sxcu` object of a mint response into a download
  // without a server round trip: the file is built here, from JSON that only
  // this response carried, so the raw token never appears in a GET URL, an
  // access log or a file on the server. One object URL at a time; the
  // previous one is revoked when a new token is minted.
  let sxcuURL = null;
  function offerSxcu(box, sxcu) {
    const wrap = $("[data-sxcu]", box);
    const link = $("[data-sxcu-download]", box);
    if (!wrap || !link) return;
    if (sxcuURL) { URL.revokeObjectURL(sxcuURL); sxcuURL = null; }
    if (!sxcu || typeof sxcu !== "object") {
      link.removeAttribute("href");
      hide(wrap);
      return;
    }
    sxcuURL = URL.createObjectURL(new Blob([JSON.stringify(sxcu, null, 2)], { type: "application/json" }));
    link.href = sxcuURL;
    link.download = "clippet.sxcu";
    show(wrap);
  }

  function setupTokens(form) {
    const err = $("[data-error]", form);
    const box = $("[data-new-token]");
    const value = $("[data-new-token-value]");
    const list = $("[data-token-list]");
    // The Kind control (§20 erratum 322): a capture token, or an inbound URL —
    // or, on an instance that answers the official bot, a Clippet Auto URL
    // (§20 erratum 324). Its words live in the template beside the words they
    // replace (data-*-inbound, data-*-autofetch); the token's own are whatever
    // the page rendered, read once here.
    const kinds = $$("[data-token-kind]", form);
    const scopesRow = $("[data-token-scopes]", form);
    const nameInput = form.elements.name;
    const submit = $("[data-token-submit]", form);
    const once = $("[data-once]", box || undefined);
    const tokenWords = {
      placeholder: nameInput ? nameInput.placeholder : "",
      label: submit ? submit.textContent : "",
      once: once ? once.textContent : "",
    };
    const kind = () => (kinds.find((k) => k.checked) || {}).value || "token";
    // pathKind: the suffix of the data- words of the two kinds whose secret
    // is a URL ("Inbound", "Autofetch"); null for a token.
    const pathKind = () => ({ inbound: "Inbound", autofetch: "Autofetch" })[kind()] || null;
    const follow = () => {
      const k = pathKind();
      if (scopesRow) scopesRow.hidden = !!k;
      if (nameInput) nameInput.placeholder = k ? nameInput.dataset["placeholder" + k] : tokenWords.placeholder;
      if (submit) submit.textContent = k ? submit.dataset["label" + k] : tokenWords.label;
    };
    kinds.forEach((k) => k.addEventListener("change", follow));
    // `reset` fires BEFORE the controls are reset, so the follow-up waits a
    // turn: after a mint the form is back on Token and every word with it.
    form.addEventListener("reset", () => setTimeout(follow, 0));
    follow();

    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      hide(err);
      // An inbound URL holds the scope `inbound` alone, a Clippet Auto URL the
      // scope `autofetch` alone; the checkboxes are hidden and not read.
      let scopes = [kind()];
      if (!pathKind()) {
        // The scope checkboxes (§20 erratum 111). Ticking none is caught here
        // rather than sent: the API answers 400 for an empty array, and a form
        // that can tell you before the round trip should.
        scopes = $$("input[name=scopes]:checked", form).map((box) => box.value);
        if (!scopes.length) {
          show(err, "Pick at least one thing the token may do.");
          return;
        }
      }
      let r;
      try {
        r = await api("POST", form.getAttribute("action"), { name: form.elements.name.value.trim(), scopes });
      } catch { r = null; }
      if (!r || !r.ok || !r.data || !r.data.token) {
        if (!authFailed(err, r)) show(err, messageOf(r));
        return;
      }
      // A path URL is shown WHOLE: the address is what a tool is given.
      const lead = tokenLead(r.data);
      const k = lead === "ci_" ? "Inbound" : lead === "ca_" ? "Autofetch" : null;
      setText(value, k && r.data.url ? r.data.url : r.data.token);
      if (once) setText(once, k ? once.dataset["once" + k] : tokenWords.once);
      offerSxcu(box, r.data.sxcu);
      show(box);
      form.reset();
      if (list) {
        list.prepend(buildTokenRow(r.data));
        hide($("[data-empty]"));
      }
    });

    const copy = $("[data-copy-token]");
    if (copy) {
      copy.addEventListener("click", async () => {
        if (await copyText(value.textContent)) { flash($("[data-copied]", box)); copied(); }
      });
    }
    // Revoke is two clicks, and the hint and the failure line are page-level
    // rather than per-row: a table cell is the wrong place for a sentence, and
    // only one button is ever armed at a time — arming a second disarms the
    // first, so the hint on screen always belongs to the button showing
    // "Really revoke".
    const revokeHint = $("[data-revoke-hint]");
    const tokenErr = $("[data-token-error]");
    let arming = null;
    document.addEventListener("click", async (ev) => {
      const btn = ev.target.closest("[data-revoke]");
      if (!btn) return;
      const row = btn.closest("[data-token-id]");
      if (!row) return;
      if (arming && arming !== btn) disarm(arming);
      if (!confirmClick(btn, revokeHint)) {
        arming = btn;
        return;
      }
      arming = null;
      hide(tokenErr);
      btn.disabled = true;
      let r;
      try { r = await api("DELETE", "/api/auth/tokens/" + encodeURIComponent(row.dataset.tokenId)); } catch { r = null; }
      if (r && (r.status === 204 || r.status === 404)) {
        row.remove();
        if (list && !list.children.length) show($("[data-empty]"));
        return;
      }
      btn.disabled = false;
      if (!authFailed(tokenErr, r)) show(tokenErr, "Revoke failed: " + messageOf(r));
    });
  }

  // tokenLead (named as internal/web's is): a row whose scopes are exactly ["inbound"] is an inbound URL
  // (§20 erratum 322), shown as ci_…; exactly ["autofetch"] a Clippet Auto URL
  // (§20 erratum 324), shown as ca_…; every other row is a ck_ token.
  function tokenLead(t) {
    const one = Array.isArray(t.scopes) && t.scopes.length === 1 ? t.scopes[0] : "";
    return one === "inbound" ? "ci_" : one === "autofetch" ? "ca_" : "ck_";
  }

  function buildTokenRow(t) {
    const tr = document.createElement("tr");
    tr.dataset.tokenId = t.id;
    // Same columns, same order, as the server-rendered row in settings.html:
    // Name, Prefix, Scopes, Created, Last used, Revoke.
    const scopes = Array.isArray(t.scopes) ? t.scopes.join(", ") : "";
    const cells = [t.name, null, scopes, fmtDate(t.created_at), "never"];
    cells.forEach((text, i) => {
      const td = document.createElement("td");
      if (i > 0) td.className = "m";
      if (i === 1) {
        const code = document.createElement("code");
        code.textContent = tokenLead(t) + t.prefix + "…";
        td.appendChild(code);
      } else {
        td.textContent = text;
      }
      tr.appendChild(td);
    });
    const td = document.createElement("td");
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "btn danger small";
    btn.dataset.revoke = "";
    btn.dataset.confirmText = "Really revoke"; // the template's own second-click label
    btn.textContent = "Revoke";
    td.appendChild(btn);
    tr.appendChild(td);
    return tr;
  }

  // ---- boot ---------------------------------------------------------------

  // ---- account settings (ROADMAP #22) ------------------------------------
  //
  // Two preferences, one PATCH, and a null that MEANS something: an empty TTL
  // selection sends `null`, which is "follow the server default", and is a
  // different request from `0`, which is "keep my clips forever". The select's
  // first option is the former and says what the server default currently is.
  function setupDefaults(section) {
    const url = section.dataset.defaultsUrl;
    const ttl = $("[data-defaults-ttl]", section);
    const unlisted = $("[data-defaults-unlisted]", section);
    const links = $$("[data-defaults-link]", section);
    const save = $("[data-defaults-save]", section);
    const status = $("[data-defaults-status]", section);
    const err = $("[data-defaults-error]", section);
    if (!url || !ttl || !unlisted || !save) return;
    // The link grammar has TWO states and no null: every account has one, so it
    // is a pair of radios rather than a select with a "server default" entry
    // (§20 erratum 250). One is always checked, and the fallback below only
    // matters if a future template ships none.
    const linkStyle = () => (links.find((r) => r.checked) || {}).value || "compact";
    save.addEventListener("click", async () => {
      hide(status);
      hide(err);
      save.disabled = true;
      const body = {
        default_ttl_days: ttl.value === "" ? null : Number(ttl.value),
        default_unlisted: unlisted.checked,
        default_link_style: linkStyle(),
      };
      let r;
      try { r = await api("PATCH", url, body); } catch { r = null; }
      save.disabled = false;
      if (r && r.ok && r.data) {
        // Re-render from the ANSWER, not from what was sent: the server is
        // where the preference lives, and a page showing a value it merely
        // hoped for is a page that lies after a refused request.
        ttl.value = r.data.default_ttl_days === null || r.data.default_ttl_days === undefined
          ? "" : String(r.data.default_ttl_days);
        unlisted.checked = !!r.data.default_unlisted;
        links.forEach((r2) => { r2.checked = r2.value === r.data.default_link_style; });
        flash(status, "saved");
        moment("saved");
        return;
      }
      if (!authFailed(err, r)) show(err, messageOf(r, "could not save the defaults"));
    });
  }

  // ---- /setup: the first-run wizard (ROADMAP #45, §20 errata 297-299, 311) -----
  //
  // Three small jobs, none of which the page NEEDS: the chooser is a form GET
  // and works with JavaScript off (the shape erratum 242 set), the block is
  // `user-select: all` so one click selects the lot, and `admin doctor` is the
  // same probe Check now runs. This is the faster half of each.
  function setupWizard(section) {
    // The chooser submits itself when either control changes — a storage
    // backend card or a Reached chip, both radios (§20 errata 309, 311) — and
    // `Show` — the no-JavaScript half — goes away with its row, exactly as
    // the theme form's Apply does.
    const form = $("form[data-setup-form]", section);
    if (form) {
      $$("input[type=radio]", form).forEach((el) => el.addEventListener("change", () => form.submit()));
      const show = $("[data-setup-show]", form);
      if (show) (show.closest(".row") || show).hidden = true;
    }
    // Copy: the value lives in this button's OWN row — or, for the block, in
    // the walk's body beside its `your .env` head — so nothing here has to
    // know which row it is on, and a future row with a block or a command is
    // wired by the markup alone.
    $$("[data-setup-copy]", section).forEach((btn) => {
      btn.addEventListener("click", async () => {
        const row = btn.closest(".row, .walk-body");
        const src = row && $("[data-setup-block], [data-setup-cmd]", row);
        if (!src) return;
        const label = btn.textContent;
        if (await copyText(src.textContent)) {
          btn.textContent = "copied";
          moment("copied");
          setTimeout(() => { btn.textContent = label; }, 1500);
        }
      });
    });
    // Check now: the identical probe `admin doctor` performs — Ping, put one
    // small object, remove it — through the store this instance is using RIGHT
    // NOW. The answer is the bounded class and nothing else, so there is
    // nothing here to redact.
    const check = $("[data-setup-check]", section);
    const cell = $("[data-setup-check-state]", section);
    const panel = $("[data-setup-state]", section);
    const err = $("[data-setup-error]", section);
    if (!check) return;
    check.addEventListener("click", async () => {
      hide(err);
      check.disabled = true;
      setText(cell, "checking…");
      let r;
      try { r = await api("POST", "/api/setup/check"); } catch { r = null; }
      check.disabled = false;
      if (r && r.ok && r.data && typeof r.data.state === "string") {
        setText(cell, r.data.state);
        setText(panel, r.data.state);
        moment(r.data.ok ? "saved" : "failed");
        return;
      }
      setText(cell, "unavailable");
      if (!authFailed(err, r)) show(err, messageOf(r, "could not ask the store"));
    });
  }

  // ---- the theme (§20 errata 242 and 288; web/STYLE.md) ----------------------
  //
  // The cookie the page handlers read is written HERE for a browser with a
  // client, and the Apply button — the form's no-JavaScript half — goes away.
  // The theme lands at once: `data-theme` on <html> is what the stylesheet's
  // second dark block keys on. A year, SameSite=Lax, Secure on https; never
  // HttpOnly, because this is where it is written.
  //
  // TWO controls write it — Settings' three-way picker and the top bar's glyph
  // (erratum 288) — so there is one function that does the writing and they
  // both call it. The last line is why: the two are faces of one choice, and a
  // glyph pressed on the Settings page must move the radio beside it. The glyph
  // needs no such syncing in the other direction, because WHICH of its two
  // buttons exists is decided by `data-theme` in the stylesheet.
  function applyTheme(v) {
    const root = document.documentElement;
    if (v === "light" || v === "dark") root.setAttribute("data-theme", v);
    else root.removeAttribute("data-theme");
    const secure = location.protocol === "https:" ? "; Secure" : "";
    document.cookie = v === "light" || v === "dark"
      ? `theme=${v}; Path=/; Max-Age=31536000; SameSite=Lax${secure}`
      : `theme=; Path=/; Max-Age=0; SameSite=Lax${secure}`;
    $$("[data-theme-pick]").forEach((radio) => { radio.checked = radio.value === (v || "system"); });
  }

  function setupTheme(form) {
    hide($("[data-theme-apply]", form));
    const status = $("[data-theme-status]", form);
    $$("[data-theme-pick]", form).forEach((radio) => {
      radio.addEventListener("change", () => {
        if (!radio.checked) return;
        applyTheme(radio.value);
        flash(status);
        moment("saved");
      });
    });
  }

  // The bar's glyph (§20 erratum 288): the same form, enhanced the same way.
  // The click is cancelled rather than the submit, so the press never leaves
  // the page — no navigation, no reload, nothing reloaded off the network — and
  // the swap to the other glyph is the attribute's own doing. A browser with no
  // script submits the form instead and lands back on `next`.
  // The focus has to move with the swap: the button that was pressed is the one
  // the stylesheet then hides, and an element that becomes `display: none` while
  // it HAS the focus drops it on the floor — the next Tab starts again at the
  // top of the document, so a keyboard user cannot press the glyph twice. The
  // other button stands in exactly its place, so it takes the focus, and only
  // when the press carried it (a mouse click on an engine that does not focus
  // buttons must not grow a focus ring).
  function setupThemeGlyph(form) {
    const buttons = $$("[data-theme-to]", form);
    buttons.forEach((button) => {
      button.addEventListener("click", (e) => {
        e.preventDefault();
        const carried = document.activeElement === button;
        applyTheme(button.getAttribute("data-theme-to"));
        const other = buttons.find((b) => b !== button);
        if (carried && other) other.focus();
      });
    });
  }

  // ---- the living mark: the eyes follow the cursor, and the mark reacts (ROADMAP #38; §20 errata 267, 273, 274; web/STYLE.md) ----
  //
  // Every qualifying wordmark gets an EMPTY svg overlay over its `pp` at boot:
  // the p's counters are the eyes and the green span is the mouth, and it
  // paints nothing at rest. Two things move it. A hovering pointer inside the
  // mark's TRACKING AREA (the header bar; the whole page for the display mark)
  // brings two ink pupils up into the counters to converge on the pointer and
  // roll after it, and takes them away when it leaves (errata 267, 273). And a
  // FACE — one of eight named storyboards from the exploration (erratum 274) —
  // that the app plays at the moments it already knows: FACE_OF below is the
  // whole table, `moment()` is the one bus, and an ambient timer draws a glance
  // or a drift (rarely a wink) every 45–90 s while nothing else is happening.
  // Font units (2000/em, y down): `.pp` is a 1 em flex item, so the viewBox maps
  // 1:1 at any size. Which marks live: one inside a tracking area, one that
  // arrives wearing a `data-face` (the 404's), and the embed's — mouth only, no
  // eyes, one face (`smile` on `ready`) and nothing else, ever. Nothing at all
  // without a pointer that hovers or under reduced motion. Attribute writes
  // only: no style attribute, no markup string, nothing the CSP would refuse.
  const EYE = { cx: 628, cy: -560, r: 150 }; // r: the owner's pick, one constant for both marks (erratum 274; was 125)
  const EYE_ADV = 1146; // the second p
  const ORBIT = { rx: 110, ry: 150 }; // the ellipse a pupil rides inside its counter
  // the first p's counter from the 600-weight outline scripts/lib/mkcard.py's WORD_PATH carries (there
  // the double p sits at PP_ORIGIN = 2039; these coordinates are the pp's own origin), closed along the stem
  const COUNTER = "M400 -318L417 -318Q425 -286 449 -260Q473 -235 514 -220Q555 -206 613 -206Q719 -206 782 -288Q845 -369 845 -550Q845 -713 785 -797Q725 -881 621 -881Q560 -881 516 -864Q473 -847 448 -824Q423 -800 417 -778L400 -778Z";
  // the whole box minus the two stems (evenodd): a mouth may bow, drop or pinch but never paints over ink
  const NOSTEMS = "M-100 -1727H2400V1200H-100Z M142 -1091H412V403H142Z M1288 -1091H1558V403H1288Z";
  // the span, literally: y 603–803 (erratum 271) is a 200-unit stroke centred on 703
  const MOUTH_REST = "M242 703L1463 703";
  // px/py move both pupils; ax/ay and bx/by are per eye (the follow's converging pupils);
  // ps is the pupil's scale, op its opacity; lidA/lidB the upper lids, loA/loB the lower;
  // sag bows the mouth (down = smile, up = frown), drop lowers it, pinch draws its ends in.
  const MARK_REST = { op: 0, ps: 1, px: 0, py: 0, ax: 0, ay: 0, bx: 0, by: 0, lidA: 0, lidB: 0, loA: 0, loB: 0, sag: 0, drop: 0, pinch: 0 };
  const EASE = {
    io: (t) => (t < 0.5 ? 2 * t * t : 1 - Math.pow(-2 * t + 2, 2) / 2),
    out: (t) => 1 - Math.pow(1 - t, 3),
    in: (t) => t * t * t,
    back: (t) => 1 + 2.7 * Math.pow(t - 1, 3) + 1.7 * Math.pow(t - 1, 2),
  };
  // A storyboard is a list of steps; a step is a duration and the tracks that
  // tween during it (a hold has none, a `par` has several of their own lengths).
  // `enter` snaps the pupils to half size, 40 units low and invisible FIRST, so
  // no pupil ever pops — unless they are already out under the pointer, in
  // which case a face starts from where they are.
  const tw = (to, ms, e) => ({ ms, tracks: [{ to, ms, e: e || EASE.io }] });
  const hold = (ms) => ({ ms, tracks: [] });
  const par = (...steps) => ({
    ms: Math.max(...steps.map((s) => s.ms)),
    from: Object.assign({}, ...steps.map((s) => s.from || {})),
    tracks: steps.flatMap((s) => s.tracks),
  });
  const enter = (extra, ms) => ({ ms: ms || 240, from: { op: 0, ps: 0.5, py: 40 }, tracks: [{ to: { op: 1, ps: 1, py: 0, ...extra }, ms: ms || 240, e: EASE.out }] });
  const leave = (extra, ms) => tw({ op: 0, ps: 0.6, py: 30, ...extra }, ms || 200, EASE.io);
  // The eight faces: the exploration's round-3/4 storyboards, timings and
  // easings verbatim (the exploration's own mock; the owner's note: slower, more human).
  const FACES = {
    // overshoot-and-settle into a left dwell, an ease-out arrival right, home
    glance: () => [enter({}, 260), tw({ px: -150 }, 200, EASE.back), hold(260), tw({ px: 150 }, 260, EASE.out), hold(300), tw({ px: 0 }, 220), leave({}, 220)],
    // the upper lids sweep down and up
    blink: () => [enter(), hold(100), tw({ lidA: 1, lidB: 1 }, 80, EASE.in), hold(60), tw({ lidA: 0, lidB: 0 }, 120, EASE.out), hold(120), leave()],
    // the right lid only, slower, and it stays down; a touch of lower lid
    wink: () => [enter({}, 260), hold(120), tw({ lidB: 1, loB: 0.35 }, 140, EASE.in), hold(300), tw({ lidB: 0, loB: 0 }, 200, EASE.out), hold(120), leave({}, 220)],
    // a slow drift to a random point and back
    drift: () => {
      const a = Math.random() * Math.PI * 2, d = 70 + Math.random() * 60;
      return [enter({}, 400), tw({ px: Math.cos(a) * d, py: Math.sin(a) * d * 0.6 }, 420, EASE.out), hold(600), tw({ px: 0, py: 0 }, 360), leave({}, 400)];
    },
    // the span bows, holds, flattens
    smile: () => [tw({ sag: 1 }, 520, EASE.out), hold(700), tw({ sag: 0 }, 480)],
    // smile + pupils + a lower-lid squint
    beam: () => [par(enter({}, 400), tw({ sag: 1 }, 480, EASE.out)), tw({ loA: 0.45, loB: 0.45, py: -40 }, 240, EASE.out), hold(700), tw({ loA: 0, loB: 0, py: 0, sag: 0 }, 420), leave({}, 300)],
    // small pupils rise as the span drops and pinches to an "o"; a long wide-eyed hold
    gasp: () => [par(enter({ ps: 0.72, py: -90 }, 260), tw({ drop: 1, pinch: 1 }, 300, EASE.out)), hold(900), par(tw({ py: 0, ps: 1 }, 360), tw({ drop: 0, pinch: 0 }, 400)), leave({}, 300)],
    // the bow rises as the whole mouth drops, so the ends stay at the tips; the mouth is --danger (app.css `sad`)
    sulk: () => [tw({ sag: -0.85, drop: 0.6 }, 520, EASE.out), hold(700), tw({ sag: 0, drop: 0 }, 480)],
  };
  const MEANINGFUL = new Set(["blink", "smile", "beam", "gasp", "sulk"]);
  // THE TABLE (the owner's roles table, 2026-09-11; erratum 274): which moment the
  // app knows plays which face, on every mark that has one. The one place.
  const FACE_OF = {
    landed: "blink",    // the upload was received: the row appears
    queued: "smile",    // in, the encode not started
    encoding: "blink",  // the worker is on it
    ready: "smile",     // the encode finished: shareable (the embed's ONE face)
    failed: "sulk",     // the clip could not be made — the red mouth
    copied: "beam",     // Copy link / Copy embed / Copy pressed
    firstcopy: "smile", // …the first time this tab copies anything
    saved: "beam",      // settings or an edit saved
    deleted: "gasp",    // a clip (or a collection) removed
    signedin: "smile",  // the library appears after a login or a registration
    notfound: "gasp",   // the 404: its template stamps data-face="gasp" on the mark
  };
  // The ambient timer: one per page, every 45–90 s while nothing else moves, a
  // glance and a drift alternating (never the same twice) and about one draw in
  // twelve a wink — a few times an hour. It pauses while the eyes are out under
  // the pointer, while any face plays and while the tab is hidden, and never
  // fires within 5 s of a meaningful face.
  const AMBIENT = ["glance", "drift"];
  const RARE = "wink";
  const AMBIENT_MS = { min: 45000, max: 90000, guard: 5000, tick: 500 };
  const marks = []; // every mark with an overlay: the bus and the timer play on all of them
  let meaningfulEnd = -Infinity;
  let eyesN = 0;
  function setupEyes(pp) {
    // One overlay per mark, ever: boot() runs once today, but any future path
    // that re-boots (a page swap, a re-render) would otherwise inject a second
    // svg over the first and animate both (§20 erratum 275).
    if (pp.querySelector(":scope > svg.eyes")) return;
    const mark = pp.closest(".wordmark");
    const slot = pp.closest("header.top, .auth");
    const embed = mark.classList.contains("embed-mark");
    if (!slot && !embed && !mark.hasAttribute("data-face")) return;
    // The display mark (front door, login, register) tracks the WHOLE page: the mark is the only
    // thing on it, and the card alone was a small target on a minimal page (the owner's call, §20 erratum 273).
    // The header mark keeps the bar. The 404's mark and the embed's have no area at all.
    const area = !slot ? null : slot.matches(".auth") ? document.documentElement : slot;
    const ns = "http:" + "//www.w3.org/2000/svg"; // split: the URL tripwire reads every line
    const el = (name, attrs, parent) => {
      const e = document.createElementNS(ns, name);
      for (const k in attrs) e.setAttribute(k, attrs[k]);
      parent.appendChild(e);
      return e;
    };
    const id = "eyes" + eyesN++;
    const svg = el("svg", { class: "eyes", viewBox: "0 -1727 2300 2000", preserveAspectRatio: "none", "aria-hidden": "true" }, pp);
    const defs = el("defs", {}, svg);
    // The eye region is the counter offset 110 units OUTWARD, as a stroked
    // luminance mask: the glyph's antialiased fringe is ~100 units at 16 px, and
    // a pupil pushed to the edge must be cut by the bowl, never sit on the ground.
    const mask = el("mask", { id, maskUnits: "userSpaceOnUse", x: -1000, y: -3000, width: 5000, height: 6000 }, defs);
    el("path", { d: COUNTER, fill: "#fff", stroke: "#fff", "stroke-width": 220, "stroke-linejoin": "round" }, mask);
    el("path", { "clip-rule": "evenodd", d: NOSTEMS }, el("clipPath", { id: id + "m" }, defs));
    // The mouth: the span redrawn as a round 200-unit stroke, clipped from the stems, drawn only
    // while it moves — the span itself hides under it then (`mouthing`, app.css), so the swap is
    // invisible and a red frown is the stroke, never a repaint of the span.
    const mouth = el("path", { class: "mouth", "clip-path": `url(#${id}m)`, fill: "none", "stroke-width": 200, "stroke-linecap": "round", visibility: "hidden", d: MOUTH_REST }, svg);
    // The eyes: a pupil and two lids per counter — not on the embed, whose 16 px pupils would be
    // two-pixel dots on someone else's page. A lid is a 1000-unit rect; closed, it sits at y -1000..0
    // and swallows the eye region (-991..-96) with room to spare; PARKED it is 1200 units out, not the
    // exploration's 1000: the region's edge is 9 units from a lid parked at 1000, a tenth of a pixel
    // at 21 px, and the two antialiased edges met in one faintly darker row at rest. 1200 is two
    // pixels clear at 16 px, and the rest frames are pixel-identical again.
    const eyes = embed ? [] : [0, EYE_ADV].map((dx) => {
      const g = el("g", { mask: `url(#${id})`, fill: "currentColor", transform: `translate(${dx} 0)` }, svg);
      return {
        pupil: el("circle", { cx: EYE.cx, cy: EYE.cy, r: EYE.r, opacity: 0 }, g),
        lid: el("rect", { class: "lid", x: 300, y: -1000, width: 700, height: 1000, transform: "translate(0 -1200)" }, g),
        lo: el("rect", { class: "lo", x: 300, y: -1000, width: 700, height: 1000, transform: "translate(0 1200)" }, g),
      };
    });
    const st = { ...MARK_REST };
    // one quadratic bow: the ends stay welded to the stems, the middle sags by sag × 230 units
    const mouthPath = () => {
      const y = 703 + 160 * st.drop, k = 230 * st.sag, inset = 470 * st.pinch;
      const x1 = 242 + inset, x2 = 1463 - inset;
      return `M${x1} ${y}Q${(x1 + x2) / 2} ${y + 2 * k} ${x2} ${y}`;
    };
    // a lid's travel is the exploration's (closed at 0, 1000 units per unit of lid); at exactly 0 it parks
    const lidT = (v) => (v === 0 ? -1200 : -1000 + 1000 * v);
    const loT = (v) => (v === 0 ? 1200 : 1000 - 1000 * v);
    function render() {
      eyes.forEach((e, i) => {
        e.pupil.setAttribute("cx", EYE.cx + st.px + (i ? st.bx : st.ax));
        e.pupil.setAttribute("cy", EYE.cy + st.py + (i ? st.by : st.ay));
        e.pupil.setAttribute("r", EYE.r * st.ps);
        e.pupil.setAttribute("opacity", st.op);
        e.lid.setAttribute("transform", `translate(0 ${lidT(i ? st.lidB : st.lidA)})`);
        e.lo.setAttribute("transform", `translate(0 ${loT(i ? st.loB : st.loA)})`);
      });
      const mouthing = st.sag !== 0 || st.drop !== 0 || st.pinch !== 0;
      pp.classList.toggle("mouthing", mouthing);
      mouth.setAttribute("visibility", mouthing ? "visible" : "hidden");
      mouth.setAttribute("d", mouthing ? mouthPath() : MOUTH_REST);
    }
    // Where the pupils point: each on its own ellipse, at the pointer's angle
    // from its own eye's centre, so the two converge on the same spot.
    // The two axes have scales of their own: the overlay is
    // `preserveAspectRatio="none"` over a 2300 x 2000 viewBox, so a `.pp` whose
    // box is not exactly 1.15:1 stretches x and y by different factors (24.95 x
    // 21.00 at the header's 21 px — 3.3 % apart, §20 erratum 275). Mapping y
    // with the x scale put the eye's centre a few tenths of a pixel off and
    // tilted every aim by that much.
    function aim(x, y) {
      const r = pp.getBoundingClientRect(), ux = r.width / 2300, uy = r.height / 2000, t = {};
      [["ax", "ay", 0], ["bx", "by", EYE_ADV]].forEach(([kx, ky, dx]) => {
        const a = Math.atan2(y - (r.top + (EYE.cy + 1727) * uy), x - (r.left + (EYE.cx + dx) * ux));
        t[kx] = Math.cos(a) * ORBIT.rx;
        t[ky] = Math.sin(a) * ORBIT.ry;
      });
      return t;
    }
    // ONE state machine per mark, one frame loop while anything moves. The
    // follow (267): the entrance (half size, 40 units low, invisible → in place,
    // 260 ms cubic-out), then the follow (18 % of the way to the aim each
    // frame), then the exit (fade, shrink to 60 %, sink 30 units, 300 ms). A
    // FACE: the storyboard's steps in order, then rest. Never two faces at once:
    // a face that arrives while one plays QUEUES behind it (two deep, never the
    // same twice in a row — a batch of twenty uploads is one blink, not
    // twenty); a face that arrives while the eyes are out under the pointer
    // starts from where they are; a pointer that moves mid-face waits, and
    // brings the eyes out on its next move once the face is over. Exact rest
    // after every face and every exit: every attribute is the injected value
    // again, which the DOM test compares as markup.
    const IN = 1, FOLLOW = 2, OUT = 3, FACE = 4;
    let mode = 0, raf = 0, t0 = 0, from = null, to = null, cur = null;
    let face = "", steps = null, si = 0, settled = null;
    const queue = []; // { name, res }: what plays next, and whom to tell when it has
    function frame(now) {
      raf = 0;
      if (!pp.isConnected) { mode = 0; return; }
      if (mode === FACE) {
        const step = steps[si], dt = now - t0;
        step.tracks.forEach((tr) => {
          const k = tr.e(Math.min(1, dt / tr.ms));
          for (const key in tr.to) st[key] = tr.from[key] + (tr.to[key] - tr.from[key]) * k;
        });
        render();
        if (dt >= step.ms) {
          si++;
          if (si < steps.length) startStep(now);
          else { settle(); return; }
        }
      } else if (mode === IN || mode === OUT) {
        const t = Math.min(1, (now - t0) / (mode === IN ? 260 : 300));
        const k = mode === IN ? EASE.out(t) : EASE.io(t);
        for (const key in to) st[key] = from[key] + (to[key] - from[key]) * k;
        render();
        if (t >= 1) {
          if (mode === OUT) { Object.assign(st, MARK_REST); render(); mode = 0; return; }
          mode = FOLLOW;
        }
      } else {
        const a = aim(cur.x, cur.y);
        for (const key in a) st[key] += (a[key] - st[key]) * 0.18;
        render();
      }
      raf = requestAnimationFrame(frame);
    }
    const loop = () => { if (!raf) raf = requestAnimationFrame(frame); };
    const go = (m, target) => {
      mode = m; t0 = performance.now(); from = { ...st }; to = target;
      loop();
    };
    function startStep(now) {
      const step = steps[si];
      t0 = now;
      if (step.from && st.op === 0) Object.assign(st, step.from);
      step.tracks.forEach((tr) => {
        tr.from = {};
        for (const key in tr.to) tr.from[key] = st[key];
      });
    }
    function begin(name, res) {
      steps = FACES[name](); si = 0; face = name; mode = FACE; settled = res;
      mark.classList.toggle("sad", name === "sulk");
      mark.setAttribute("data-face", name);
      startStep(performance.now());
      loop();
    }
    function settle() {
      // A face that carries no `op` track (smile, sulk) can end while the eyes
      // are still OUT under the pointer — an owner hovering the bar as a clip
      // goes ready. Snapping to rest there extinguishes lit pupils in one
      // frame; every other face fades them through leave(). So: rest at once
      // when the eyes are already closed, the OUT tween (which ends at
      // MARK_REST itself) when they are not. The next pointer move re-enters.
      const lit = st.op > 0;
      if (!lit) { Object.assign(st, MARK_REST); render(); }
      mark.classList.remove("sad");
      mark.removeAttribute("data-face");
      if (MEANINGFUL.has(face)) meaningfulEnd = performance.now();
      mode = 0; face = ""; steps = null;
      const done = settled; settled = null;
      if (lit) go(OUT, { ...MARK_REST, ps: 0.6, py: 30 });
      if (done) done();
      // a breath between two faces, so they read as two
      if (queue.length) setTimeout(() => { const next = queue.shift(); if (mode !== FACE) begin(next.name, next.res); else next.res(); }, 300);
    }
    // play(name, why): the bus's entry. Resolves when THIS mark has finished
    // the face (at once when it will not play it), so a page that is about to
    // reload can let the face land first. The embed answers `ready` alone;
    // `wear` is a mark that arrived with a data-face (the 404's gasp).
    function play(name, why) {
      if (!FACES[name]) return Promise.resolve();
      if (embed && why !== "ready" && why !== "wear") return Promise.resolve();
      if (mode === FACE) {
        const tail = queue.length ? queue[queue.length - 1].name : face;
        if (queue.length >= 2 || tail === name) return Promise.resolve();
        return new Promise((res) => queue.push({ name, res }));
      }
      return new Promise((res) => begin(name, res));
    }
    marks.push({ el: mark, eyes: eyes.length > 0, play, busy: () => mode !== 0 });
    if (!area) return;
    // The entrance waits for the first MOVE inside the area, never the enter alone: Chromium hands a
    // freshly laid-out page a pointerenter at wherever the pointer already sits, with no move behind
    // it, and a mark that answered that would look at a cursor nobody is holding (erratum 273; the
    // visual pass relies on it now that the display mark's area is the whole page).
    area.addEventListener("pointermove", (e) => {
      if (e.pointerType === "touch") return;
      cur = { x: e.clientX, y: e.clientY };
      if (mode === IN || mode === FOLLOW || mode === FACE) return;
      if (mode === 0) Object.assign(st, { op: 0, ps: 0.5, py: 40 });
      go(IN, { op: 1, ps: 1, py: 0, ...aim(cur.x, cur.y) });
    });
    area.addEventListener("pointerleave", () => { if (mode && mode !== FACE) go(OUT, { op: 0, ps: 0.6, py: 30, ax: 0, ay: 0, bx: 0, by: 0 }); });
  }
  // moment(name) is the bus: the app names the moment, the table names the
  // face, every mark plays it. Resolves when every mark is at rest again.
  function moment(name) {
    const face = FACE_OF[name];
    if (!face) return Promise.resolve();
    return Promise.all(marks.map((m) => m.play(face, name)));
  }
  // afterMoment(name, fn): let the face land, then do the thing (a reload) —
  // bounded, so a page never waits on a face for longer than one takes.
  function afterMoment(name, fn) {
    let once = () => { once = () => {}; fn(); };
    moment(name).then(() => once());
    setTimeout(() => once(), 4000);
  }
  // carry(name): a moment whose page is about to go — a sign-in, a delete
  // that leaves for the library — is played by the page that comes next.
  const CARRY_KEY = "clippet.face";
  const COPIED_KEY = "clippet.copied";
  function carry(name) {
    try { sessionStorage.setItem(CARRY_KEY, name); } catch { /* no storage: no face on the next page */ }
  }
  // copied(): the first copy of this tab's session is its own moment.
  function copied() {
    let first = false;
    try {
      first = !sessionStorage.getItem(COPIED_KEY);
      if (first) sessionStorage.setItem(COPIED_KEY, "1");
    } catch { /* no storage: never "first" */ }
    return moment(first ? "firstcopy" : "copied");
  }
  function startAmbient() {
    if (!marks.some((m) => m.eyes)) return; // the embed alone: no timer
    const draw = () => AMBIENT_MS.min + Math.random() * (AMBIENT_MS.max - AMBIENT_MS.min);
    let due = draw(), acc = 0, last = "", prev = performance.now();
    const tick = () => {
      const now = performance.now();
      const dt = Math.min(now - prev, AMBIENT_MS.tick); // a throttled background tick counts as one tick, not the gap
      prev = now;
      const idle = document.visibilityState !== "hidden" && !marks.some((m) => m.busy()) && now - meaningfulEnd >= AMBIENT_MS.guard;
      if (idle) acc += dt;
      if (acc >= due) {
        acc = 0; due = draw();
        const name = Math.random() < 1 / 12 ? RARE : (last === AMBIENT[0] ? AMBIENT[1] : AMBIENT[0]);
        if (name !== RARE) last = name;
        marks.forEach((m) => { if (m.eyes) m.play(name, "ambient"); });
      }
      setTimeout(tick, AMBIENT_MS.tick);
    };
    setTimeout(tick, AMBIENT_MS.tick);
  }
  function setupEyesAll() {
    const worn = $$(".wordmark[data-face]");
    if (matchMedia("(prefers-reduced-motion: reduce)").matches || !matchMedia("(hover: hover)").matches) {
      worn.forEach((m) => m.removeAttribute("data-face")); // the shipped mark, exactly
      return;
    }
    $$(".wordmark .pp").forEach(setupEyes);
    // a mark that arrived wearing a face (the 404's gasp) plays it now
    worn.forEach((w) => {
      const name = w.getAttribute("data-face");
      w.removeAttribute("data-face");
      marks.forEach((m) => { if (m.el === w) m.play(name, "wear"); });
    });
    // …and a moment the previous page carried over
    let carried = "";
    try {
      carried = sessionStorage.getItem(CARRY_KEY) || "";
      if (carried) sessionStorage.removeItem(CARRY_KEY);
    } catch { carried = ""; }
    if (carried) moment(carried);
    startAmbient();
  }

  function boot() {
    setupTimes();
    setupEyesAll();
    $$("form[data-theme-form]").forEach(setupTheme);
    $$("form[data-theme-glyph]").forEach(setupThemeGlyph);
    $$("form[data-auth]").forEach(setupAuthForm);
    $$("[data-logout]").forEach(setupLogout);
    $$("[data-library]").forEach(setupLibrary);
    $$("form[data-upload]").forEach(setupUpload);
    $$("[data-clip-page]").forEach(setupClipPage);
    $$("[data-collection-page]").forEach(setupCollectionPage);
    $$("form[data-collection-new]").forEach(setupCollectionNew);
    $$("form[data-token-mint]").forEach(setupTokens);
    $$("[data-defaults]").forEach(setupDefaults);
    $$("form[data-password-form]").forEach(setupPasswordForm);
    $$("[data-logout-others]").forEach(setupLogoutOthers);
    $$("[data-setup]").forEach(setupWizard);
    $$("form[data-discord-link]").forEach(setupDiscordLink);
    $$("[data-discord-unlink]").forEach(setupDiscordUnlink);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot);
  } else {
    boot();
  }
})();
