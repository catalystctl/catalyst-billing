// The push service worker.
//
// Served at /sw.js from the embedded frontend, at the origin root, because a service worker's
// scope is the directory it is served from: /assets/sw.js could only control /assets. The file is
// built by web/build.mjs from this source and written *unhashed*, unlike the application bundle,
// because the registration URL has to be stable — a hashed name would mean re-registering a
// different worker on every deploy and leaving the previous one installed.
//
// # What it does, and what it deliberately does not
//
// It shows the notification and, when one is clicked, focuses an existing panel tab or opens a
// new one at the notification's path. That is all. It does not cache, it does not intercept
// fetches, it has no offline fallback and it holds no credentials: a worker with a fetch handler
// becomes a second, invisible layer between the browser and the API, and everything that is
// surprising about this panel would then be explainable by "the worker did it". The one thing a
// push notification needs is the ability to be shown and clicked.
//
// # Why the payload is parsed defensively
//
// The payload is JSON written by the server (notify.PushMessage), so in practice it parses. But a
// worker that throws on a malformed push shows nothing at all, and Chrome penalises a
// subscription whose pushes are consistently not shown by revoking it. So a payload that is not
// JSON falls back to being shown as text, and a payload with no title gets a generic one.
//
// # Types
//
// `self` in a service worker is a ServiceWorkerGlobalScope, whose `push` and `notificationclick`
// events are declared in the `webworker` lib — which cannot be loaded beside `dom` without
// duplicating every global. So the scope is described by the small interface below: exactly the
// surface this file uses, which is also the surface a reader has to keep in mind.

/** The payload the server sends. Field names match notify.PushMessage. */
interface PushPayload {
  title?: string;
  body?: string;
  url?: string;
  tag?: string;
  event_key?: string;
}

interface ExtendableEventLike {
  waitUntil(promise: Promise<unknown>): void;
}

interface PushMessageDataLike {
  json(): unknown;
  text(): string;
}

interface PushEventLike extends ExtendableEventLike {
  data: PushMessageDataLike | null;
}

interface NotificationLike {
  close(): void;
  data?: unknown;
}

interface NotificationClickEventLike extends ExtendableEventLike {
  notification: NotificationLike;
  action?: string;
}

interface WindowClientLike {
  url: string;
  focus(): Promise<WindowClientLike>;
  navigate?(url: string): Promise<WindowClientLike>;
}

interface ServiceWorkerScopeLike {
  registration: {
    showNotification(title: string, options?: NotificationOptions): Promise<void>;
  };
  clients: {
    matchAll(options: { type: string; includeUncontrolled: boolean }): Promise<WindowClientLike[]>;
    openWindow(url: string): Promise<WindowClientLike | null>;
    claim?(): Promise<void>;
  };
  skipWaiting?(): Promise<void>;
  addEventListener(type: "install" | "activate", handler: (event: ExtendableEventLike) => void): void;
  addEventListener(type: "push", handler: (event: PushEventLike) => void): void;
  addEventListener(type: "notificationclick", handler: (event: NotificationClickEventLike) => void): void;
}

const scope = self as unknown as ServiceWorkerScopeLike;

const FALLBACK_TITLE = "Catalyst Billing";

/** readPayload turns a push body into a notification, without trusting it. */
function readPayload(event: PushEventLike): PushPayload {
  if (!event.data) return {};
  try {
    const parsed: unknown = event.data.json();
    if (parsed && typeof parsed === "object") return parsed as PushPayload;
  } catch {
    // Not JSON. Fall through to the text form rather than showing nothing: a push that is
    // received and not displayed is a push the push service will eventually stop sending.
  }
  try {
    return { body: event.data.text() };
  } catch {
    return {};
  }
}

/** safePath keeps a notification's link inside the panel. */
function safePath(url: string | undefined): string {
  // The server already refuses to send an absolute URL here (a notification that can send a
  // customer off-site is a phishing vector wearing our name), and this is the second check: the
  // payload is data crossing a process boundary, and the cost of checking twice is one line.
  if (typeof url === "string" && url.startsWith("/") && !url.startsWith("//")) return url;
  return "/portal/notifications";
}

// Take over as soon as the first worker is installed and activated. The page registers this
// worker when the customer asks to enable notifications, so waiting for every tab to close before
// it can show anything would mean the very first test notification does nothing.
scope.addEventListener("install", (event) => {
  event.waitUntil(Promise.resolve(scope.skipWaiting?.()));
});

scope.addEventListener("activate", (event) => {
  event.waitUntil(Promise.resolve(scope.clients.claim?.()));
});

scope.addEventListener("push", (event) => {
  const payload = readPayload(event);
  const title = (payload.title || "").trim() || FALLBACK_TITLE;
  const options: NotificationOptions = {
    body: (payload.body || "").trim(),
    // A tag makes a second notification with the same tag replace the first rather than stack.
    // The server sends one per event, so ten queued invoice reminders do not become ten lines.
    tag: payload.tag || undefined,
    // `data` is what the click handler reads. It survives the notification being shown by a
    // different worker instance than the one that received the push.
    data: { url: safePath(payload.url), event_key: payload.event_key || "" },
    // The mark, so the notification is recognisable in a crowded notification centre. It is
    // served from the same origin, so no external host learns that this customer was notified.
    icon: "/assets/mark.svg",
    badge: "/assets/mark.svg",
  };
  event.waitUntil(scope.registration.showNotification(title, options));
});

scope.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const data = event.notification.data as { url?: string } | undefined;
  const target = safePath(data?.url);

  event.waitUntil(
    (async () => {
      // Focus a tab that already has the panel open, and navigate it to the record the
      // notification is about. Opening a second tab for a customer who already has the panel in
      // front of them is the behaviour that makes people turn notifications off.
      const windows = await scope.clients.matchAll({ type: "window", includeUncontrolled: true });
      for (const client of windows) {
        const url = new URL(client.url);
        if (url.origin !== new URL(target, client.url).origin) continue;
        await client.focus();
        if (client.navigate) await client.navigate(target);
        return;
      }
      await scope.clients.openWindow(target);
    })(),
  );
});
