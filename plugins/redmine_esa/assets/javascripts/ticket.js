(() => {
  "use strict";

  function start() {
    const root = document.getElementById("esa-ticket");
    if (!root || root.dataset.connected) return;
    root.dataset.connected = "true";
    const panel = document.getElementById("esa-live-status");
    const liveNotice = document.getElementById("esa-live-notice");
    const fillNotice = document.getElementById("esa-fill-notice");
    const undo = document.getElementById("esa-undo-fill");
    const form = document.getElementById("esa-metadata-form");
    const fields = ["resolution_reason", "cause", "solution", "validation"];
    let timer, request, polling = false, stopped = false, failures = 0;
    let filling = false, previous = null, lastHTML = null;

    async function readJSON(url, signal) {
      const response = await fetch(url, {
        credentials: "same-origin", cache: "no-store", signal,
        headers: { Accept: "application/json" }
      });
      if (response.redirected || [401, 403, 404].includes(response.status)) {
        stopped = true;
        panel.textContent = "This page is no longer available. Sign in or reopen the issue.";
        throw new Error("Access unavailable");
      }
      if (!response.ok) throw new Error("Request failed");
      return response.json();
    }

    async function poll() {
      if (stopped || document.hidden || polling) return;
      polling = true;
      request = new AbortController();
      const timeout = setTimeout(() => request.abort(), 25000);
      let delay = 3000;
      try {
        const data = await readJSON(root.dataset.statusUrl, request.signal);
        if (typeof data.html !== "string") throw new Error("Invalid status response");
        // Only the server-rendered status fragment changes. Forms stay untouched.
        if (lastHTML !== data.html) { panel.innerHTML = data.html; lastHTML = data.html; }
        liveNotice.textContent = "Status updates automatically.";
        failures = 0;
        delay = data.active ? 3000 : 15000;
      } catch (error) {
        if (!document.hidden) liveNotice.textContent = stopped
          ? "Live updates stopped. Reopen this issue after signing in."
          : "Live update unavailable. Retrying automatically; your form edits are preserved.";
        failures += 1;
        delay = Math.min(30000, 3000 * Math.pow(2, failures));
      } finally {
        clearTimeout(timeout);
        request = null;
        polling = false;
        if (!stopped && !document.hidden) timer = setTimeout(poll, delay);
      }
    }

    async function fill() {
      if (!form || filling || stopped) return;
      filling = true;
      fillNotice.textContent = "Loading ESA suggestions…";
      try {
        const data = await readJSON(root.dataset.suggestionUrl);
        const values = data.values || {};
        const available = fields.filter(name => typeof values[name] === "string" && values[name].trim());
        if (!available.length) {
          fillNotice.textContent = "No current ESA suggestion is available. Open the report for details.";
          return;
        }
        previous = {};
        available.forEach(name => {
          const input = form.elements.namedItem(name);
          if (!input) return;
          previous[name] = input.value;
          input.value = values[name];
          input.dispatchEvent(new Event("input", { bubbles: true }));
        });
        undo.hidden = false;
        fillNotice.textContent = `${data.source}. Suggestions filled into the form. Review, then save. Nothing has been saved or forwarded.`;
        document.getElementById("esa-metadata").scrollIntoView({ block: "start" });
      } catch (error) {
        fillNotice.textContent = "Could not load ESA suggestions. Your metadata has not changed.";
      } finally {
        filling = false;
      }
    }

    root.addEventListener("click", event => {
      if (event.target.closest("[data-esa-fill]")) fill();
    });
    if (undo) undo.addEventListener("click", () => {
      Object.entries(previous || {}).forEach(([name, value]) => {
        const input = form.elements.namedItem(name);
        input.value = value;
        input.dispatchEvent(new Event("input", { bubbles: true }));
      });
      previous = null;
      undo.hidden = true;
      fillNotice.textContent = "Fill undone. Nothing has been saved or forwarded.";
    });
    document.addEventListener("visibilitychange", () => {
      clearTimeout(timer);
      if (document.hidden) { if (request) request.abort(); }
      else poll();
    });
    window.addEventListener("pagehide", () => {
      stopped = true;
      clearTimeout(timer);
      if (request) request.abort();
    });
    window.addEventListener("pageshow", event => {
      if (event.persisted) { stopped = false; poll(); }
    });
    // A report-page link is an explicit request to fill this local form.
    if (new URLSearchParams(window.location.search).get("fill") === "esa") {
      const url = new URL(window.location.href);
      url.searchParams.delete("fill");
      window.history.replaceState(null, "", url);
      fill();
    }
    poll();
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start);
  else start();
})();
