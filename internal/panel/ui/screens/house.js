// The house's own conversation: an agent working on the installation
// itself (its settings, its tasks, new sites), with every existing site
// denied to it. The same chat as a site's, pointed at /chat/middleware.

import { t } from "../lib/i18n.js";
import { page } from "../components/page.js";
import { chatTab } from "./site-chat.js";

export function houseScreen() {
  const p = page({ title: t("nav.house"), sub: t("house.sub"), icon: "sparkles" });
  const chat = chatTab("middleware");
  p.body.append(chat.el);
  return { el: p.el, destroy() { chat.destroy(); } };
}
