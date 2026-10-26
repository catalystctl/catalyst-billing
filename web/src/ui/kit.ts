// The component kit.
//
// Pages compose these and add layout. Keeping every visual primitive in one module is what
// makes thirty screens look like one product: a page cannot invent its own badge colour or
// its own empty state, because the function that draws it already exists.
//
// Every helper returns a real element, so a page can attach a listener to it, put it in a
// grid, or hand it to a dialog without a wrapper.

import { ApiError, type Page } from "../lib/api";
import { h, fill, swap, type Child, type Props } from "../lib/dom";
import { icon, type IconName } from "../lib/icons";
import {
  humanStatus,
  money,
  type Amount,
  parseMoneyToMinor,
  percent,
  statusTone,
  type Tone,
} from "../lib/format";
import type { Ctx } from "../lib/types";

export { h, fill, swap };
export type { Child, Props };

// --------------------------------------------------------------- primitives --

export function badge(label: Child, tone: Tone = "neutral", opts?: { dot?: boolean; lg?: boolean }): HTMLElement {
  const cls = ["badge", tone !== "neutral" ? `badge--${tone}` : "", opts?.lg ? "badge--lg" : ""]
    .filter(Boolean)
    .join(" ");
  return h("span", { class: cls }, opts?.dot ? h("span", { class: "badge-dot" }) : null, label);
}

export function statusBadge(status: string | null | undefined, opts?: { dot?: boolean; label?: string }): HTMLElement {
  const value = status ?? "";
  return badge(opts?.label ?? humanStatus(value), statusTone(value), { dot: opts?.dot ?? true });
}

export function dot(tone: Tone = "neutral"): HTMLElement {
  return h("span", { class: `badge-dot badge-dot--${tone}` });
}

export interface MoneyOptions {
  size?: "sm" | "md" | "lg" | "xl";
  code?: boolean;
  signed?: boolean;
}

/** Renders an amount from the API. Never takes a number: minor units and a scale are the
 *  only safe inputs, and accepting a float here is how a panel starts disagreeing with the
 *  ledger by a cent. */
export function moneyEl(amount: Amount | null | undefined, opts: MoneyOptions = {}): HTMLElement {
  const cls = ["money", opts.size && opts.size !== "md" ? `money--${opts.size}` : ""].filter(Boolean).join(" ");
  if (!amount) return h("span", { class: cls }, "—");
  const negative = amount.minor < 0;
  const showCode = opts.code ?? true;
  return h(
    "span",
    { class: cls + (opts.signed && negative ? " money--neg" : "") },
    showCode ? h("span", { class: "money-code" }, amount.currency) : null,
    opts.signed && amount.minor > 0 ? "+" : "",
    money(amount),
  );
}

export function avatar(name: string, opts?: { size?: "sm" | "lg"; accent?: boolean }): HTMLElement {
  const parts = name.trim().split(/\s+/).filter(Boolean);
  const text =
    parts.length > 1
      ? `${parts[0]?.charAt(0) ?? ""}${parts[parts.length - 1]?.charAt(0) ?? ""}`
      : (parts[0] ?? "?").slice(0, 2);
  return h(
    "span",
    {
      class: ["avatar", opts?.size === "lg" ? "avatar--lg" : "", opts?.accent ? "avatar--accent" : ""]
        .filter(Boolean)
        .join(" "),
      "aria-hidden": "true",
    },
    text,
  );
}

// ------------------------------------------------------------------ buttons --

export interface ButtonOptions {
  variant?: "default" | "primary" | "ghost" | "danger" | "danger-solid";
  size?: "sm" | "md" | "lg";
  icon?: IconName;
  iconAfter?: IconName;
  title?: string;
  href?: string;
  type?: "button" | "submit";
  disabled?: boolean;
  block?: boolean;
  onclick?: (event: MouseEvent) => void;
  id?: string;
  /** target for a link button. Used by "open in another product", where the other origin owns its own
   *  session and the billing page should survive the visit. */
  target?: string;
  /** rel for a link button. A cross-origin link opened in a new tab needs noopener. */
  rel?: string;
}

export function btn(label: Child, opts: ButtonOptions = {}): HTMLButtonElement | HTMLAnchorElement {
  const cls = [
    "btn",
    opts.variant && opts.variant !== "default" ? `btn--${opts.variant}` : "",
    opts.size === "sm" ? "btn--sm" : opts.size === "lg" ? "btn--lg" : "",
    opts.block ? "btn--block" : "",
  ]
    .filter(Boolean)
    .join(" ");
  const children: Child[] = [opts.icon ? icon(opts.icon, 14) : null, label, opts.iconAfter ? icon(opts.iconAfter, 14) : null];
  if (opts.href) {
    return h("a", { class: cls, href: opts.href, title: opts.title, target: opts.target, rel: opts.rel }, ...children);
  }
  return h(
    "button",
    {
      class: cls,
      type: opts.type ?? "button",
      title: opts.title,
      disabled: opts.disabled,
      id: opts.id,
      onclick: opts.onclick,
    },
    ...children,
  );
}

export function iconBtn(name: IconName, label: string, onclick: (e: MouseEvent) => void, opts?: { size?: "sm" }): HTMLButtonElement {
  return h(
    "button",
    {
      class: `btn btn--ghost btn--icon${opts?.size === "sm" ? " btn--sm" : ""}`,
      type: "button",
      title: label,
      "aria-label": label,
      onclick,
    },
    icon(name, 14),
  );
}

/** Disables a button for the duration of an async action and reports a failure as a toast. */
export async function withBusy(
  ctx: Ctx,
  button: HTMLButtonElement | HTMLAnchorElement | null,
  fn: () => Promise<void>,
): Promise<void> {
  const wasDisabled = button instanceof HTMLButtonElement ? button.disabled : false;
  if (button instanceof HTMLButtonElement) button.disabled = true;
  const original = button?.querySelector(".spinner");
  if (button) button.classList.add("is-busy");
  try {
    await fn();
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") return;
    if (err instanceof ApiError) {
      ctx.toast(err.detail || humanStatus(err.code), { tone: "danger", detail: err.code });
    } else {
      ctx.toast("Something went wrong", { tone: "danger", detail: String(err) });
    }
  } finally {
    if (button instanceof HTMLButtonElement) button.disabled = wasDisabled;
    if (button) button.classList.remove("is-busy");
    if (original) original.remove();
  }
}

// -------------------------------------------------------------------- forms --

export interface FieldOptions {
  label?: string;
  hint?: string;
  error?: string;
  required?: boolean;
  control: HTMLElement;
  id?: string;
  class?: string;
}

export function field(opts: FieldOptions): HTMLElement {
  const id = opts.id ?? opts.control.id;
  return h(
    "div",
    { class: ["field", opts.class ?? ""].filter(Boolean).join(" ") },
    opts.label
      ? h(
          "label",
          { class: "field-label", for: id },
          opts.label,
          opts.required ? h("span", { class: "req" }, "*") : null,
        )
      : null,
    opts.control,
    opts.hint && !opts.error ? h("span", { class: "field-hint" }, opts.hint) : null,
    opts.error ? h("span", { class: "field-error" }, icon("alertCircle", 11), opts.error) : null,
  );
}

export interface InputOptions extends Props {
  id?: string;
  name?: string;
  type?: string;
  placeholder?: string;
  value?: string | number;
  autocomplete?: string;
  required?: boolean;
  min?: string | number;
  max?: string | number;
  step?: string | number;
  inputmode?: string;
  maxlength?: number;
  oninput?: (event: Event) => void;
  onchange?: (event: Event) => void;
  onkeydown?: (event: KeyboardEvent) => void;
}

export function input(opts: InputOptions = {}): HTMLInputElement {
  const { class: extra, ...rest } = opts;
  return h("input", {
    class: ["input", extra ?? ""].filter(Boolean).join(" "),
    type: opts.type ?? "text",
    ...rest,
  });
}

export function textarea(opts: InputOptions = {}): HTMLTextAreaElement {
  return h("textarea", {
    class: ["textarea", opts.class ?? ""].filter(Boolean).join(" "),
    id: opts.id,
    name: opts.name,
    placeholder: opts.placeholder,
    value: opts.value,
    rows: 3,
    maxlength: opts.maxlength,
  });
}

export interface SelectOption {
  value: string;
  label: string;
  disabled?: boolean;
}

export function select(
  options: SelectOption[],
  opts: { id?: string; name?: string; value?: string; onchange?: (e: Event) => void; class?: string } = {},
): HTMLSelectElement {
  const el = h("select", {
    class: ["select", opts.class ?? ""].filter(Boolean).join(" "),
    id: opts.id,
    name: opts.name,
    onchange: opts.onchange,
  });
  for (const option of options) {
    const o = h("option", { value: option.value, disabled: option.disabled }, option.label);
    if (option.value === opts.value) o.selected = true;
    el.appendChild(o);
  }
  return el;
}

export function checkboxField(label: Child, opts: InputOptions = {}): HTMLElement {
  const box = h("input", { type: "checkbox", checked: opts.checked, onchange: opts.onchange });
  return h("label", { class: "checkbox" }, box, h("span", null, label));
}

export function switchField(label: Child, opts: InputOptions = {}): HTMLElement {
  const box = h("input", { type: "checkbox", checked: opts.checked, onchange: opts.onchange });
  return h("label", { class: "switch" }, box, h("span", null, label));
}

/** A search box with a clear button. `onchange` fires debounced by the caller. */
export function searchInput(opts: {
  value?: string;
  placeholder?: string;
  oninput: (value: string) => void;
  autofocus?: boolean;
}): HTMLElement {
  const box = h("input", {
    class: "input",
    type: "search",
    placeholder: opts.placeholder ?? "Search…",
    value: opts.value ?? "",
    autofocus: opts.autofocus,
    oninput: (event: Event) => {
      const value = (event.target as HTMLInputElement).value;
      clear.hidden = value === "";
      opts.oninput(value);
    },
    onkeydown: (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        (event.target as HTMLInputElement).value = "";
        clear.hidden = true;
        opts.oninput("");
      }
    },
  });
  const clear = h(
    "button",
    {
      class: "search-clear",
      type: "button",
      title: "Clear search",
      "aria-label": "Clear search",
      hidden: !opts.value,
      onclick: () => {
        box.value = "";
        clear.hidden = true;
        opts.oninput("");
        box.focus();
      },
    },
    icon("x", 11),
  );
  return h("div", { class: "search-input" }, icon("search", 14), box, clear);
}

/** Reads a form's named inputs into an object, trimming strings. */
export function formValues(form: HTMLFormElement): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [key, value] of new FormData(form).entries()) {
    if (typeof value === "string") out[key] = value.trim();
  }
  return out;
}

// ------------------------------------------------------------------- layout --

export interface CardOptions {
  title?: Child;
  sub?: Child;
  actions?: Child;
  body?: Child;
  foot?: Child;
  flush?: boolean;
  tight?: boolean;
  class?: string;
  /** The colour of the small square before the title. Pink is the console's default marker;
   *  amber marks a panel that wants action, green one that reports a healthy state. */
  marker?: "pink" | "accent" | "success";
}

export function card(opts: CardOptions): HTMLElement {
  return h(
    "section",
    { class: ["card", opts.class ?? ""].filter(Boolean).join(" ") },
    opts.title || opts.actions
      ? h(
          "header",
          { class: "card-head" },
          h(
            "h2",
            { class: ["card-title", opts.marker ? `card-title--${opts.marker}` : ""].filter(Boolean).join(" ") },
            opts.title,
            opts.sub ? h("span", { class: "card-sub" }, opts.sub) : null,
          ),
          opts.actions ? h("div", { class: "card-actions" }, opts.actions) : null,
        )
      : null,
    opts.body !== undefined
      ? h("div", { class: ["card-body", opts.flush ? "card-body--flush" : "", opts.tight ? "card-body--tight" : ""].filter(Boolean).join(" ") }, opts.body)
      : null,
    opts.foot ? h("footer", { class: "card-foot" }, opts.foot) : null,
  );
}

export interface PageHeadOptions {
  title: Child;
  sub?: Child;
  actions?: Child;
  badge?: Child;
  /** Small operational section label above the title, matching the console's visual rhythm. */
  eyebrow?: Child;
}

export function pageHead(opts: PageHeadOptions): HTMLElement {
  return h(
    "header",
    { class: "page-head" },
    h(
      "div",
      { class: "page-head-text" },
      h("div", { class: "page-kicker" }, opts.eyebrow ?? "Catalyst billing"),
      h("h1", { class: "page-title" }, opts.title, opts.badge ?? null),
      opts.sub ? h("p", { class: "page-sub" }, opts.sub) : null,
    ),
    opts.actions ? h("div", { class: "page-actions" }, opts.actions) : null,
  );
}

export function sectionTitle(text: Child, actions?: Child): HTMLElement {
  return h("div", { class: "section-title" }, text, actions ?? null);
}

export function metaGrid(items: Array<{ label: Child; value: Child }>): HTMLElement {
  return h(
    "div",
    { class: "meta-grid" },
    ...items.map((item) =>
      h("div", { class: "meta" }, h("span", { class: "meta-label" }, item.label), h("span", { class: "meta-value" }, item.value)),
    ),
  );
}

export function kv(key: Child, value: Child): HTMLElement {
  return h("div", { class: "kv" }, h("span", { class: "kv-key" }, key), h("span", { class: "kv-value" }, value));
}

export function banner(
  tone: "neutral" | "info" | "warning" | "danger" | "success",
  title: Child,
  body?: Child,
  action?: Child,
): HTMLElement {
  const iconName: IconName =
    tone === "danger" ? "alertCircle" : tone === "warning" ? "alertTriangle" : tone === "success" ? "checkCircle" : "info";
  return h(
    "div",
    { class: `banner banner--${tone}` },
    icon(iconName, 15),
    h("div", { class: "stack-sm" }, h("strong", null, title), body ? h("span", null, body) : null),
    action ? h("div", { class: "spacer" }, action) : null,
  );
}

export function tabs(
  items: Array<{ key: string; label: string; count?: number }>,
  active: string,
  onSelect: (key: string) => void,
): HTMLElement {
  const nav = h("nav", { class: "tabs", role: "tablist" });
  for (const item of items) {
    nav.appendChild(
      h(
        "button",
        {
          class: `tab${item.key === active ? " is-active" : ""}`,
          type: "button",
          role: "tab",
          "aria-selected": item.key === active ? "true" : "false",
          onclick: () => onSelect(item.key),
        },
        item.label,
        item.count !== undefined ? h("span", { class: "tab-count" }, String(item.count)) : null,
      ),
    );
  }
  return nav;
}

/** A two-segment bar: how much of an invoice is settled, and how much is not.
 *
 *  A single "percent paid" bar is technically correct and reads as broken when it is empty —
 *  a customer looking at an unpaid invoice sees a grey line and has to check the legend to
 *  learn that nothing has gone wrong. Two segments answer the question the bar is asked.
 *
 *  The width comes from a data attribute and a stylesheet rule, never an inline style: the CSP
 *  has no 'unsafe-inline' for style attributes. */
export function balanceBar(paid: number, total: number, opts?: { dueTone?: "warning" | "danger" }): HTMLElement {
  const ratio = total > 0 ? Math.max(0, Math.min(1, paid / total)) : 0;
  const bucket = Math.max(0, Math.min(10, Math.round(ratio * 10)));
  const percent = Math.round(ratio * 100);
  const dueTone = opts?.dueTone === "danger" ? " balance-due--danger" : "";
  // When nothing is paid the bar is one uninterrupted "outstanding" segment, not a full
  // green bar: at a 0% width the flex remainder would otherwise paint the whole track as
  // unpaid *and* leave the paid segment invisible, which is right, but the two children
  // still read as a 0/100 split with a stray 1px gap.
  const segments =
    bucket === 0
      ? [h("div", { class: `balance-due${dueTone}` })]
      : bucket === 10
        ? [h("div", { class: "balance-paid" })]
        : [h("div", { class: "balance-paid" }), h("div", { class: `balance-due${dueTone}` })];
  return h(
    "div",
    {
      class: "balance-bar",
      "data-progress": String(bucket),
      role: "img",
      "aria-label": `${percent}% of the total is paid`,
    },
    ...segments,
  );
}

export function progressBar(value: number, tone?: "success" | "warning" | "danger"): HTMLElement {
  // The width comes from a data attribute and a stylesheet rule, never an inline style:
  // the CSP has no 'unsafe-inline' for style attributes.
  const bucket = Math.max(0, Math.min(10, Math.round(percent(value, 100) / 10)));
  return h(
    "div",
    { class: "progress", "data-progress": String(bucket), role: "progressbar", "aria-valuenow": String(Math.round(value)) },
    h("div", { class: `progress-fill${tone ? ` progress-fill--${tone}` : ""}` }),
  );
}

// ------------------------------------------------------------------- states --

export function spinner(size = 14): HTMLElement {
  return h("span", { class: "spinner", "aria-hidden": "true" });
}

export function skeletonLines(count = 3, widths: number[] = [80, 60, 40]): HTMLElement {
  const box = h("div", { class: "skeleton-rows" });
  for (let i = 0; i < count; i++) {
    box.appendChild(h("div", { class: `skeleton skeleton-line w-${widths[i % widths.length] ?? 80}` }));
  }
  return box;
}

/** A table-shaped placeholder: the layout does not jump when data arrives. */
export function skeletonTable(rows = 6, cols = 5): HTMLElement {
  const body = h("div", { class: "card-body card-body--tight" });
  for (let r = 0; r < rows; r++) {
    const row = h("div", { class: "row skeleton-row" });
    for (let c = 0; c < cols; c++) {
      const width = c === 0 ? 24 : c === cols - 1 ? 10 : 16;
      row.appendChild(h("div", { class: `skeleton skeleton-line w-${width}` }));
    }
    body.appendChild(row);
  }
  return h("div", { class: "card" }, body);
}

export function skeletonPage(): HTMLElement {
  return h(
    "div",
    { class: "page-skeleton", "aria-busy": "true", "aria-label": "Loading" },
    h(
      "div",
      { class: "skeleton-head" },
      h("div", { class: "skeleton skeleton-line w-20" }),
      h("div", { class: "skeleton skeleton-line w-45" }),
    ),
    skeletonTable(),
  );
}

export interface EmptyOptions {
  title: Child;
  body?: Child;
  action?: Child;
  icon?: IconName;
}

export function emptyState(opts: EmptyOptions): HTMLElement {
  return h(
    "div",
    { class: "empty" },
    h("div", { class: "empty-art" }, icon(opts.icon ?? "inbox", 56)),
    h("p", { class: "empty-title" }, opts.title),
    opts.body ? h("p", { class: "empty-body" }, opts.body) : null,
    opts.action ? h("div", { class: "empty-actions" }, opts.action) : null,
  );
}

export function errorState(err: unknown, retry?: () => void): HTMLElement {
  const apiErr = err instanceof ApiError ? err : null;
  const message = apiErr?.detail || (err instanceof Error ? err.message : "The request failed.");
  // A 403 or a 404 is an answer, not a transient failure: the same request will produce the
  // same answer, so offering "try again" is a button that cannot work. Only a failure that
  // could plausibly succeed next time gets one.
  const retryable = apiErr === null || apiErr.retryable || apiErr.status === 0 || apiErr.status >= 500;
  return h(
    "div",
    { class: "error-state" },
    badge(apiErr ? humanStatus(apiErr.code) : "Error", "danger"),
    h("p", { class: "empty-title" }, "That did not work"),
    h("p", { class: "empty-body" }, message),
    apiErr ? h("code", { class: "error-code" }, `${apiErr.status} ${apiErr.code}`) : null,
    retry && retryable ? h("div", { class: "empty-actions" }, btn("Try again", { icon: "refresh", size: "sm", onclick: retry })) : null,
  );
}

/** Loads data, showing a skeleton immediately and replacing it when the data arrives.
 *
 *  The skeleton is returned synchronously, so a page never renders an empty shell while a
 *  request is in flight — the layout is stable from the first frame, which is most of what
 *  "fast" feels like. */
export function resource<T>(
  ctx: Ctx,
  load: (signal: AbortSignal) => Promise<T>,
  render: (data: T) => Node,
  opts?: { skeleton?: () => Node },
): HTMLElement {
  const host = h("div", { class: "resource" }, (opts?.skeleton ?? skeletonPage)());
  const controller = new AbortController();
  ctx.onDispose(() => controller.abort());

  const run = async () => {
    try {
      const data = await load(controller.signal);
      if (controller.signal.aborted) return;
      swap(host, render(data));
    } catch (err) {
      if (controller.signal.aborted || (err instanceof DOMException && err.name === "AbortError")) return;
      swap(host, errorState(err, () => {
        swap(host, (opts?.skeleton ?? skeletonPage)());
        void run();
      }));
    }
  };
  void run();
  return host;
}

/** A mutation helper: runs `fn`, shows a success or failure toast, and refreshes the page's
 *  data on success by calling `after`. */
export async function mutate(
  ctx: Ctx,
  options: {
    run: () => Promise<unknown>;
    success?: string;
    detail?: string;
    after?: () => void | Promise<void>;
    onError?: (err: unknown) => boolean | void;
  },
): Promise<boolean> {
  try {
    await options.run();
    if (options.success) ctx.toast(options.success, { tone: "success", detail: options.detail });
    await options.after?.();
    return true;
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") return false;
    if (options.onError?.(err)) return false;
    if (err instanceof ApiError) {
      ctx.toast(err.detail || humanStatus(err.code), { tone: "danger", detail: err.code });
    } else {
      ctx.toast("Something went wrong", { tone: "danger", detail: String(err) });
    }
    return false;
  }
}

// ------------------------------------------------------------------ helpers --

export function copyButton(ctx: Ctx, value: string, label = "Copy"): HTMLButtonElement {
  return h(
    "button",
    {
      class: "copy",
      type: "button",
      title: `Copy ${label}`,
      onclick: async () => {
        try {
          await navigator.clipboard.writeText(value);
          ctx.toast("Copied to clipboard", { tone: "success", duration: 1600 });
        } catch {
          ctx.toast("Could not copy", { tone: "danger", detail: "The clipboard is unavailable in this browser." });
        }
      },
    },
    icon("copy", 11),
    label,
  );
}

export function code(value: string): HTMLElement {
  return h("code", { class: "inline-code" }, value);
}

export function mono(value: string | number, title?: string): HTMLElement {
  return h("span", { class: "mono", title: title ?? String(value) }, String(value));
}

export function cellMain(primary: Child, secondary?: Child): HTMLElement {
  return h("div", { class: "cell-main" }, h("div", { class: "primary" }, primary), secondary ? h("div", { class: "secondary" }, secondary) : null);
}

export function amountFromInput(raw: string, scale = 2): number | null {
  return parseMoneyToMinor(raw, scale);
}

/** Reads the `next_cursor` from a page and returns it only when it is usable. */
export function cursorOf<T>(page: Page<T>): string | null {
  return page.next_cursor ?? null;
}

export function isAbort(err: unknown): boolean {
  return err instanceof DOMException && err.name === "AbortError";
}
