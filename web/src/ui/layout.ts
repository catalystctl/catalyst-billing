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
import { languageSwitcher, tx, type LocaleOption } from "../lib/i18n";
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

import { hasRoute } from "../routes";

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
        // Affiliates sits under Revenue rather than Growth because the screen is a payout report:
        // an operator opens it to reconcile what is owed, not to watch a channel perform.
        { label: "Affiliates", href: "/affiliates", icon: "percent", permission: "report:read" },
      ],
    });
    groups.push({
      label: "People",
      items: [
        { label: "Users", href: "/users", icon: "users", permission: "staff:manage" },
        { label: "Roles", href: "/roles", icon: "shield", permission: "staff:manage" },
        { label: "Security", href: "/security", icon: "lock", permission: "staff:manage" },
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
      items: [
        { label: "Tickets", href: "/support", icon: "message", permission: "ticket:read:all" },
        { label: "Ticket fields", href: "/ticket-fields", icon: "sliders", permission: "ticket:read:all" },
      ],
    });
    groups.push({
      label: "Configuration",
      items: [
        { label: "Catalog", href: "/catalog-management", icon: "box", permission: "invoice:read:all" },
        { label: "Upgrades", href: "/upgrades", icon: "trendUp", permission: "catalog:write" },
        { label: "Config options", href: "/config-options", icon: "sliders", permission: "config:write" },
        { label: "Tax rates", href: "/tax-rates", icon: "percent", permission: "tax:write" },
        { label: "Currencies", href: "/currencies", icon: "globe", permission: "currency:write" },
        { label: "System settings", href: "/settings/system", icon: "settings", permission: "settings:write" },
      ],
    });
    // Integrations and platform. Grouped separately because these are the screens that carry
    // provider credentials or replace the running binary, so they are not "configuration" in
    // the catalogue sense.
    groups.push({
      label: "Integrations",
      items: [
        { label: "Gateways", href: "/gateways", icon: "card", permission: "gateway:write" },
        { label: "Servers", href: "/servers", icon: "layers", permission: "server:write" },
        { label: "Marketplace", href: "/marketplace", icon: "box", permission: "extension:manage" },
      ],
    });
    groups.push({
      label: "Platform",
      items: [
        { label: "Imports", href: "/imports", icon: "database", permission: "import:write" },
        { label: "Locales", href: "/locales", icon: "globe", permission: "i18n:write" },
        { label: "OAuth clients", href: "/oauth-clients", icon: "key", permission: "oauth:manage" },
        { label: "Updates", href: "/settings/updates", icon: "sparkles", permission: "update:manage" },
        { label: "Service properties", href: "/service-properties", icon: "tag", permission: "service:write:all" },
        { label: "Holds", href: "/holds", icon: "pauseCircle", permission: "service:read:all" },
        { label: "Notification templates", href: "/notification-templates", icon: "mail", permission: "notification:write" },
        { label: "Automation", href: "/automation", icon: "refresh", permission: "settings:write" },
        { label: "Sign-in providers", href: "/providers", icon: "key", permission: "oauth:manage" },
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
    // The labels are translated here, at the point the table is finalised, rather than at each
    // entry: the navigation stays a plain list of destinations — the thing this file is read
    // for — and the key is derived from the English label (`shell.nav.system_settings`) instead
    // of being a second identifier to keep in step. The one cost is that renaming a label
    // renames its key, which is visible in the diff and cheap to re-translate.
    .map((group) => ({
      ...group,
      label: shellLabel("group", group.label),
      items: group.items.map((item) => ({ ...item, label: shellLabel("nav", item.label) })),
    }))
    // Two filters, both of which must hold. The permission check hides an action the server would
    // refuse; the route check hides a link the router cannot serve. The second one is what makes
    // "a link in the panel is never a bug" true: the navigation and the route table are generated
    // from one list, so an entry for an undeclared route is not rendered rather than leading to a
    // not-found page. It also means a screen appears in the menu the moment its route lands,
    // without a second edit that someone will forget.
    .map((group) => ({
      ...group,
      items: group.items.filter((item) => (!item.permission || can(item.permission)) && hasRoute(item.href)),
    }))
    .filter((group) => group.items.length > 0);
}

/** `"System settings"` → `tx("shell.nav.system_settings", "System settings")`. */
function shellLabel(scope: "nav" | "group", label: string): string {
  const key = label
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "_")
    .replace(/^_+|_+$/g, "");
  return tx(`shell.${scope}.${key}`, label);
}

export interface ShellOptions {
  session: Session;
  theme: "dark" | "light";
  /** The languages this installation offers. Empty or single-entry means no switcher at all. */
  locales: LocaleOption[];
  /** The locale the shell is currently rendering in. */
  locale: string;
  navigate: (path: string) => void;
  onToggleTheme: () => void;
  onLogout: () => void;
  onOpenPalette: () => void;
  /** Called with the chosen code. The app persists it and rebuilds the shell and the current
   *  screen in the new language. */
  onLocaleChange: (code: string) => void;
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
  const nav = h("nav", { class: "nav", "aria-label": tx("shell.nav.primary", "Primary") });
  const crumbsHost = h("div", { class: "crumb" });
  const liveHost = h(
    "span",
    { class: "live", title: tx("shell.live.title", "Live updates") },
    h("span", { class: "live-dot" }),
    h("span", null, tx("shell.live.on", "Live")),
  );
  const userHost = h("div");
  const themeBtn = iconBtn(
    options.theme === "dark" ? "sun" : "moon",
    options.theme === "dark"
      ? tx("shell.theme.to_light", "Switch to light theme")
      : tx("shell.theme.to_dark", "Switch to dark theme"),
    options.onToggleTheme,
  );
  // Null when the installation offers one language — the common case — so the toolbar has no
  // control that cannot do anything.
  const localeSwitch = languageSwitcher({
    locales: options.locales,
    selected: options.locale,
    onSelect: options.onLocaleChange,
    id: "locale-switch",
  });
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
    const roles = user.roles.length > 0 ? user.roles.join(", ") : tx("shell.role.customer", "Customer");

    fill(
      menu,
      h("div", { class: "menu-label" }, user.email),
      h("div", { class: "menu-label" }, roles),
      h("div", { class: "menu-sep" }),
      h("a", { class: "menu-item", href: "/settings" }, icon("sliders", 15), tx("shell.menu.settings", "Account settings")),
      session.permissions.has("apikey:manage")
        ? h("a", { class: "menu-item", href: "/api-keys" }, icon("key", 15), tx("shell.menu.api_keys", "API keys"))
        : null,
      session.user.is_staff
        ? h("a", { class: "menu-item", href: "/portal" }, icon("globe", 15), tx("shell.menu.portal", "Customer portal"))
        : null,
      h("div", { class: "menu-sep" }),
      // The second factor, in the place people look for it. It is a link rather than a label so
      // the answer comes with the way to change it: the account screens are where enrolment
      // lives, and an operator who reads "off" here should not have to guess where to go.
      h(
        "a",
        {
          class: "menu-item menu-item--mfa",
          href: user.is_staff ? "/settings" : "/portal/security",
          title: session.mfa?.has_mfa
            ? tx("shell.mfa.on_title", "A code is required at every sign-in")
            : tx("shell.mfa.off_title", "Your password alone gets into this account"),
        },
        icon(session.mfa?.has_mfa ? "shieldCheck" : "shield", 15),
        h(
          "span",
          { class: "menu-item-label" },
          tx("shell.mfa.label", "Two-factor"),
          " · ",
          h(
            "span",
            { class: session.mfa?.has_mfa ? "menu-mfa-on" : "menu-mfa-off" },
            session.mfa?.has_mfa ? tx("shell.mfa.on", "on") : tx("shell.mfa.off", "off"),
          ),
        ),
      ),
      h("div", { class: "menu-sep" }),
      h(
        "button",
        { class: "menu-item menu-item--danger", type: "button", onclick: options.onLogout },
        icon("logout", 15),
        tx("shell.menu.sign_out", "Sign out"),
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
    { class: "sidebar", "aria-label": tx("shell.sidebar.label", "Sidebar") },
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
        "aria-label": tx("shell.nav.open", "Open navigation"),
        onclick: () => app.classList.toggle("nav-open"),
      },
      icon("menu", 16),
    ),
    crumbsHost,
    h(
      "div",
      { class: "topbar-actions" },
      localeSwitch,
      liveHost,
      h(
        "button",
        {
          class: "search-trigger",
          type: "button",
          onclick: options.onOpenPalette,
          title: tx("shell.search.title", "Search (Ctrl/⌘ K)"),
        },
        icon("search", 14),
        h("span", null, tx("shell.search.label", "Search")),
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

  // The privileged-account prompt. It lives in the shell rather than on the settings page
  // because "your role can change every customer's account and your login is a password" is a
  // fact an operator should meet on the way to doing something else, not only if they go looking.
  // It is dismissible for the tab, not forever: a warning that can be silenced permanently is
  // one that gets silenced in the first week and never seen again.
  const noticeHost = h("div", { class: "shell-notice" });
  let dismissed = false;
  const renderNotice = (session: Session) => {
    const privileged = session.user.is_staff;
    const enabled = session.mfa?.has_mfa === true;
    if (!privileged || enabled || dismissed) {
      noticeHost.replaceChildren();
      return;
    }
    noticeHost.replaceChildren(
      h(
        "div",
        { class: "mfa-prompt", role: "status" },
        icon("shield", 15),
        h(
          "span",
          { class: "mfa-prompt-text" },
          h("strong", null, tx("shell.mfa.prompt_title", "Your account has no second factor.")),
          " ",
          tx(
            "shell.mfa.prompt_body",
            "Your role can change every customer's account, so a stolen password is not one customer's problem.",
          ),
        ),
        h(
          "a",
          { class: "btn btn--sm btn--primary", href: "/settings" },
          tx("shell.mfa.prompt_action", "Set up two-factor"),
        ),
        h(
          "button",
          {
            class: "btn btn--sm btn--ghost",
            type: "button",
            "aria-label": tx("shell.mfa.prompt_dismiss", "Dismiss for now"),
            onclick: () => {
              dismissed = true;
              noticeHost.replaceChildren();
            },
          },
          tx("shell.mfa.prompt_dismiss_label", "Not now"),
        ),
      ),
    );
  };

  const main = h("div", { class: "main" }, topbar, noticeHost, content);
  fill(app, sidebar, scrim, main);
  renderNav(options.session);
  renderUser(options.session);
  renderNotice(options.session);
  setCrumbs([{ label: tx("shell.crumb.dashboard", "Dashboard") }]);

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
    if (label) {
      label.textContent =
        state === "live" ? tx("shell.live.on", "Live") : tx("shell.live.reconnecting", "Reconnecting");
    }
    liveHost.title =
      state === "live"
        ? tx("shell.live.connected", "Live updates connected")
        : tx("shell.live.reconnecting_title", "Live updates reconnecting");
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
      renderNotice(session);
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
    commands.push({ label: tx("shell.palette.new_invoice", "New invoice"), icon: "plus", href: "/invoices/new", group: tx("shell.palette.group.actions", "Actions") });
  }

  const list = h("div", { class: "palette-list" });
  const input = h("input", {
    type: "text",
    placeholder: tx("shell.palette.placeholder", "Search pages, customers, invoices…"),
    autocomplete: "off",
    spellcheck: "false",
    "aria-label": tx("shell.palette.label", "Search"),
  });
  const palette = h(
    "div",
    { class: "palette", role: "dialog", "aria-modal": "true", "aria-label": tx("shell.palette.dialog", "Command palette") },
    h("div", { class: "palette-input" }, icon("search", 16), input),
    list,
    h(
      "div",
      { class: "palette-foot" },
      h("span", { class: "row" }, h("kbd", { class: "kbd" }, "↑"), h("kbd", { class: "kbd" }, "↓"), tx("shell.palette.move", "to move")),
      h("span", { class: "row" }, h("kbd", { class: "kbd" }, "↵"), tx("shell.palette.open", "to open")),
      h("span", { class: "row" }, h("kbd", { class: "kbd" }, "esc"), tx("shell.palette.close", "to close")),
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
          title: tx("shell.palette.empty_title", "Nothing found"),
          body: tx("shell.palette.empty_body", "Try a page name, or a customer's email address."),
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
          sub: c.name ? c.email : tx("shell.role.customer", "Customer"),
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
