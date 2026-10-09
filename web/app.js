"use strict";

// Display only: the server decides what admins may actually do.
const ADMIN_GROUP = "qdrant-admins";

let isAdmin = false;

const $ = (id) => document.getElementById(id);
const show = (el, on) => { el.hidden = !on; };

const flashTimers = new WeakMap();

// Shows (or clears, when text is empty) a message. With ttlMs, it hides itself
// after that long, unless a newer message replaced it in the meantime.
function flash(el, text, ok, ttlMs) {
  clearTimeout(flashTimers.get(el));
  el.textContent = text;
  el.classList.toggle("msg--ok", !!ok);
  show(el, !!text);
  if (text && ttlMs) {
    flashTimers.set(el, setTimeout(() => flash(el, ""), ttlMs));
  }
}

// Fetch wrapper: a 401 means the session is gone, so go back to the login view.
async function api(path, opts = {}) {
  const res = await fetch(path, { credentials: "same-origin", ...opts });
  if (res.status === 401) {
    showLogin();
    throw new Error("Your session has expired. Please log in again.");
  }
  if (!res.ok) {
    const text = (await res.text()).trim();
    throw new Error(text || `Request failed (${res.status})`);
  }
  return res;
}

function showLogin() {
  show($("loading"), false);
  show($("dashboard"), false);
  show($("session"), false);
  show($("admin-badge"), false);
  show($("admin"), false);
  isAdmin = false;
  show($("login"), true);
}

function showDashboard(user) {
  show($("loading"), false);
  show($("login"), false);
  $("who").textContent = user.name || user.email || user.preferred_username || user.sub;
  isAdmin = (user.groups || []).includes(ADMIN_GROUP);
  show($("admin-badge"), isAdmin);
  show($("admin"), isAdmin);
  show($("session"), true);
  show($("dashboard"), true);
  refreshLists();
  loadQuota();
}

function safeHref(raw) {
  try {
    const u = new URL(raw);
    return u.protocol === "http:" || u.protocol === "https:" ? u.href : null;
  } catch {
    return null;
  }
}

function renderItem(sub, admin = false) {
  const li = document.createElement("li");
  li.className = "item";

  const head = document.createElement("div");
  head.className = "item__head";

  const badge = document.createElement("span");
  badge.className = `badge badge--${sub.type === "event" ? "event" : "content"}`;
  badge.textContent = sub.type;

  const date = document.createElement("span");
  date.className = "item__date";
  const when = new Date(sub.timestamp);
  date.textContent = isNaN(when) ? "" : when.toLocaleString();

  const del = document.createElement("button");
  del.type = "button";
  del.className = "btn btn--danger";
  del.textContent = "Delete";
  del.addEventListener("click", async () => {
    const q = admin
      ? `Delete submission ${sub.id}? This cannot be undone. It will still count toward the owner's monthly limit.`
      : "Delete this submission? It will still count toward this month's limit.";
    if (!confirm(q)) return;
    del.disabled = true;
    try {
      await api(`/api/submissions/${encodeURIComponent(sub.id)}`, { method: "DELETE" });
      await refreshLists();
    } catch (err) {
      del.disabled = false;
      flash($(admin ? "admin-msg" : "list-msg"), err.message);
    }
  });

  head.append(badge, date, del);
  li.append(head);

  if (admin) {
    const meta = document.createElement("div");
    meta.className = "item__meta";
    const who = [sub.owner_username, sub.owner_email && `<${sub.owner_email}>`].filter(Boolean).join(" ");
    meta.textContent = `${who || `owner ${(sub.owner || "unknown").slice(0, 8)}`} · ${sub.id}`;
    li.append(meta);
  }

  const href = sub.url && safeHref(sub.url);
  if (href) {
    const a = document.createElement("a");
    a.className = "item__url";
    a.href = href;
    a.rel = "noopener noreferrer";
    a.target = "_blank";
    a.textContent = href;
    li.append(a);
  }

  const p = document.createElement("p");
  p.className = "item__desc";
  p.textContent = sub.description;
  li.append(p);
  return li;
}

function refreshLists() {
  return Promise.all([loadSubmissions(), isAdmin ? loadAdminSubmissions() : null]);
}

async function loadAdminSubmissions() {
  try {
    const res = await api("/api/admin/submissions");
    const { submissions } = await res.json();
    const list = submissions || [];
    list.sort((a, b) => new Date(b.timestamp) - new Date(a.timestamp));
    $("admin-list").replaceChildren(...list.map((s) => renderItem(s, true)));
    show($("admin-empty"), list.length === 0);
    flash($("admin-msg"), "");
  } catch (err) {
    flash($("admin-msg"), err.message);
  }
}

async function loadSubmissions() {
  try {
    const res = await api("/api/submissions");
    const { submissions } = await res.json();
    const list = submissions || [];
    list.sort((a, b) => new Date(b.timestamp) - new Date(a.timestamp));
    $("list").replaceChildren(...list.map(renderItem));
    show($("empty"), list.length === 0);
    flash($("list-msg"), "");
  } catch (err) {
    flash($("list-msg"), err.message);
  }
}

async function loadQuota() {
  const el = $("quota");
  const radio = document.querySelector('input[name=type][value=content]');
  try {
    const res = await api("/api/quota");
    const { quota } = await res.json();
    el.textContent = quota > 0
      ? `${quota} of 5 content submissions left this month. Events are unlimited. Deleting a submission does not give the slot back.`
      : "You have used all 5 content submissions for this month. You can still submit events.";
    // Out of content slots: steer the form to events instead of failing on submit.
    radio.disabled = quota <= 0;
    if (quota <= 0 && radio.checked) document.querySelector('input[name=type][value=event]').checked = true;
    syncSuggestButton();
  } catch {
    /* keep the static hint text */
  }
}

// AI suggestions only make sense for content pieces, not events.
function syncSuggestButton() {
  $("suggest").hidden = new FormData($("form")).get("type") !== "content";
}

function hideSuggestions() {
  show($("suggestions"), false);
}

async function getSuggestions() {
  const msg = $("form-msg");
  flash(msg, "");
  hideSuggestions();
  const description = $("description").value.trim();
  const url = $("url").value.trim();
  if (!url || !safeHref(url)) return flash(msg, "AI suggestions need a valid http(s) link.");
  if (!description) return flash(msg, "AI suggestions need a description.");

  const btn = $("suggest");
  const label = btn.textContent;
  btn.disabled = true;
  btn.textContent = "Reviewing… this can take a minute";
  try {
    const res = await api("api/suggestions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ url, description }),
      signal: AbortSignal.timeout(200000),
    });
    const out = await res.json();
    const items = out.suggestions || [];
    $("suggestions-list").replaceChildren(...items.map((text) => {
      const li = document.createElement("li");
      li.textContent = text;
      return li;
    }));
    $("suggestions-summary").textContent = items.length === 0 && !out.could_be_improved
      ? "No suggestions: this looks good to go."
      : "Ideas to improve it before you submit:";
    const left = res.headers.get("X-RateLimit-Remaining");
    $("suggestions-left").textContent = left === null ? "" : `${left} AI review${left === "1" ? "" : "s"} left today.`;
    show($("suggestions"), true);
  } catch (err) {
    flash(msg, err.name === "TimeoutError" ? "The AI review took too long. Please try again." : err.message);
  } finally {
    btn.disabled = false;
    btn.textContent = label;
  }
}

$("suggest").addEventListener("click", getSuggestions);
$("clear-suggestions").addEventListener("click", () => {
  hideSuggestions();
  $("suggest").focus();
});
$("form").addEventListener("reset", () => {
  hideSuggestions();
  setTimeout(syncSuggestButton); // the reset event fires before the values are restored
});
for (const radio of document.querySelectorAll("input[name=type]")) {
  radio.addEventListener("change", () => {
    syncSuggestButton();
    hideSuggestions();
  });
}

$("description").addEventListener("input", (e) => {
  $("count").textContent = `${e.target.value.length} / 5000`;
});

$("form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const msg = $("form-msg");
  flash(msg, "");

  const description = $("description").value.trim();
  const url = $("url").value.trim();
  if (!description) return flash(msg, "Please add a description.");
  if (url && !safeHref(url)) return flash(msg, "The link must be a valid http(s) URL.");

  const body = {
    type: new FormData(e.target).get("type"),
    description,
  };
  if (url) body.url = url;

  const btn = $("submit");
  btn.disabled = true;
  try {
    await api("/api/submissions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    e.target.reset();
    $("count").textContent = "0 / 5000";
    flash(msg, "Submitted. Thank you!", true, 15000);
    await Promise.all([refreshLists(), loadQuota()]);
  } catch (err) {
    flash(msg, err.message);
    loadQuota();
  } finally {
    btn.disabled = false;
  }
});

$("logout").addEventListener("click", async () => {
  try {
    await fetch("/auth/logout", { method: "POST", credentials: "same-origin" });
  } finally {
    location.reload();
  }
});

(async function init() {
  try {
    const res = await fetch("/api/me", { credentials: "same-origin" });
    if (res.status === 401) return showLogin();
    if (!res.ok) throw new Error(`Request failed (${res.status})`);
    showDashboard(await res.json());
  } catch (err) {
    $("loading").textContent = `Could not load the app: ${err.message}`;
  }
})();
