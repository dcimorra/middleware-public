// Status is words plus colour, never colour alone: a red dot means
// nothing to a colour-blind operator or a greyscale print, and "down" is
// as short as the dot is.

import { h } from "../lib/dom.js";
import { t } from "../lib/i18n.js";

const MODE_CLASS = { run: "accent", static: "info", upstream: "info", hold: "warn", stopped: "", disabled: "", unmanaged: "warn" };

const MODE_LABEL = {
  run: "mode.run", static: "mode.static", upstream: "mode.upstream", hold: "mode.hold",
  stopped: "mode.stopped", disabled: "mode.stopped", unmanaged: "mode.unmanaged",
};

export function siteMode(site) {
  if (site.managed === false) return "unmanaged";
  const m = site.mode || "";
  if (m === "disabled") return "stopped";
  return m || "unmanaged";
}

export function modeBadge(site) {
  const mode = siteMode(site);
  return h("span", { class: "badge " + (MODE_CLASS[mode] || "") }, t(MODE_LABEL[mode] || "mode.unknown"));
}

export function aliveStatus(site) {
  const mode = siteMode(site);
  if (site.handing) return h("span", { class: "status" }, h("span", { class: "dot warn" }), t("status.handing"));
  if (mode === "run") {
    if (site.alive) return h("span", { class: "status" }, h("span", { class: "dot ok" }), t("status.up"));
    if (site.error) return h("span", { class: "status", title: site.error }, h("span", { class: "dot bad" }), t("status.failing"));
    return h("span", { class: "status" }, h("span", { class: "dot bad" }), t("status.down"));
  }
  if (mode === "static" || mode === "upstream") return h("span", { class: "status" }, h("span", { class: "dot ok" }), t("status.serving"));
  if (mode === "hold") return h("span", { class: "status" }, h("span", { class: "dot warn" }), t("status.held"));
  if (mode === "stopped") return h("span", { class: "status" }, h("span", { class: "dot" }), t("status.stopped"));
  return h("span", { class: "status" }, h("span", { class: "dot" }), t("status.notServed"));
}

export function levelBadge(level) {
  if (level === "error") return h("span", { class: "badge bad" }, t("level.error"));
  if (level === "warn") return h("span", { class: "badge warn" }, t("level.warn"));
  return h("span", { class: "badge" }, t("level.info"));
}

export function boolBadge(v, yes, no) {
  return v ? h("span", { class: "badge ok" }, yes) : h("span", { class: "badge" }, no);
}
