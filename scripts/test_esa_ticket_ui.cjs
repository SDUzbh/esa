#!/usr/bin/env node
"use strict";
const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { resolve } = require("node:path");
const { runInNewContext } = require("node:vm");
const test = require("node:test");
const script = readFileSync(resolve(__dirname, "../plugins/redmine_esa/assets/javascripts/ticket.js"), "utf8");
const flush = () => new Promise(setImmediate);

function fixture() {
  const nodes = new Map(), timers = new Map(), calls = [], replies = [];
  let sequence = 0;
  function element() {
    return { dataset: {}, hidden: true, value: "", innerHTML: "original status", textContent: "",
      listeners: {}, addEventListener(name, fn) { this.listeners[name] = fn; }, dispatchEvent() {}, scrollIntoView() {} };
  }
  for (const id of ["esa-ticket", "esa-live-status", "esa-live-notice", "esa-fill-notice", "esa-undo-fill", "esa-metadata-form", "esa-metadata"]) nodes.set(id, element());
  nodes.get("esa-ticket").dataset = { statusUrl: "/status", suggestionUrl: "/suggestion" };
  const inputs = Object.fromEntries(["resolution_reason", "cause", "solution", "validation", "versions", "environment"].map(name => [name, { ...element(), value: `unsaved ${name}` }]));
  nodes.get("esa-metadata-form").elements = { namedItem: name => inputs[name] };
  const document = { ...element(), readyState: "complete", hidden: false, getElementById: id => nodes.get(id) };
  const window = { ...element(), location: { search: "", href: "http://localhost/esa/tickets/1" }, history: { replaceState() {} } };
  const fetch = async (url, options) => {
    calls.push({ url, options });
    const reply = replies.shift() || { html: "completed status", active: false };
    if (typeof reply === "function") return reply(options);
    if (reply instanceof Error) throw reply;
    return { ok: !reply.denied, status: reply.denied ? 403 : 200, redirected: false, json: async () => reply };
  };
  const context = { document, window, fetch, AbortController, URLSearchParams, URL, Event,
    setTimeout: (fn, ms) => { const id = ++sequence; timers.set(id, { fn, ms }); return id; },
    clearTimeout: id => timers.delete(id) };
  return { nodes, inputs, timers, calls, replies, document, window,
    start: () => runInNewContext(script, context),
    clickFill: () => nodes.get("esa-ticket").listeners.click({ target: { closest: () => ({}) } }),
    tick: ms => { const timer = [...timers].find(([, value]) => value.ms === ms); assert.ok(timer, `missing ${ms}ms timer`); timers.delete(timer[0]); timer[1].fn(); } };
}

test("live updates preserve unsaved metadata; filling is reversible and uses GET only", async () => {
  const f = fixture(); f.replies.push({ html: "investigating", active: true }); f.start(); await flush();
  assert.equal(f.nodes.get("esa-live-status").innerHTML, "investigating");
  assert.equal(f.inputs.cause.value, "unsaved cause");
  f.replies.push({ html: "awaiting review with report link", active: false }); f.tick(3000); await flush();
  assert.equal(f.inputs.cause.value, "unsaved cause");
  assert.equal(f.nodes.get("esa-live-status").innerHTML, "awaiting review with report link");
  f.replies.push({ source: "ESA draft — not verified", values: { cause: "Observed null body", solution: "Retry", validation: "Proposed verification", versions: "do not change" } });
  f.clickFill(); await flush();
  assert.equal(f.inputs.cause.value, "Observed null body");
  assert.equal(f.inputs.versions.value, "unsaved versions");
  assert.match(f.nodes.get("esa-fill-notice").textContent, /Nothing has been saved or forwarded/);
  f.nodes.get("esa-undo-fill").listeners.click();
  assert.equal(f.inputs.cause.value, "unsaved cause");
  assert.ok(f.calls.every(c => !c.options.method && c.options.credentials === "same-origin" && c.options.cache === "no-store"));
});

test("failures back off and a subsequent success restores normal polling", async () => {
  const f = fixture(); f.replies.push(new Error("offline")); f.start(); await flush();
  assert.match(f.nodes.get("esa-live-notice").textContent, /edits are preserved/);
  f.replies.push(new Error("offline")); f.tick(6000); await flush();
  f.tick(12000); await flush();
  assert.ok([...f.timers.values()].some(t => t.ms === 15000));
  assert.equal(f.inputs.solution.value, "unsaved solution");
});

test("hidden pages stop polling; return from back-forward cache resumes it", async () => {
  const f = fixture(); f.start(); await flush();
  f.document.hidden = true; f.document.listeners.visibilitychange();
  assert.equal(f.timers.size, 0);
  f.document.hidden = false; f.document.listeners.visibilitychange(); await flush();
  assert.equal(f.calls.length, 2);
  f.window.listeners.pagehide(); assert.equal(f.timers.size, 0);
  f.window.listeners.pageshow({ persisted: true }); await flush();
  assert.equal(f.calls.length, 3);
});

test("expired visibility permission removes report links and stops requests", async () => {
  const f = fixture(); f.replies.push({ denied: true }); f.start(); await flush();
  assert.match(f.nodes.get("esa-live-status").textContent, /no longer available/);
  assert.equal(f.timers.size, 0);
  assert.equal(f.inputs.cause.value, "unsaved cause");
});

test("overlapping polls and repeated fill clicks do not duplicate requests", async () => {
  const f = fixture(); let release;
  f.replies.push(() => new Promise(resolve => { release = resolve; })); f.start();
  f.document.listeners.visibilitychange(); assert.equal(f.calls.length, 1);
  release({ ok: true, status: 200, redirected: false, json: async () => ({ html: "ready", active: false }) }); await flush();
  f.replies.push(() => new Promise(resolve => { release = resolve; }));
  f.clickFill(); f.clickFill(); assert.equal(f.calls.length, 2);
  release({ ok: true, status: 200, redirected: false, json: async () => ({ values: {} }) }); await flush();
  assert.equal(f.inputs.cause.value, "unsaved cause");
});
