// The panel's service worker. It is what makes the console installable,
// and it is where web push will land (ROADMAP §6): a notification shown
// when one arrives, and a click that opens or focuses the panel.
//
// It caches nothing, on purpose. The panel has no build step and serves
// its files with ETags; a worker answering from a cache would keep an old
// app.js after an upgrade, which is the bug the ETags exist to prevent.
// Every request goes to the network as if no worker were here.

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (event) => event.waitUntil(self.clients.claim()));

self.addEventListener("push", (event) => {
  let data = {};
  try { data = event.data ? event.data.json() : {}; } catch { data = { title: event.data ? event.data.text() : "" }; }
  const title = data.title || "middleware";
  event.waitUntil(self.registration.showNotification(title, {
    body: data.body || "",
    tag: data.tag || undefined,
    data: { url: data.url || "/#/events" },
    icon: "/assets/icons/icon-192.png",
  }));
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const url = (event.notification.data && event.notification.data.url) || "/";
  event.waitUntil((async () => {
    const all = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
    for (const c of all) {
      if (new URL(c.url).origin === self.location.origin) {
        await c.focus();
        if ("navigate" in c) await c.navigate(url);
        return;
      }
    }
    await self.clients.openWindow(url);
  })());
});
