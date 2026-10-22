// The shell: rail, bar, command palette.
//
// The rail is built from a table of destinations filtered by permission. Hiding a
// destination the user cannot reach is a courtesy, not a security control — the server
// refuses the request regardless — but showing a link that always 403s is a bug report
// waiting to happen.
//
// The palette is the fastest path to any record: it searches the navigation and, for staff
// who may read customers, the customer list itself. Typing an email and pressing Enter is
// the single most common thing an operator does.

import { ApiError, type Page } from "../lib/api";
import { h, debounce, fill, on, swap } from "../lib/dom";
import { initials } from "../lib/format";
import { brandMark, icon, type IconName } from "../lib/icons";
import type { Crumb, Ctx, Session } from "../lib/types";
import { avatar, badge, btn, emptyState, iconBtn, spinner } from "./kit";

export interface NavItem {
  label: string;
  href: string;
  icon: IconName;
  permission?: string;
  /** Matches a sub-path without highlighting the parent item. */
  exact?: boolean;
}

export interface NavGroup {
  label: string;
  items: NavItem[];
}

export function navGroups(can: (p: string) => boolean, isStaff: boolean): NavGroup[] {
  const groups: NavGroup[] = [];
  if (isStaff) {
    groups.push({
      label: "Overview",
      items: [
        { label: "Dashboard", href: "/", icon: "home", permission: "report:read", exact: true },
        { label: "Announcements", href: "/announcements", icon: "radio", permission: "announcement:write" },
      ],
    });
    groups.push({
      label: "Revenue",
      items: [
        { label: "Customers", href: "/customers", icon: "users", permission: "customer:read:all" },
        { label: "Invoices", href: "/invoices", icon: "invoice", permission: "invoice:read:all" },
        { label: "Orders", href: "/orders", icon: "bag", permission: "invoice:read:all" },
        { label: "Payments", href: "/payments", icon: "card", permission: "payment:reconcile" },
        { label: "Coupons", href: "/coupons", icon: "tag", permission: "coupon:read:all" },
        { label: "Credit", href: "/credits", icon: "wallet", permission: "credit:write" },
      ],
    });
    groups.push({
      label: "Operations",
      items: [
        { label: "Services", href: "/services", icon: "server", permission: "service:read:all" },
        { label: "Cancellations", href: "/cancellations", icon: "logout", permission: "service:read:all" },
        { label: "Queue", href: "/operations", icon: "activity", permission: "service:read:all" },
        { label: "Webhooks", href: "/webhooks", icon: "radio", permission: "payment:reconcile" },
      ],
    });
    groups.push({
      label: "Support",
      items: [{ label: "Tickets", href: "/support", icon: "message", permission: "ticket:read:all" }],
    });
    groups.push({
      label: "Configuration",
      items: [
        { label: "Catalog", href: "/catalog-management", icon: "box", permission: "invoice:read:all" },
        { label: "Config options", href: "/config-options", icon: "sliders", permission: "config:write" },
        { label: "Tax rates", href: "/tax-rates", icon: "percent", permission: "tax:write" },
        { label: "Currencies", href: "/currencies", icon: "globe", permission: "currency:write" },
        { label: "System settings", href: "/settings/system", icon: "settings", permission: "settings:write" },
      ],
    });
    groups.push({
      label: "Trust",
      items: [
        { label: "Approvals", href: "/approvals", icon: "checkCircle", permission: "audit:read" },
        { label: "Audit log", href: "/audit", icon: "shield", permission: "audit:read" },
        { label: "Logs", href: "/logs", icon: "terminal", permission: "log:read" },
        { label: "API keys", href: "/api-keys", icon: "key", permission: "apikey:manage" },
      ],
    });
  } else {
    groups.push({
      label: "Your account",
      items: [
        { label: "Overview", href: "/portal", icon: "home", exact: true },
        { label: "Invoices", href: "/portal/invoices", icon: "invoice" },
        { label: "Services", href: "/portal/services", icon: "server" },
        { label: "Credit", href: "/portal/credits", icon: "wallet" },
        { label: "Tickets", href: "/tickets", icon: "message" },
        { label: "Billing details", href: "/portal/profile", icon: "building" },
      ],
    });
  }
  // "Account" is a real section for both audiences now. It used to be added for every
  // session, which meant a customer saw a Settings link that the route guard bounced back
  // to /portal. Customers get their own security and notification screens, and staff keep
  // the personal settings page.
  groups.push({
    label: "Account",
    items: isStaff
      ? [{ label: "Settings", href: "/settings", icon: "sliders" }]
      : [
          { label: "Security", href: "/portal/security", icon: "shield" },
          { label: "Notifications", href: "/portal/notifications", icon: "radio" },
          { label: "Payment methods", href: "/portal/payment-methods", icon: "card" },
        ],
  });

  return groups
    .map((group) => ({ ...group, items: group.items.filter((item) => !item.permission || can(item.permission)) }))
    .filter((group) => group.items.length > 0);
}

export interface ShellOptions {
  session: Session;
  theme: "dark" | "light";
  navigate: (path: string) => void;
  onToggleTheme: () => void;
  onLogout: () => void;
  onOpenPalette: () => void;
}

export interface Shell {
  el: HTMLElement;
  content: HTMLElement;
  setActive(path: string): void;
  setCrumbs(crumbs: Crumb[]): void;
  setLive(state: "live" | "offline"): void;
  closeNav(): void;
  updateSession(session: Session): void;
}

function isActive(item: NavItem, path: string): boolean {
  if (item.exact) return path === item.href;
  if (item.href === "/") return path === "/";
  return path === item.href || path.startsWith(`${item.href}/`);
}

export function createShell(options: ShellOptions): Shell {
  const nav = h("nav", { class: "nav", "aria-label": "Primary" });
  const crumbsHost = h("div", { class: "crumb" });
  const liveHost = h("span", { class: "live", title: "Live updates" }, h("span", { class: "live-dot" }), h("span", null, "Live"));
  const userHost = h("div");
  const themeBtn = iconBtn(
    options.theme === "dark" ? "sun" : "moon",
    options.theme === "dark" ? "Switch to light theme" : "Switch to dark theme",
    options.onToggleTheme,
  );
  // A stable handle: app.ts swaps the glyph after a toggle without rebuilding the shell.
  themeBtn.id = "theme-toggle";

  const app = h("div", { class: "app" });
  const scrim = h("div", { class: "scrim", onclick: () => app.classList.remove("nav-open") });
  const content = h("main", { class: "content", id: "main", tabindex: -1 });

  let navItems: NavItem[] = [];

  const renderNav = (session: Session) => {
    const groups = navGroups((p) => session.permissions.has(p), session.user.is_staff);
    navItems = groups.flatMap((g) => g.items);
    fill(
      nav,
      ...groups.map((group) =>
        h(
          "div",
          { class: "nav-group" },
          h("div", { class: "nav-label" }, group.label),
          ...group.items.map((item) =>
            h(
              "a",
              {
                class: "nav-item",
                href: item.href,
                dataset: { href: item.href },
                onclick: () => app.classList.remove("nav-open"),
              },
              icon(item.icon, 16),
              h("span", null, item.label),
            ),
          ),
        ),
      ),
    );
    setActive(window.location.pathname);
  };

  const renderUser = (session: Session) => {
    const user = session.user;
    const name = user.display_name || user.email;
    const menu = h("div", { class: "menu", hidden: true });
    const roles = user.roles.length > 0 ? user.roles.join(", ") : "Customer";

    fill(
      menu,
      h("div", { class: "menu-label" }, user.email),
      h("div", { class: "menu-label" }, roles),
      h("div", { class: "menu-sep" }),
      h("a", { class: "menu-item", href: "/settings" }, icon("sliders", 15), "Account settings"),
      session.permissions.has("apikey:manage")
        ? h("a", { class: "menu-item", href: "/api-keys" }, icon("key", 15), "API keys")
        : null,
      session.user.is_staff
        ? h("a", { class: "menu-item", href: "/portal" }, icon("globe", 15), "Customer portal")
        : null,
      h("div", { class: "menu-sep" }),
      h(
        "button",
        { class: "menu-item menu-item--danger", type: "button", onclick: options.onLogout },
        icon("logout", 15),
        "Sign out",
      ),
    );

    const trigger = h(
      "button",
      {
        class: "who",
        type: "button",
        "aria-haspopup": "menu",
        "aria-expanded": "false",
        title: `${name} — ${roles}`,
        onclick: (event: MouseEvent) => {
          event.stopPropagation();
          const open = menu.hidden;
          menu.hidden = !open;
          trigger.setAttribute("aria-expanded", open ? "true" : "false");
        },
      },
      avatar(name, { accent: user.is_staff }),
      h("span", { class: "who-name" }, h("strong", null, name), h("span", null, roles)),
    );

    const close = () => {
      menu.hidden = true;
      trigger.setAttribute("aria-expanded", "false");
    };

    fill(
      userHost,
      h(
        "div",
        { class: "sidebar-foot" },
        h("div", { class: "popover-host" }, trigger),
        menu,
      ),
    );
    // The menu is absolutely positioned relative to the footer.
    menu.classList.add("popover");
    menu.classList.add("user-menu");
  };

  const sidebar = h(
    "aside",
    { class: "sidebar", "aria-label": "Sidebar" },
    h(
      "div",
      { class: "sidebar-head" },
      h(
        "a",
        { class: "brand", href: options.session.user.is_staff ? "/" : "/portal" },
        h("span", { class: "brand-mark" }, brandMark(17)),
        h("span", { class: "brand-name" }, "Catalyst", h("small", null, "Billing")),
      ),
    ),
    nav,
    userHost,
  );

  const topbar = h(
    "header",
    { class: "topbar" },
    h(
      "button",
      {
        class: "btn btn--ghost btn--icon sidebar-toggle",
        type: "button",
        "aria-label": "Open navigation",
        onclick: () => app.classList.toggle("nav-open"),
      },
      icon("menu", 16),
    ),
    crumbsHost,
    h(
      "div",
      { class: "topbar-actions" },
      liveHost,
      h(
        "button",
        { class: "search-trigger", type: "button", onclick: options.onOpenPalette, title: "Search (Ctrl/⌘ K)" },
        icon("search", 14),
        h("span", null, "Search"),
        h("kbd", { class: "kbd" }, "⌘K"),
      ),
      themeBtn,
    ),
  );

  // One document listener, not one per render: the user menu is rebuilt whenever the
  // session changes, and a listener added inside that render would accumulate.
  on(document, "click", (event) => {
    if ((event.target as Element)?.closest?.(".sidebar-foot")) return;
    for (const menu of document.querySelectorAll<HTMLElement>(".user-menu")) menu.hidden = true;
  });
  on(document, "keydown", (event) => {
    if ((event as KeyboardEvent).key !== "Escape") return;
    for (const menu of document.querySelectorAll<HTMLElement>(".user-menu")) menu.hidden = true;
  });

  const main = h("div", { class: "main" }, topbar, content);
  fill(app, sidebar, scrim, main);
  renderNav(options.session);
  renderUser(options.session);
  setCrumbs([{ label: "Dashboard" }]);

  function setActive(path: string) {
    for (const el of nav.querySelectorAll<HTMLAnchorElement>(".nav-item")) {
      const item = navItems.find((i) => i.href === el.dataset.href);
      if (!item) continue;
      el.classList.toggle("is-active", isActive(item, path));
      if (isActive(item, path)) el.setAttribute("aria-current", "page");
      else el.removeAttribute("aria-current");
    }
  }

  function setCrumbs(crumbs: Crumb[]) {
    const parts: Node[] = [];
    crumbs.forEach((crumb, i) => {
      if (i > 0) parts.push(h("span", { class: "crumb-sep" }, "/"));
      const last = i === crumbs.length - 1;
      if (crumb.href && !last) parts.push(h("a", { href: crumb.href }, crumb.label));
      else parts.push(h("span", { class: last ? "crumb-current" : "" }, crumb.label));
    });
    fill(crumbsHost, ...parts);
  }

  function setLive(state: "live" | "offline") {
    liveHost.classList.toggle("is-live", state === "live");
    const label = liveHost.lastElementChild;
    if (label) label.textContent = state === "live" ? "Live" : "Reconnecting";
    liveHost.title = state === "live" ? "Live updates connected" : "Live updates reconnecting";
  }

  return {
    el: app,
    content,
    setActive,
    setCrumbs,
    setLive,
    closeNav: () => app.classList.remove("nav-open"),
    updateSession(session) {
      renderNav(session);
      renderUser(session);
      setActive(window.location.pathname);
    },
  };
}

// ---------------------------------------------------------------- palette --

export interface PaletteOptions {
  ctx: Ctx;
  onClose: () => void;
}

interface Command {
  label: string;
  hint?: string;
  icon: IconName;
  href: string;
  group: string;
}

/** The command palette. Returns a handle; the caller mounts and opens it. */
export function openPalette(options: PaletteOptions): { close(): void } {
  const { ctx } = options;
  const commands: Command[] = [];
  for (const group of navGroups((p) => ctx.can(p), ctx.session.user.is_staff)) {
    for (const item of group.items) {
      commands.push({ label: item.label, icon: item.icon, href: item.href, group: group.label });
    }
  }
  if (ctx.can("invoice:write:all")) {
    commands.push({ label: "New invoice", icon: "plus", href: "/invoices/new", group: "Actions" });
  }

  const list = h("div", { class: "palette-list" });
  const input = h("input", {
    type: "text",
    placeholder: "Search pages, customers, invoices…",
    autocomplete: "off",
    spellcheck: "false",
    "aria-label": "Search",
  });
  const palette = h(
    "div",
    { class: "palette", role: "dialog", "aria-modal": "true", "aria-label": "Command palette" },
    h("div", { class: "palette-input" }, icon("search", 16), input),
    list,
    h(
      "div",
      { class: "palette-foot" },
      h("span", { class: "row" }, h("kbd", { class: "kbd" }, "↑"), h("kbd", { class: "kbd" }, "↓"), "to move"),
      h("span", { class: "row" }, h("kbd", { class: "kbd" }, "↵"), "to open"),
      h("span", { class: "row" }, h("kbd", { class: "kbd" }, "esc"), "to close"),
      h("span", { class: "spacer" }),
      h("span", null, "Catalyst"),
    ),
  );
  const overlay = h("div", { class: "overlay overlay--top", onclick: (event: MouseEvent) => {
    if (event.target === overlay) close();
  } }, palette);

  let items: Array<{ href: string; label: string; sub?: string; icon: IconName }> = [];
  let cursor = 0;
  let searchGeneration = 0;

  const renderList = () => {
    if (items.length === 0) {
      swap(
        list,
        emptyState({
          title: "Nothing found",
          body: "Try a page name, or a customer's email address.",
          icon: "search",
        }),
      );
      return;
    }
    fill(
      list,
      ...items.map((item, i) =>
        h(
          "button",
          {
            class: `palette-item${i === cursor ? " is-active" : ""}`,
            type: "button",
            onclick: () => go(item.href),
            onmousemove: () => {
              if (cursor !== i) {
                cursor = i;
                renderList();
              }
            },
          },
          icon(item.icon, 15),
          h("span", { class: "truncate" }, item.label),
          item.sub ? h("span", { class: "palette-hint truncate" }, item.sub) : null,
        ),
      ),
    );
    list.querySelector(".palette-item.is-active")?.scrollIntoView({ block: "nearest" });
  };

  const localMatches = (q: string) => {
    const needle = q.trim().toLowerCase();
    const matches = needle
      ? commands.filter((c) => c.label.toLowerCase().includes(needle) || c.group.toLowerCase().includes(needle))
      : commands;
    items = matches.slice(0, 8).map((c) => ({ href: c.href, label: c.label, sub: c.group, icon: c.icon }));
    cursor = 0;
    renderList();
  };

  const searchCustomers = debounce(async (q: string) => {
    if (q.trim().length < 2 || !ctx.can("customer:read:all")) return;
    const generation = ++searchGeneration;
    try {
      const page = await ctx.api.get<Page<{ id: string; email: string; name: string }>>("/api/v1/staff/customers", {
        query: { q, limit: 5 },
        cache: false,
      });
      if (generation !== searchGeneration) return;
      if (page.data.length === 0) return;
      const head = items.slice(0, Math.min(3, items.length));
      items = [
        ...head,
        ...page.data.map((c) => ({
          href: `/customers/${c.id}`,
          label: c.name || c.email,
          sub: c.name ? c.email : "Customer",
          icon: "user" as IconName,
        })),
      ];
      renderList();
    } catch (err) {
      if (err instanceof ApiError) return;
    }
  }, 180);

  const go = (href: string) => {
    close();
    ctx.navigate(href);
  };

  const close = () => {
    overlay.remove();
    document.removeEventListener("keydown", onKey, true);
    options.onClose();
  };

  const onKey = (event: KeyboardEvent) => {
    if (event.key === "Escape") {
      event.preventDefault();
      close();
      return;
    }
    if (event.key === "ArrowDown") {
      event.preventDefault();
      cursor = Math.min(items.length - 1, cursor + 1);
      renderList();
      return;
    }
    if (event.key === "ArrowUp") {
      event.preventDefault();
      cursor = Math.max(0, cursor - 1);
      renderList();
      return;
    }
    if (event.key === "Enter") {
      event.preventDefault();
      const item = items[cursor];
      if (item) go(item.href);
    }
  };

  input.addEventListener("input", () => {
    const value = input.value;
    localMatches(value);
    void searchCustomers(value);
  });

  document.addEventListener("keydown", onKey, true);
  document.body.appendChild(overlay);
  input.focus();
  localMatches("");

  return { close };
}
