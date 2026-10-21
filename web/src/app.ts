// The application.
//
// This file owns the things a page must not: the session, the shell, the router, the live
// feed, and the boundary between signed-out and signed-in. Everything else is a module that
// renders one screen.

import { Api, connectEvents, type EventStream, type LiveEvent } from "./lib/api";
import { fill } from "./lib/dom";
import { icon } from "./lib/icons";
import { createRouter, route, type Route, type RouteMatch } from "./lib/router";
import { homeFor, loadSession } from "./lib/session";
import { initTheme, toggleTheme, type Theme } from "./lib/theme";
import type { AuthCtx, Crumb, Ctx, PageMeta, Session } from "./lib/types";
import { confirmDialog, formDialog } from "./ui/dialog";
import { createShell, openPalette, type Shell } from "./ui/layout";
import { skeletonPage } from "./ui/kit";
import { createToaster } from "./ui/toast";

const PUBLIC_PATHS = new Set(["/login", "/forgot-password", "/reset-password", "/verify"]);

const routes: Route[] = [
  route("/", () => import("./views/dashboard")),
  route("/login", () => import("./views/login")),
  route("/forgot-password", () => import("./views/forgot-password")),
  route("/reset-password", () => import("./views/reset-password")),
  route("/verify", () => import("./views/verify")),

  route("/customers", () => import("./views/customers")),
  route("/customers/:id", () => import("./views/customer")),

  route("/invoices", () => import("./views/invoices")),
  route("/invoices/new", () => import("./views/invoice-new")),
  route("/invoices/:id", () => import("./views/invoice")),

  route("/services", () => import("./views/services")),
  route("/services/:id", () => import("./views/service")),

  route("/orders", () => import("./views/orders")),
  route("/orders/:id", () => import("./views/order")),

  route("/payments", () => import("./views/payments")),
  route("/payments/:id", () => import("./views/payment")),

  route("/operations", () => import("./views/operations")),
  route("/operations/:id", () => import("./views/operation")),
  route("/webhooks", () => import("./views/webhooks")),

  route("/catalog", () => import("./views/catalog")),
  route("/catalog/:id", () => import("./views/product")),

  route("/approvals", () => import("./views/approvals")),
  route("/audit", () => import("./views/audit")),
  route("/api-keys", () => import("./views/api-keys")),
  route("/settings", () => import("./views/settings")),

  route("/portal", () => import("./views/portal")),
  route("/portal/invoices", () => import("./views/portal-invoices")),
  route("/portal/invoices/:id", () => import("./views/portal-invoice")),
  route("/portal/services", () => import("./views/portal-services")),
  route("/portal/services/:id", () => import("./views/portal-service")),
  route("/portal/profile", () => import("./views/portal-profile")),
];

const SEGMENT_LABELS: Record<string, string> = {
  customers: "Customers",
  invoices: "Invoices",
  services: "Services",
  orders: "Orders",
  payments: "Payments",
  operations: "Queue",
  webhooks: "Webhooks",
  catalog: "Catalog",
  approvals: "Approvals",
  audit: "Audit log",
  "api-keys": "API keys",
  settings: "Settings",
  portal: "Portal",
  profile: "Billing details",
  new: "New",
  login: "Sign in",
  "forgot-password": "Reset password",
  "reset-password": "Choose a password",
  verify: "Verify email",
};

export async function bootstrap(root: HTMLElement): Promise<void> {
  let theme: Theme = initTheme();
  const api = new Api();
  const toaster = createToaster();

  let session: Session | null = null;
  let shell: Shell | null = null;
  let live: EventStream | null = null;
  let disposers: Array<() => void> = [];
  let routeToken = 0;
  let palette: { close(): void } | null = null;

  try {
    session = await loadSession(api);
  } catch (err) {
    renderFatal(root, err);
    return;
  }

  const router = createRouter({
    routes,
    notFound: (path) => void renderNotFound(path),
    onChange: (match) => void handleRoute(match),
  });

  api.setUnauthorizedHandler(() => {
    if (!session) return;
    session = null;
    live?.close();
    live = null;
    disposePage();
    toaster.push("Your session expired", { tone: "danger", detail: "Sign in again to continue." });
    router.navigate("/login", { replace: true });
  });

  // ------------------------------------------------------------- navigation --

  const disposePage = () => {
    for (const fn of disposers.splice(0)) {
      try {
        fn();
      } catch {
        /* a cleanup must not break navigation */
      }
    }
  };

  async function handleRoute(match: RouteMatch | null): Promise<void> {
    const path = window.location.pathname;
    const token = ++routeToken;
    disposePage();

    if (!session) {
      if (isPublic(path)) {
        await renderPublic(path, match);
        return;
      }
      router.navigate(`/login?next=${encodeURIComponent(path + window.location.search)}`, { replace: true });
      return;
    }

    if (isPublic(path)) {
      router.navigate(homeFor(session), { replace: true });
      return;
    }

    if (!match) {
      await renderNotFound(path);
      return;
    }

    if (match.path !== "/portal" && !match.path.startsWith("/portal/") && !session.user.is_staff) {
      router.navigate("/portal", { replace: true });
      return;
    }

    const content = ensureShell().content;
    content.replaceChildren(skeletonPage());
    window.scrollTo({ top: 0 });

    let mod;
    try {
      mod = (await match.route.load()) as { default: (ctx: Ctx) => Node | Promise<Node>; meta?: PageMeta };
    } catch (err) {
      if (token !== routeToken) return;
      content.replaceChildren(
        h(
          "div",
          { class: "error-state" },
          h("p", { class: "empty-title" }, "This screen could not be loaded"),
          h("p", { class: "empty-body" }, String(err)),
          h("button", { class: "btn", type: "button", onclick: () => window.location.reload() }, "Reload"),
        ),
      );
      return;
    }
    if (token !== routeToken) return;

    document.title = `${mod.meta?.title ?? "Catalyst"} · Catalyst Billing`;
    shell?.setActive(match.path);
    shell?.setCrumbs(defaultCrumbs(match, mod.meta));

    const ctx = makeCtx(match, mod.meta);
    try {
      const node = await mod.default(ctx);
      if (token !== routeToken) return;
      content.replaceChildren(node);
    } catch (err) {
      if (token !== routeToken) return;
      if (err instanceof DOMException && err.name === "AbortError") return;
      content.replaceChildren(
        h(
          "div",
          { class: "error-state" },
          h("p", { class: "empty-title" }, "Something went wrong rendering this screen"),
          h("p", { class: "empty-body" }, err instanceof Error ? err.message : String(err)),
          h(
            "button",
            { class: "btn", type: "button", onclick: () => router.navigate(window.location.pathname + window.location.search, { replace: true }) },
            "Try again",
          ),
        ),
      );
    }
  }

  async function renderPublic(path: string, match: RouteMatch | null): Promise<void> {
    root.replaceChildren(skeletonPage());
    shell = null;
    const routeMatch = match ?? { route: routes[0]!, params: {}, path, query: new URLSearchParams(window.location.search) };
    const mod = (await routeMatch.route.load()) as { default: (ctx: AuthCtx) => Node | Promise<Node>; meta?: PageMeta };
    document.title = `${mod.meta?.title ?? "Sign in"} · Catalyst Billing`;
    const ctx: AuthCtx = {
      api,
      query: new URLSearchParams(window.location.search),
      navigate: (p, o) => router.navigate(p, o),
      onSignedIn: async (next) => {
        session = next;
        api.setCSRFToken(next.csrfToken);
        startLive();
        router.navigate(nextQuery() ?? homeFor(next), { replace: true });
      },
      toast: (title, options) => toaster.push(title, options),
    };
    root.replaceChildren(await mod.default(ctx));
  }

  function nextQuery(): string | null {
    const next = new URLSearchParams(window.location.search).get("next");
    if (!next) return null;
    // Only an internal path is a safe redirect target: an absolute URL here would be an
    // open redirect, which is a phishing primitive with the panel's own domain on it.
    if (!next.startsWith("/") || next.startsWith("//")) return null;
    return next;
  }

  async function renderNotFound(path: string): Promise<void> {
    disposePage();
    const mod = (await import("./views/not-found")) as { default: (ctx: Ctx) => Node };
    if (!session) {
      root.replaceChildren(
        h(
          "div",
          { class: "auth-main" },
          h(
            "div",
            { class: "auth-panel" },
            h("h1", { class: "page-title" }, "Page not found"),
            h("p", { class: "page-sub" }, `There is nothing at ${path}.`),
            h("a", { class: "btn btn--primary", href: "/login" }, "Sign in"),
          ),
        ),
      );
      return;
    }
    const content = ensureShell().content;
    content.replaceChildren(mod.default(makeCtx({ route: routes[0]!, params: {}, path, query: new URLSearchParams() }, { title: "Not found" })));
  }

  // ---------------------------------------------------------------- the shell --

  function ensureShell(): Shell {
    if (shell) {
      if (root.firstElementChild !== shell.el) root.replaceChildren(shell.el);
      return shell;
    }
    shell = createShell({
      session: session!,
      theme,
      navigate: (p) => router.navigate(p),
      onToggleTheme: () => {
        theme = toggleTheme(theme);
        const btn = document.getElementById("theme-toggle");
        if (btn) fill(btn, icon(theme === "dark" ? "sun" : "moon", 14));
      },
      onLogout: () => void signOut(),
      onOpenPalette: () => openPaletteNow(),
    });
    root.replaceChildren(shell.el);
    return shell;
  }

  function openPaletteNow(): void {
    if (palette || !session || !shell) return;
    const ctx = makeCtx({ route: routes[0]!, params: {}, path: window.location.pathname, query: new URLSearchParams() }, undefined);
    palette = openPalette({ ctx, onClose: () => (palette = null) });
  }

  async function signOut(): Promise<void> {
    try {
      await api.post("/api/v1/auth/logout", {});
    } catch {
      // Clearing the local state is the point: a failed revoke must not leave the browser
      // looking signed in.
    }
    session = null;
    live?.close();
    live = null;
    shell = null;
    disposePage();
    router.navigate("/login", { replace: true });
  }

  function startLive(): void {
    if (live || !session) return;
    live = connectEvents({
      onStatus: (state) => shell?.setLive(state),
      onEvent: (event: LiveEvent) => {
        api.invalidate();
        window.dispatchEvent(new CustomEvent<LiveEvent>("catalyst:live", { detail: event }));
      },
    });
  }

  // ---------------------------------------------------------------- the ctx --

  function makeCtx(match: RouteMatch, meta?: PageMeta): Ctx {
    return {
      api,
      session: session!,
      route: { path: match.path, params: match.params, query: match.query },
      can: (permission) => session!.permissions.has(permission),
      navigate: (path, options) => router.navigate(path, options),
      replaceQuery: (patch) => router.replaceQuery(patch),
      toast: (title, options) => toaster.push(title, options),
      confirm: confirmDialog,
      formDialog,
      setTitle: (title) => {
        document.title = `${title} · Catalyst Billing`;
      },
      setCrumbs: (crumbs: Crumb[]) => shell?.setCrumbs(crumbs),
      refreshSession: async () => {
        const next = await loadSession(api);
        if (!next) return;
        session = next;
        shell?.updateSession(next);
      },
      onDispose: (fn) => disposers.push(fn),
      onLive: (fn) => {
        const handler = (event: Event) => fn((event as CustomEvent<LiveEvent>).detail);
        window.addEventListener("catalyst:live", handler);
        disposers.push(() => window.removeEventListener("catalyst:live", handler));
      },
    };
  }

  // Global shortcuts. Ctrl/⌘+K is the palette; "/" is the same thing for people who live in
  // a terminal, but only when the focus is not already in a field.
  document.addEventListener("keydown", (event) => {
    const target = event.target as HTMLElement | null;
    const typing =
      target?.tagName === "INPUT" || target?.tagName === "TEXTAREA" || target?.tagName === "SELECT" || target?.isContentEditable;
    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
      event.preventDefault();
      openPaletteNow();
      return;
    }
    if (!typing && event.key === "/" && session) {
      event.preventDefault();
      openPaletteNow();
    }
  });

  if (session) startLive();
  router.start();
}

// ------------------------------------------------------------------ helpers --

function isPublic(path: string): boolean {
  return PUBLIC_PATHS.has(path);
}

function titleForSegment(segment: string): string {
  if (SEGMENT_LABELS[segment]) return SEGMENT_LABELS[segment]!;
  if (/^[0-9a-f]{8}-[0-9a-f]{4}-/i.test(segment)) return "Detail";
  return segment.charAt(0).toUpperCase() + segment.slice(1).replace(/-/g, " ");
}

function defaultCrumbs(match: RouteMatch, meta?: PageMeta): Crumb[] {
  const segments = match.path.split("/").filter(Boolean);
  if (segments.length === 0) return [{ label: meta?.crumb ?? "Dashboard" }];
  const crumbs: Crumb[] = [];
  let acc = "";
  segments.forEach((segment, i) => {
    acc += `/${segment}`;
    const last = i === segments.length - 1;
    crumbs.push({
      label: last ? (meta?.crumb ?? titleForSegment(segment)) : titleForSegment(segment),
      href: last ? undefined : acc,
    });
  });
  return crumbs;
}

function h<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  props: Record<string, unknown> | null,
  ...children: Array<Node | string | null | undefined>
): HTMLElementTagNameMap[K] {
  const el = document.createElement(tag);
  if (props) {
    for (const [key, value] of Object.entries(props)) {
      if (value === undefined || value === null || value === false) continue;
      if (key === "class") el.setAttribute("class", String(value));
      else if (key.startsWith("on") && typeof value === "function") {
        el.addEventListener(key.slice(2).toLowerCase(), value as EventListener);
      } else el.setAttribute(key, String(value));
    }
  }
  for (const child of children) {
    if (child === null || child === undefined) continue;
    el.append(typeof child === "string" ? document.createTextNode(child) : child);
  }
  return el;
}

function renderFatal(root: HTMLElement, err: unknown): void {
  root.replaceChildren(
    h(
      "div",
      { class: "auth-main" },
      h(
        "div",
        { class: "auth-panel" },
        h("h1", { class: "page-title" }, "Catalyst Billing is unreachable"),
        h("p", { class: "page-sub" }, err instanceof Error ? err.message : String(err)),
        h("p", { class: "auth-note" }, "The API did not answer. Check that the server is running, then reload."),
        h("button", { class: "btn btn--primary", type: "button", onclick: () => window.location.reload() }, "Reload"),
      ),
    ),
  );
}
