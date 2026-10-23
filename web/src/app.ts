// The application.
//
// This file owns the things a page must not: the session, the shell, the router, the live
// feed, and the boundary between signed-out and signed-in. Everything else is a module that
// renders one screen.

import { Api, ApiError, connectEvents, type EventStream, type LiveEvent } from "./lib/api";
import { fill } from "./lib/dom";
import { currentLocale, enabledLocales, initI18n, onLocaleChange, refreshLocales, setLocale, tx } from "./lib/i18n";
import { icon } from "./lib/icons";
import { createRouter, type RouteMatch } from "./lib/router";
import { homeFor, loadSession } from "./lib/session";
import { initTheme, toggleTheme, type Theme } from "./lib/theme";
import type { AuthCtx, Crumb, Ctx, PageMeta, Session } from "./lib/types";
import { SEGMENT_LABELS, isCustomerPath, isPublicPath, isStorefrontPath, routes } from "./routes";
import { confirmDialog, formDialog } from "./ui/dialog";
import { createShell, impersonationBanner, openPalette, type Shell } from "./ui/layout";
import { skeletonPage } from "./ui/kit";
import { createToaster } from "./ui/toast";

// The route table, the public-path set and the customer-path predicate live in ./routes so
// that adding a screen is not an edit to this file. This file owns the things a page must
// not: the session, the shell, the router, the live feed, and the boundary between
// signed-out, customer and staff.

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

  // The catalogue has to be in hand before the first screen renders: a view translates while it
  // builds its nodes, so a screen built before the catalogue lands is a screen in English. The
  // call never throws — an installation whose locale endpoint is down is a panel in English,
  // not a panel that fails to start.
  await initI18n(api);

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
    toaster.push(tx("app.session.expired", "Your session expired"), {
      tone: "danger",
      detail: tx("app.session.expired_detail", "Sign in again to continue."),
    });
    router.navigate("/login", { replace: true });
  });

  // A language switch is a re-render, not a reload. The rail, the crumbs and the toolbar are
  // built once, so the shell is rebuilt too — a German menu around an English screen is the
  // half-translated state this feature exists to remove. Public pages have no shell; the
  // storefront subscribes on its own and re-renders in place, which keeps the scroll position
  // and the selected category.
  onLocaleChange(() => {
    const match = router.current();
    if (!session || !match) return;
    shell = null;
    void handleRoute(match);
  });

  /** Switches the language and lets the notification above do the re-rendering. */
  function changeLocale(code: string): void {
    void setLocale(code);
  }

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

    // The storefront is public, and it stays public for every audience — a visitor, a customer
    // and an operator alike. This is checked before the session branch on purpose: `/` is the
    // shop, and an administrator who cannot open their own shop cannot answer "is the price on
    // the site right?". The page reads the identity itself (ctx.viewer) to offer the right
    // door back into the panel; it never requires one.
    if (isStorefrontPath(path)) {
      await renderPublic(path, match);
      return;
    }

    if (!session) {
      // Everything that is not the storefront asks for a session and remembers where you were
      // going.
      if (isPublicPath(path)) {
        await renderPublic(path, match);
        return;
      }
      router.navigate(`/login?next=${encodeURIComponent(path + window.location.search)}`, { replace: true });
      return;
    }

    // The remaining public pages (sign-in, reset, registration) are meaningless once you are
    // signed in, so they send you home. The storefront is not one of them, which is why it was
    // handled above rather than here.
    if (isPublicPath(path)) {
      router.navigate(homeFor(session), { replace: true });
      return;
    }

    if (!match) {
      await renderNotFound(path);
      return;
    }

    // A customer may reach the portal and the account screens they own; everything else is
    // staff-only. This used to be "anything outside /portal redirects to /portal", which is
    // why a customer saw an "Account settings" link in the navigation and was bounced by
    // clicking it. The predicate now lives in ./routes, so the guard and the navigation are
    // built from one list and cannot disagree.
    if (!session.user.is_staff && !isCustomerPath(match.path)) {
      router.navigate(homeFor(session), { replace: true });
      return;
    }

    // An account that owes a second factor is sent to the screen that enrols one, and stays there
    // until it does. `mfaEnrolmentRequired` is set by an operator's MFA reset (the customer lost
    // their phone and their recovery codes), and it is deliberately a redirect rather than a
    // banner: a reset that only *suggested* enrolling would leave the account on password-only
    // access for ever, which is a silent downgrade of the customer's security.
    //
    // The security page itself is excluded, so this cannot loop. `ctx.refreshSession()` there
    // re-reads the identity after a confirmed enrolment, which clears the flag and lets the
    // customer navigate away.
    if (
      !session.user.is_staff &&
      session.mfaEnrolmentRequired &&
      match.path !== "/portal/security"
    ) {
      router.navigate("/portal/security", { replace: true });
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
      // Two different failures arrive here and they deserve different words.
      //
      // A dynamic import that cannot be resolved means the chunk for this screen is not in this
      // build. That is not a crash and a Reload button is a lie: reloading fetches the same
      // build. It is what a screen looks like while it is still being written, and it is also
      // what a real deployment sees after a partial asset upload, so the honest answer is "this
      // screen is not available in this build" with a way back rather than a stack trace.
      //
      // Anything else is a genuine load failure — a network error, a corrupted chunk — and the
      // Reload button is right for it.
      const message = String(err);
      const unresolved = /dynamically imported module|Failed to fetch dynamically|Importing a module script failed|Cannot find module/i.test(message);
      content.replaceChildren(
        unresolved
          ? h(
              "div",
              { class: "empty-state" },
              h("p", { class: "empty-title" }, tx("app.chunk.missing_title", "This screen is not available in this build")),
              h(
                "p",
                { class: "empty-body" },
                tx(
                  "app.chunk.missing_body",
                  "The page exists in the navigation but its module is not part of the bundle that was served. Everything else in the panel still works.",
                ),
              ),
              h(
                "a",
                { class: "btn", href: "/dashboard", "data-link": "true" },
                tx("app.chunk.back", "Back to the dashboard"),
              ),
            )
          : h(
              "div",
              { class: "error-state" },
              h("p", { class: "empty-title" }, tx("app.load.failed_title", "This screen could not be loaded")),
              h("p", { class: "empty-body" }, message),
              h(
                "button",
                { class: "btn", type: "button", onclick: () => window.location.reload() },
                tx("app.reload", "Reload"),
              ),
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
          h("p", { class: "empty-title" }, tx("app.render.failed_title", "Something went wrong rendering this screen")),
          h("p", { class: "empty-body" }, err instanceof Error ? err.message : String(err)),
          h(
            "button",
            {
              class: "btn",
              type: "button",
              onclick: () => router.navigate(window.location.pathname + window.location.search, { replace: true }),
            },
            tx("app.retry", "Try again"),
          ),
        ),
      );
    }
  }

  async function renderPublic(path: string, match: RouteMatch | null): Promise<void> {
    root.replaceChildren(skeletonPage());
    shell = null;
    // `/` is the storefront and the route table says so, so `match` already points at the right
    // module. The fallback exists for one case only: a storefront URL that matched nothing (a
    // truncated product slug) still renders the shop rather than a 404, because the shop is a
    // better answer than an error page for somebody who mistyped a link.
    const storefront = routes.find((r) => r.pattern === "/")!;
    const routeMatch =
      match ?? { route: storefront, params: {}, path, query: new URLSearchParams(window.location.search) };
    const mod = (await routeMatch.route.load()) as { default: (ctx: AuthCtx) => Node | Promise<Node>; meta?: PageMeta };
    document.title = `${mod.meta?.title ?? "Sign in"} · Catalyst Billing`;
    const ctx: AuthCtx = {
      api,
      // The identity, when there is one. A public page must not *require* a session, and must not
      // pretend a signed-in visitor is a stranger either: the storefront's header uses this to
      // offer an operator "Dashboard" instead of "Sign in".
      viewer: session,
      query: new URLSearchParams(window.location.search),
      params: routeMatch.params,
      navigate: (p, o) => router.navigate(p, o),
      onSignedIn: async (next) => {
        session = next;
        api.setCSRFToken(next.csrfToken);
        // The visitor was resolved anonymously a moment ago. Now that there is a session, the
        // account's own stored language is part of the answer, so the catalogue is re-read
        // before the first signed-in screen renders.
        await refreshLocales(api);
        startLive();
        router.navigate(nextQuery() ?? homeFor(next), { replace: true });
      },
      toast: (title, options) => toaster.push(title, options),
    };
    const page = await mod.default(ctx);
    // The storefront renders outside the shell, so it would be the one screen an impersonated
    // operator could work behind. The banner is mounted above it instead.
    root.replaceChildren(
      ...(session?.impersonating
        ? [impersonationBanner(session, () => void stopImpersonation(), { flush: true })]
        : []),
      page,
    );
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
            h("h1", { class: "page-title" }, tx("app.not_found.title", "Page not found")),
            h("p", { class: "page-sub" }, tx("app.not_found.body", "There is nothing at {{path}}.", { path })),
            h("a", { class: "btn btn--primary", href: "/login" }, tx("app.sign_in", "Sign in")),
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
      locales: enabledLocales(),
      locale: currentLocale(),
      navigate: (p) => router.navigate(p),
      onLocaleChange: changeLocale,
      onToggleTheme: () => {
        theme = toggleTheme(theme);
        const btn = document.getElementById("theme-toggle");
        if (btn) fill(btn, icon(theme === "dark" ? "sun" : "moon", 14));
      },
      onLogout: () => void signOut(),
      onStopImpersonation: () => void stopImpersonation(),
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

  /**
   * Ends an impersonation and returns the operator to their own session.
   *
   * Three things have to happen in order, and each one is load-bearing:
   *
   *  1. The server ends the borrowed session and rotates the operator's own token into the
   *     cookie. That rotation is why the local CSRF token must be replaced below — the old one
   *     belongs to a session that no longer exists, and every mutation would fail with
   *     `csrf_failed` until the next reload.
   *  2. The identity is re-read rather than patched locally. `impersonating` is a fact about the
   *     session, not a flag this file owns; asking `/auth/me` is what makes the banner disappear
   *     for the same reason it appeared.
   *  3. The shell is rebuilt from the new session, so the navigation, the role list and the
   *     "whose account is this" question all change together. A page left rendered as the
   *     customer would be a screen the operator can no longer act on.
   *
   * A failure is reported and *not* papered over: if the server refuses, the operator is still
   * impersonating and the banner must stay up saying so.
   */
  async function stopImpersonation(): Promise<void> {
    try {
      await api.post("/api/v1/portal/impersonation/stop", {});
    } catch (err) {
      // `operator_session_ended` means the operator's own session is gone too. The API client's
      // 401 handler has already sent them to the sign-in screen; saying anything else here
      // would be a second, conflicting message.
      if (err instanceof ApiError && err.code === "operator_session_ended") return;
      toaster.push(tx("app.impersonation.stop_failed", "The impersonation was not ended"), {
        tone: "danger",
        detail:
          err instanceof ApiError
            ? err.detail || err.code
            : tx("app.impersonation.stop_failed_detail", "The server could not be reached."),
      });
      return;
    }

    const next = await loadSession(api);
    if (!next) {
      // The rotation landed but the identity read did not: the honest answer is the sign-in
      // screen, not a shell built from a session nobody confirmed.
      session = null;
      shell = null;
      router.navigate("/login", { replace: true });
      return;
    }
    session = next;
    api.setCSRFToken(next.csrfToken);
    shell?.updateSession(next);
    toaster.push(tx("app.impersonation.stopped", "Back in your own account"), {
      tone: "success",
      detail: next.user.email,
    });
    router.navigate(homeFor(next), { replace: true });
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
    // `/` opens the palette, but only inside the shell: on the storefront there is no palette to
    // open, and swallowing the key there would take the browser's own quick-find away for nothing.
    if (!typing && event.key === "/" && session && shell) {
      event.preventDefault();
      openPaletteNow();
    }
  });

  if (session) startLive();
  router.start();
}

// ------------------------------------------------------------------ helpers --

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
        h("h1", { class: "page-title" }, tx("app.fatal.title", "Catalyst Billing is unreachable")),
        h("p", { class: "page-sub" }, err instanceof Error ? err.message : String(err)),
        h(
          "p",
          { class: "auth-note" },
          tx("app.fatal.body", "The API did not answer. Check that the server is running, then reload."),
        ),
        h(
          "button",
          { class: "btn btn--primary", type: "button", onclick: () => window.location.reload() },
          tx("app.reload", "Reload"),
        ),
      ),
    ),
  );
}
