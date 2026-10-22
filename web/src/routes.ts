// The route table, as data.
//
// Every screen is one entry. It is a separate module from app.ts for one reason: a new
// screen is a line here, not an edit inside the shell's control flow. The shell owns the
// session, the router and the live feed, and it was the worst possible file to merge in.
//
// The lazy `load` is what keeps the bundle small: each view is a dynamic import, so the
// initial download is the shell and the screen the visitor asked for, not all forty.
//
// New screens append to `routes`; the array is grouped by area so an append lands at the end
// of a block rather than in the middle of another area's entries.

import { route, type Route } from "./lib/router";

/** A path a signed-out visitor may load.
 *
 *  `/` is deliberately included: it is the storefront for a visitor and the dashboard for
 *  staff, and which one you get is decided by the session, not by the URL. A storefront
 *  behind a login is not a storefront. */
export const PUBLIC_PATHS = new Set(["/", "/login", "/register", "/forgot-password", "/reset-password", "/verify", "/products"]);

/** True when `path` is public, including the storefront's subtrees.
 *
 *  Written as a prefix test rather than a second set so `/products/anything` cannot fall
 *  through to the signed-in branch and bounce a visitor to a login screen. */
export function isPublicPath(path: string): boolean {
  if (PUBLIC_PATHS.has(path)) return true;
  return path === "/products" || path.startsWith("/products/");
}

/** Prefixes a signed-in *customer* may reach. Everything else is staff-only.
 *
 *  This list is the reason a customer no longer sees an "Account settings" link that bounces
 *  them: the route guard consults it, and the navigation is built from the same predicate. */
export const CUSTOMER_PREFIXES: ReadonlyArray<string> = ["/portal", "/account", "/cart", "/checkout", "/tickets"];

/** True when a signed-in customer may reach `path`. */
export function isCustomerPath(path: string): boolean {
  for (const prefix of CUSTOMER_PREFIXES) {
    if (path === prefix || path.startsWith(`${prefix}/`)) return true;
  }
  return false;
}

export const routes: Route[] = [
  route("/", () => import("./views/dashboard")),
  route("/login", () => import("./views/login")),
  route("/forgot-password", () => import("./views/forgot-password")),
  route("/reset-password", () => import("./views/reset-password")),
  route("/verify", () => import("./views/verify")),

  // Public storefront. /products/:slug is a product page; / is the index (see above).
  route("/products", () => import("./views/storefront")),
  route("/products/:slug", () => import("./views/storefront-product")),

  // /catalog and /catalog/:id were the read-only catalog screens. They are gone, but the URL
  // was linked from services, orders and the old nav, so it redirects instead of 404ing.
  route("/catalog", () => import("./views/catalog-admin")),
  route("/catalog/:id", () => import("./views/catalog-admin-product")),
  route("/catalog-management", () => import("./views/catalog-admin")),
  route("/catalog-management/:id", () => import("./views/catalog-admin-product")),

  route("/customers", () => import("./views/customers")),
  route("/customers/:id", () => import("./views/customer")),
  route("/customers/:id/portal", () => import("./views/customer-portal-view")),

  route("/invoices", () => import("./views/invoices")),
  route("/invoices/new", () => import("./views/invoice-new")),
  route("/invoices/:id", () => import("./views/invoice")),

  route("/services", () => import("./views/services")),
  route("/services/:id", () => import("./views/service")),

  route("/orders", () => import("./views/orders")),
  route("/orders/new", () => import("./views/order-new")),
  route("/orders/:id", () => import("./views/order")),

  route("/payments", () => import("./views/payments")),
  route("/payments/:id", () => import("./views/payment")),

  route("/operations", () => import("./views/operations")),
  route("/operations/:id", () => import("./views/operation")),
  route("/webhooks", () => import("./views/webhooks")),

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

  // ---- commerce, support and account screens ---------------------------------------------
  //
  // A route here whose view module does not exist yet is not a broken link: `navGroups` filters
  // the navigation by whether a route is declared, and the router renders an honest "not in this
  // build" state for a declared route whose chunk is missing. So a screen appears in the panel
  // the moment its module lands, with no second edit.
  route("/register", () => import("./views/register")),
  route("/cart", () => import("./views/cart")),
  route("/checkout", () => import("./views/checkout")),

  route("/coupons", () => import("./views/coupons")),
  route("/tax-rates", () => import("./views/tax-rates")),
  route("/currencies", () => import("./views/currencies")),
  route("/config-options", () => import("./views/config-options")),
  route("/settings/system", () => import("./views/system-settings")),
  route("/announcements", () => import("./views/announcements")),
  route("/logs", () => import("./views/logs")),
  route("/cancellations", () => import("./views/cancellations")),

  route("/tickets", () => import("./views/tickets")),
  route("/tickets/new", () => import("./views/ticket-new")),
  route("/tickets/:id", () => import("./views/ticket")),
  route("/support", () => import("./views/support-queue")),
  // Opening a ticket *for* a customer. A separate screen from /tickets/new because the customer's
  // form cannot name a customer — that would be a ticket on someone else's account — and the staff
  // form has to.
  route("/support/new", () => import("./views/support")),

  route("/credits", () => import("./views/credits")),
  route("/affiliates", () => import("./views/affiliates")),
  route("/portal/credits", () => import("./views/portal-credits")),
  route("/portal/security", () => import("./views/portal-security")),
  route("/portal/notifications", () => import("./views/portal-notifications")),
  route("/portal/payment-methods", () => import("./views/portal-payment-methods")),

  // ---- platform and integration screens ---------------------------------------------------
  //
  // Each of these is the operator half of a P4/P6 epic: the gateway and server adapters, the
  // importer, locales, OAuth2 clients, updates and the extension marketplace. Declaring the
  // route before the module exists is deliberate — the router renders an honest "not in this
  // build" state, so a half-landed feature is visible instead of a dead link.
  route("/gateways", () => import("./views/gateways")),
  route("/servers", () => import("./views/servers")),
  route("/imports", () => import("./views/imports")),
  route("/locales", () => import("./views/locales")),
  route("/oauth-clients", () => import("./views/oauth-clients")),
  route("/settings/updates", () => import("./views/updates")),
  route("/marketplace", () => import("./views/marketplace")),
  route("/users", () => import("./views/users")),
  route("/roles", () => import("./views/roles")),
  route("/security", () => import("./views/security")),
  route("/notification-templates", () => import("./views/notification-templates")),
  route("/automation", () => import("./views/automation")),
  route("/service-properties", () => import("./views/service-properties")),
  route("/holds", () => import("./views/holds")),
  route("/upgrades", () => import("./views/upgrades")),
  route("/providers", () => import("./views/providers")),
  route("/ticket-fields", () => import("./views/ticket-fields")),
];

/** True when a route for `path` is declared.
 *
 *  The navigation uses this so a menu entry cannot point at a screen the router does not know:
 *  the two are generated from one list, and an entry for an undeclared route is not rendered.
 *  That is what makes "a link in the panel is never a bug" true rather than aspirational, and it
 *  means a screen appears in the menu the moment its route is added. */
export function hasRoute(path: string): boolean {
  return routes.some((r) => r.pattern === path);
}

/** The labels a breadcrumb uses for a path segment. */
export const SEGMENT_LABELS: Record<string, string> = {
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
  cart: "Cart",
  checkout: "Checkout",
  register: "Create an account",
  coupons: "Coupons",
  "tax-rates": "Tax rates",
  currencies: "Currencies",
  "config-options": "Config options",
  system: "System settings",
  announcements: "Announcements",
  logs: "Logs",
  cancellations: "Cancellations",
  tickets: "Tickets",
  support: "Support",
  credits: "Credit",
  affiliates: "Affiliates",
  security: "Security",
  notifications: "Notifications",
  "payment-methods": "Payment methods",
  gateways: "Payment gateways",
  servers: "Provisioning servers",
  imports: "Imports",
  locales: "Locales",
  "oauth-clients": "OAuth clients",
  updates: "Updates",
  marketplace: "Marketplace",
  users: "Users",
  roles: "Roles",
  "notification-templates": "Notification templates",
  automation: "Automation",
  "service-properties": "Service properties",
  holds: "Holds",
  upgrades: "Upgrades",
  providers: "Sign-in providers",
  "ticket-fields": "Ticket fields",
};
