// System settings.
//
// The settings surface is deliberately *bulk*: a read is one group, a write is one group,
// validated as a whole before anything is stored. So this page is a set of per-group forms, each
// with its own Save button, and saving one group cannot half-apply another.
//
// Two rules shape it, and both come from the server rather than from taste:
//
//  - **A secret is write-only.** `GET /api/v1/staff/settings` never returns a secret's value: it
//    returns `is_secret: true`, `is_set: true|false` and the literal marker `"[redacted]"`. So a
//    secret renders as an empty password field labelled "set" or "not set", and an empty field
//    means "leave it alone" — the server skips the marker, and this page skips an empty box.
//    There is no control anywhere that can reveal a stored credential, because there is no
//    response that contains one.
//  - **A group is a transaction.** The Save button belongs to a group and posts that group. The
//    response is the committed state, which replaces the form, so what is on screen after a save
//    is what the server stored — not what was typed.
//
// Fields are typed by the `kind` the server declares (`string`, `int`, `bool`, `json`) and
// narrowed by `choices` where a closed set exists. A boolean is a switch, an int is a number
// input, JSON is a textarea with the value pretty-printed, and a string with choices is a select.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime, titleCase } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { badge, banner, btn, card, pageHead, resource, skeletonPage, withBusy } from "../ui/kit";

export const meta = { title: "System settings", crumb: "System settings" };

// ---------------------------------------------------------------- payloads --

/** One setting, from `settingsGroupJSON` in internal/httpapi/system_settings.go.
 *
 *  `value` is the stored text — or `"[redacted]"` for a secret, which is why a secret's field
 *  never contains anything a page could show. `is_set` is the only fact about a secret's
 *  contents this screen ever receives. */
interface SettingKey {
  group: string;
  key: string;
  kind: "string" | "int" | "bool" | "json";
  value: string;
  is_secret: boolean;
  is_set: boolean;
  updated_at: string | null;
}

interface SettingsGroup {
  group: string;
  keys: SettingKey[];
}

interface SettingsResponse {
  data: SettingsGroup[];
  /** The declared group names in render order. Present on the all-groups read. */
  groups?: string[];
}

/** The marker the server substitutes for a secret. A secret sent back as this is skipped, so
 *  this page never sends it — but naming it makes the intent explicit. */
const REDACTED = "[redacted]";

/** Human labels for the groups and keys. The server's keys are stable identifiers
 *  (`smtp_password`), not copy, so the words live here rather than in the API. */
const GROUP_LABELS: Record<string, string> = {
  general: "General",
  invoices: "Invoices",
  credits: "Credit",
  tax: "Tax",
  tickets: "Tickets",
  cronjob: "Scheduled jobs",
  mail: "Email",
  announcements: "Announcements",
  storefront: "Storefront",
};

const GROUP_HINTS: Record<string, string> = {
  general: "The company's name, the system address and whether registration is open.",
  invoices: "Numbering, payment terms and what a customer sees on a document.",
  credits: "Whether customers can top up, and the limits that apply.",
  tax: "Whether tax is charged at all, and whether quoted prices include it.",
  tickets: "Support is off until it is switched on, and closes itself when it is on.",
  cronjob: "How long the dunning ladder waits before it cancels or terminates.",
  mail: "Outgoing mail. The password is write-only: it is never read back.",
  announcements: "Whether the announcement feed is shown to customers.",
  storefront: "How the public shop presents the catalog: the category strip, stock counts and the currency switcher.",
};

const KEY_LABELS: Record<string, string> = {
  home_categories: "Categories on the home page",
  show_stock: "Show stock counts",
  low_stock_threshold: "Low-stock threshold",
  currency_switcher: "Offer the currency switcher",
  company_name: "Company name",
  system_email_address: "System email address",
  tos: "Terms of service",
  registration_disabled: "Registration is closed",
  pagination: "Rows per page",
  bill_to_text: "Bill-to text",
  notes_client_visible: "Client notes are visible",
  invoice_number_prefix: "Invoice number prefix",
  invoice_number_padding: "Invoice number padding",
  credit_note_prefix: "Credit note prefix",
  credit_note_padding: "Credit note padding",
  due_days: "Days until due",
  proforma: "Issue proformas",
  enabled: "Enabled",
  minimum_deposit_minor: "Minimum deposit (minor units)",
  maximum_deposit_minor: "Maximum deposit (minor units)",
  maximum_credit_minor: "Maximum balance (minor units)",
  auto_use: "Use credit automatically",
  credit_on_downgrade: "Credit the unused part of the period on a downgrade",
  default_mode: "Tax mode",
  disabled: "Support is disabled",
  client_closing_disabled: "Customers cannot close their own tickets",
  departments: "Departments",
  autoclose_days: "Auto-close after (days)",
  order_cancel_days: "Cancel unpaid orders after (days)",
  terminate_days: "Terminate suspended services after (days)",
  disable: "Do not send mail",
  must_verify: "Require a verified email address",
  smtp_host: "SMTP host",
  smtp_port: "SMTP port",
  smtp_username: "SMTP username",
  smtp_password: "SMTP password",
  smtp_encryption: "SMTP encryption",
  smtp_from: "From address",
};

const KEY_HINTS: Record<string, string> = {
  company_name: "Appears on invoices and in the page title.",
  tos: "When this is not empty, registration and checkout require the box to be ticked.",
  pagination: "How many rows a list requests per page. The server clamps it to 100.",
  bill_to_text: "A short block printed above the customer's address on an invoice.",
  invoice_number_prefix: "The series prefix. Changing it affects documents issued from now on.",
  invoice_number_padding: "Zero-padding for the sequence, e.g. 6 gives INV-000042.",
  due_days: "Payment terms. The due date is issued_at plus this many days.",
  proforma: "A proforma is a provisional document that gains its number when it is paid.",
  minimum_deposit_minor: "In minor units: 500 is 5.00 in a two-decimal currency.",
  maximum_deposit_minor: "In minor units.",
  maximum_credit_minor: "In minor units. A top-up that would exceed it is refused.",
  credit_on_downgrade: "When on, moving a service to a cheaper plan credits the unused part of the period. When off, the change is still recorded but nothing is returned.",
  default_mode: "Exclusive adds tax to the quoted price; inclusive treats the price as tax-included.",
  departments: "A JSON array of department names, e.g. [\"billing\",\"technical\"].",
  smtp_password: "Write-only. An empty box leaves the stored password unchanged.",
  smtp_encryption: "none, starttls or tls.",
  smtp_from: "The address a customer sees. Defaults to the system address when empty.",
  must_verify: "When on, a customer must confirm their address before signing in or buying.",
};

/** The closed sets the server declares for two keys. They are repeated here because the
 *  response carries `choices` only in the Go definition, not on the wire, so the form would
 *  otherwise render a free-text box for a value the server will refuse. */
const CHOICES: Record<string, string[]> = {
  "tax.default_mode": ["exclusive", "inclusive"],
  "mail.smtp_encryption": ["none", "starttls", "tls"],
};

export default async function systemSettingsPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("settings:write")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "System settings" }),
      banner(
        "danger",
        "You cannot read the settings",
        h(
          "span",
          null,
          "This page needs the ",
          h("code", { class: "inline-code" }, "settings:write"),
          " permission. Reading is gated with writing on purpose: the SMTP host and the gateway configuration are reconnaissance, not support information.",
        ),
        btn("Back to the dashboard", { href: "/dashboard", size: "sm", variant: "ghost" }),
      ),
    );
  }

  const host = h("div");
  // The group a link asked for (`?group=tax`) is scrolled to after the render, so a link from
  // the tax-rate screen lands on the switch that turns tax on rather than at the top.
  const wanted = ctx.route.query.get("group") ?? "";

  /** Loads every group, or reports that this build does not serve the settings API.
   *
   *  `GET /api/v1/staff/settings` is the operator-surface package's route. Until it is merged
   *  the request is a 404, and a 404 here is a fact about the installation, not a failed
   *  request: retrying produces the same answer, so the screen says what is missing instead of
   *  offering a button that cannot work. */
  const load = async (signal: AbortSignal): Promise<SettingsResponse | "unavailable"> => {
    try {
      // `no-store` on the server, and `cache: false` here: a settings page that showed a cached
      // value would show one operator another operator's last save.
      return await ctx.api.get<SettingsResponse>("/api/v1/staff/settings", { cache: false, signal });
    } catch (err) {
      if (err instanceof ApiError && (err.status === 404 || err.status === 501)) return "unavailable";
      throw err;
    }
  };

  const paint = (): void => {
    swap(
      host,
      resource(
        ctx,
        load,
        (data) => {
          if (data === "unavailable") return unavailableCard();
          const groups = (data as SettingsResponse).data ?? [];
          if (groups.length === 0) {
            return card({
              body: h(
                "div",
                { class: "empty" },
                h("p", { class: "empty-title" }, "No settings groups"),
                h(
                  "p",
                  { class: "empty-body" },
                  "The server declares no settings groups, which means this build has no configuration to edit.",
                ),
              ),
            });
          }
          // The tax group is linked from the tax-rate screen, so it is scrolled into view once
          // the cards exist. `scrollIntoView` is not a style write, so the CSP is not involved.
          if (wanted) {
            window.requestAnimationFrame(() => {
              document.getElementById(`settings-${wanted}`)?.scrollIntoView({ block: "start", behavior: "smooth" });
            });
          }
          return h("div", { class: "stack" }, ...groups.map((group: SettingsGroup) => groupCard(group)));
        },
        { skeleton: skeletonPage },
      ),
    );
  };

  /** What a missing settings API looks like. It names the endpoint, says what still works, and
   *  points at the file that owns it, because the reader of this message is as likely to be an
   *  operator who can deploy a build as a developer who can merge one. */
  const unavailableCard = (): HTMLElement =>
    card({
      title: "System settings are not available in this build",
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "p",
          { class: "field-hint" },
          "This installation does not serve ",
          h("code", { class: "inline-code" }, "GET /api/v1/staff/settings"),
          ", so there is no configuration to show. That is a property of the build that is running, not of your account: reloading will give the same answer until a build that includes the settings API is deployed.",
        ),
        h(
          "p",
          { class: "field-hint" },
          "Everything else in the panel works. The settings that are stored are still applied — this screen simply cannot read or change them.",
        ),
        h(
          "div",
          { class: "row" },
          btn("Back to the dashboard", { href: "/dashboard", size: "sm", icon: "home" }),
          btn("Open the catalog", { href: "/catalog-management", size: "sm", variant: "ghost", icon: "box" }),
        ),
      ),
    });

  // ---------------------------------------------------------------- groups --

  const groupCard = (group: SettingsGroup): HTMLElement => {
    const label = GROUP_LABELS[group.group] ?? titleCase(group.group);
    const controls = new Map<string, () => string | null>();
    const fieldHost = h("div", { class: "settings-grid" });
    const errorHost = h("div");

    for (const key of group.keys ?? []) {
      const control = controlFor(key, controls);
      fieldHost.appendChild(
        h(
          "div",
          { class: ["settings-field", key.kind === "json" || key.kind === "string" && key.key === "tos" ? "settings-field--wide" : ""].filter(Boolean).join(" ") },
          control,
        ),
      );
    }

    const save = btn("Save " + label.toLowerCase(), { variant: "primary", icon: "check" }) as HTMLButtonElement;
    save.addEventListener("click", () => void submitGroup(group, controls, save, errorHost));

    const secrets = (group.keys ?? []).filter((key) => key.is_secret);
    const missing = secrets.filter((key) => !key.is_set).length;

    return h(
      "section",
      { class: "settings-group", id: `settings-${group.group}` },
      card({
        title: label,
        sub: GROUP_HINTS[group.group] ?? undefined,
        actions: h(
          "div",
          { class: "row" },
          missing > 0 ? badge(`${missing} secret${missing === 1 ? "" : "s"} not set`, "warning", { dot: true }) : null,
          save,
        ),
        body: h(
          "div",
          { class: "stack" },
          errorHost,
          fieldHost,
          secrets.length > 0
            ? h(
                "p",
                { class: "field-hint" },
                icon("lock", 12),
                " A secret is never returned by the API. The box shows whether one is stored; typing a new value replaces it, and leaving it empty keeps it.",
              )
            : null,
        ),
      }),
    );
  };

  /** Builds one control and registers how to read it. A reader returns `null` for "invalid",
   *  which stops the whole group from being sent — a bulk write validates before it writes. */
  const controlFor = (key: SettingKey, readers: Map<string, () => string | null>): HTMLElement => {
    const label = KEY_LABELS[key.key] ?? titleCase(key.key);
    const hint = KEY_HINTS[key.key] ?? undefined;
    const choiceSet = CHOICES[`${key.group}.${key.key}`];

    const head = h(
      "div",
      { class: "settings-field-head" },
      h("span", { class: "field-label" }, label),
      key.is_secret ? h("span", { class: "chip mono" }, icon("lock", 11), "write-only") : null,
      h("span", { class: "spacer" }),
      key.is_secret
        ? badge(key.is_set ? "set" : "not set", key.is_set ? "success" : "warning", { dot: true })
        : null,
      key.updated_at
        ? h("span", { class: "faint nowrap", title: dateTime(key.updated_at) }, `saved ${dateTime(key.updated_at)}`)
        : null,
    );

    if (key.is_secret) {
      const box = h("input", {
        class: "input input--mono",
        type: "password",
        id: `setting-${key.group}-${key.key}`,
        autocomplete: "new-password",
        placeholder: key.is_set ? "Stored — type to replace" : "Not set — type to set",
        // Deliberately empty: the server never sends the value, and putting the marker here
        // would be a value an operator could save by accident.
        value: "",
      });
      readers.set(key.key, () => {
        const value = box.value;
        // An empty box means "leave the stored secret alone", which is expressed by omitting
        // the key from the payload rather than by sending "". `null` marks it skipped.
        return value === "" ? null : value;
      });
      return h(
        "div",
        { class: "stack-sm settings-control" },
        head,
        box,
        hint ? h("span", { class: "field-hint" }, hint) : null,
      );
    }

    if (key.kind === "bool") {
      const box = h("input", { type: "checkbox", checked: key.value === "true" });
      readers.set(key.key, () => (box.checked ? "true" : "false"));
      // The word beside a switch is the only place the state is written out, so it has to follow
      // the box. A label that stayed "Off" after a click would make the form disagree with itself
      // and with what Save would send.
      const state = h("span", null, key.value === "true" ? "On" : "Off");
      box.addEventListener("change", () => {
        state.textContent = box.checked ? "On" : "Off";
      });
      return h(
        "div",
        { class: "stack-sm settings-control" },
        head,
        h("label", { class: "switch" }, box, state),
        hint ? h("span", { class: "field-hint" }, hint) : null,
      );
    }

    if (choiceSet) {
      const picker = h("select", { class: "select", id: `setting-${key.group}-${key.key}` });
      for (const choice of choiceSet) {
        const option = h("option", { value: choice }, choice);
        if (choice === key.value) option.selected = true;
        picker.appendChild(option);
      }
      readers.set(key.key, () => picker.value);
      return h(
        "div",
        { class: "stack-sm settings-control" },
        head,
        picker,
        hint ? h("span", { class: "field-hint" }, hint) : null,
      );
    }

    if (key.kind === "json") {
      const box = h("textarea", {
        class: "textarea input--mono",
        id: `setting-${key.group}-${key.key}`,
        rows: 4,
        value: prettyJSON(key.value),
      });
      readers.set(key.key, () => {
        const raw = box.value.trim();
        try {
          JSON.parse(raw);
        } catch {
          return null;
        }
        return raw;
      });
      return h(
        "div",
        { class: "stack-sm settings-control" },
        head,
        box,
        h("span", { class: "field-hint" }, hint ?? "A JSON value. It is validated before it is stored."),
      );
    }

    if (key.key === "tos") {
      const box = h("textarea", {
        class: "textarea",
        id: `setting-${key.group}-${key.key}`,
        rows: 6,
        value: key.value ?? "",
        placeholder: "Leave empty to require no acceptance at registration or checkout.",
      });
      readers.set(key.key, () => box.value);
      return h(
        "div",
        { class: "stack-sm settings-control" },
        head,
        box,
        h("span", { class: "field-hint" }, hint ?? "Shown on the checkout and registration forms when it is not empty."),
      );
    }

    const numeric = key.kind === "int";
    const box = h("input", {
      class: `input${numeric ? " input--mono" : ""}`,
      type: numeric ? "number" : "text",
      step: numeric ? 1 : undefined,
      id: `setting-${key.group}-${key.key}`,
      value: key.value ?? "",
      autocomplete: "off",
    });
    readers.set(key.key, () => {
      const raw = box.value;
      if (numeric) {
        const trimmed = raw.trim();
        if (!/^-?\d+$/.test(trimmed)) return null;
        return trimmed;
      }
      return raw;
    });
    return h(
      "div",
      { class: "stack-sm settings-control" },
      head,
      box,
      hint ? h("span", { class: "field-hint" }, hint) : null,
    );
  };

  /** Saves one group. Values that were not changed are still sent: the write is a whole group
   *  by design, and a value the operator did not touch is the value they saw. */
  const submitGroup = async (
    group: SettingsGroup,
    readers: Map<string, () => string | null>,
    save: HTMLButtonElement,
    errorHost: HTMLElement,
  ): Promise<void> => {
    errorHost.replaceChildren();
    const values: Record<string, string> = {};
    const invalid: string[] = [];
    for (const key of group.keys ?? []) {
      const reader = readers.get(key.key);
      if (!reader) continue;
      const value = reader();
      if (value === null) {
        if (!key.is_secret) invalid.push(KEY_LABELS[key.key] ?? key.key);
        continue;
      }
      // A secret left empty is omitted, never sent: the server would skip the marker but an
      // empty string would genuinely blank the credential.
      if (key.is_secret && value === "") continue;
      if (key.is_secret && value === REDACTED) continue;
      values[key.key] = value;
    }
    if (invalid.length > 0) {
      swap(
        errorHost,
        h(
          "div",
          { class: "banner banner--danger" },
          icon("alertCircle", 15),
          h(
            "div",
            { class: "stack-sm" },
            h("strong", null, "A value is not valid"),
            h("span", null, `${invalid.join(", ")} — check the format and save again.`),
          ),
        ),
      );
      return;
    }

    await withBusy(ctx, save, async () => {
      try {
        const updated = await ctx.api.put<SettingsGroup>("/api/v1/staff/settings", {
          group: group.group,
          values,
        });
        ctx.toast(`${GROUP_LABELS[group.group] ?? group.group} saved`, {
          tone: "success",
          detail: `${Object.keys(values).length} value${Object.keys(values).length === 1 ? "" : "s"} written`,
        });
        // The response is the committed group. Re-render from it so the screen shows what the
        // server stored, not what was typed.
        const cardEl = save.closest(".card");
        if (cardEl && updated?.keys) {
          cardEl.replaceWith(groupCard({ group: group.group, keys: updated.keys }));
        } else {
          paint();
        }
      } catch (err) {
        swap(errorHost, h("div", { class: "banner banner--danger" }, settingsError(err)));
      }
    });
  };

  paint();

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "System settings",
      sub: "The installation's configuration, one group at a time. A secret is write-only: the API never returns it, so nothing on this page can reveal one.",
      actions: btn("Refresh", { variant: "ghost", size: "sm", icon: "refresh", onclick: () => paint() }),
    }),
    banner(
      "info",
      "Changes apply to the next request",
      h(
        "span",
        null,
        "Settings are read per request rather than cached in the process, so a saved change takes effect immediately. A secret you leave empty keeps its stored value.",
      ),
    ),
    host,
  );
}

// ----------------------------------------------------------------- helpers --

/** A JSON value, pretty-printed for editing. A value that is not valid JSON is shown as-is:
 *  the operator typed it, and rewriting it silently would hide the problem. */
function prettyJSON(raw: string): string {
  const trimmed = (raw ?? "").trim();
  if (!trimmed) return "";
  try {
    return JSON.stringify(JSON.parse(trimmed), null, 2);
  } catch {
    return raw;
  }
}

function settingsError(err: unknown): string {
  if (!(err instanceof ApiError)) return "The server could not be reached, so nothing was saved.";
  switch (err.code) {
    case "unknown_setting":
      return err.detail || "The server does not declare one of the keys this form sent.";
    case "invalid_value":
      return err.detail || "A value is not valid for its declared type. Nothing was written.";
    case "unknown_group":
      return "That settings group does not exist in this build.";
    case "group_required":
      return "The form did not name a group. Reload the page.";
    case "values_required":
      return "The form sent no values. Reload the page.";
    case "forbidden":
      return "You do not hold settings:write, so nothing was saved.";
    default:
      return err.detail || `The settings could not be saved (${err.code}).`;
  }
}
