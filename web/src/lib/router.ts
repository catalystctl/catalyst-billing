// History-based routing.
//
// Paths, not hashes: a billing link is pasted into a ticket and opened by someone else, and
// `/invoices/abc` is a URL a proxy, a log and a bookmark all handle correctly. The server
// already serves the shell for an unknown non-API path, so a reload works.

export interface RouteMatch {
  route: Route;
  params: Record<string, string>;
  path: string;
  query: URLSearchParams;
}

export interface Route {
  pattern: string;
  /** Segments with a leading `:` capture into params. */
  segments: string[];
  load: () => Promise<unknown>;
}

export function route(pattern: string, load: () => Promise<unknown>): Route {
  return { pattern, segments: split(pattern), load };
}

function split(path: string): string[] {
  return path.split("/").filter(Boolean);
}

export function matchRoute(routes: Route[], pathname: string, search: string): RouteMatch | null {
  const parts = split(pathname);
  for (const r of routes) {
    if (r.segments.length !== parts.length) continue;
    const params: Record<string, string> = {};
    let ok = true;
    for (let i = 0; i < r.segments.length; i++) {
      const seg = r.segments[i] ?? "";
      const part = parts[i] ?? "";
      if (seg.startsWith(":")) {
        params[seg.slice(1)] = decodeURIComponent(part);
      } else if (seg !== part) {
        ok = false;
        break;
      }
    }
    if (!ok) continue;
    return { route: r, params, path: pathname, query: new URLSearchParams(search) };
  }
  return null;
}

export interface RouterOptions {
  routes: Route[];
  /** Renders the not-found screen. */
  notFound: (path: string) => void;
  /** Called for every navigation, after the URL has changed. */
  onChange: (match: RouteMatch | null) => void;
}

export interface Router {
  start(): void;
  navigate(path: string, options?: { replace?: boolean }): void;
  replaceQuery(patch: Record<string, string | null>): URLSearchParams;
  current(): RouteMatch | null;
  dispose(): void;
}

export function createRouter(options: RouterOptions): Router {
  let current: RouteMatch | null = null;
  let disposed = false;

  const resolve = (): RouteMatch | null =>
    matchRoute(options.routes, window.location.pathname, window.location.search);

  const render = () => {
    if (disposed) return;
    const match = resolve();
    current = match;
    if (!match) {
      options.notFound(window.location.pathname);
      options.onChange(null);
      return;
    }
    options.onChange(match);
  };

  const navigate = (path: string, opts?: { replace?: boolean }) => {
    if (path === window.location.pathname + window.location.search) return;
    if (opts?.replace) window.history.replaceState({}, "", path);
    else window.history.pushState({}, "", path);
    render();
  };

  const replaceQuery = (patch: Record<string, string | null>): URLSearchParams => {
    const url = new URL(window.location.href);
    for (const [key, value] of Object.entries(patch)) {
      if (value === null || value === "") url.searchParams.delete(key);
      else url.searchParams.set(key, value);
    }
    window.history.replaceState({}, "", url.pathname + url.search + url.hash);
    const next = new URLSearchParams(url.search);
    if (current) current = { ...current, query: next };
    return next;
  };

  // One delegated listener for every internal link. A page writes `<a href="/customers">`
  // and gets client-side navigation without wiring anything.
  const onClick = (event: MouseEvent) => {
    if (event.defaultPrevented || event.button !== 0) return;
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    const target = event.target as Element | null;
    const anchor = target?.closest?.("a[href]") as HTMLAnchorElement | null;
    if (!anchor) return;
    if (anchor.target && anchor.target !== "_self") return;
    if (anchor.hasAttribute("download")) return;
    if (anchor.dataset.native === "true") return;
    const href = anchor.getAttribute("href") ?? "";
    if (!href.startsWith("/") || href.startsWith("//")) return;
    const url = new URL(href, window.location.origin);
    if (url.origin !== window.location.origin) return;
    event.preventDefault();
    navigate(url.pathname + url.search + url.hash);
  };

  const onPop = () => render();

  return {
    start() {
      document.addEventListener("click", onClick);
      window.addEventListener("popstate", onPop);
      render();
    },
    navigate,
    replaceQuery,
    current: () => current,
    dispose() {
      disposed = true;
      document.removeEventListener("click", onClick);
      window.removeEventListener("popstate", onPop);
    },
  };
}
