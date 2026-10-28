// The machine's checks, what `middleware doctor` prints: the binary, the
// units, the loopback socket, the helper, the root, the reserved ports,
// docker, fail2ban, Node, wireguard and the disk (GET /system/doctor).
// Run on demand: a few of them ask systemd and dial sockets.

import { icon } from "../lib/icons.js";
import { h, clear } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api } from "../lib/api.js";
import { toastError } from "../components/toast.js";
import { checklist } from "../components/checklist.js";
import { cardHead } from "./settings-kit.js";

export function doctorCard() {
  const el = h("div", { class: "card sys-card" });
  const runBtn = h("button", { type: "button", class: "btn" }, icon("check"), t("doctor.run"));
  const out = h("div");
  el.append(cardHead(t("doctor.title"), t("doctor.hint")), out, h("div", { class: "form-actions sys-foot" }, runBtn));
  runBtn.addEventListener("click", async () => {
    runBtn.classList.add("busy");
    try {
      const r = await api.get("/system/doctor");
      clear(out).append(
        h("p", null, h("span", { class: "badge " + (r.ok ? "ok" : "bad") }, t(r.ok ? "doctor.allOk" : "doctor.someFail"))),
        checklist((r.checks || []).map((c) => ({ ok: c.ok, text: c.name + ": " + c.detail }))).el,
      );
    } catch (e) { toastError(e); } finally { runBtn.classList.remove("busy"); }
  });
  return { el };
}
