// Service properties.
//
// A property *definition* is installation configuration: "what may a service carry". The value
// itself is set on the service (the service screen), and this screen is the catalogue those
// values are read against — the label, the description, the order, and the two flags.
//
// # The two flags are the thing operators get wrong
//
// `customer_visible` decides whether the customer's own service page shows the property at all.
// `show_on_invoice` decides whether it prints on an invoice or credit note the customer keeps.
// They are independent, and the combinations mean different things:
//
//   - visible and printed — the ordinary case;
//   - visible, not printed — a note the customer may read that does not belong on a document;
//   - printed, not visible — a property that appears on a document and nowhere else, which is
//     almost always a mistake and is worth naming as one;
//   - neither — internal only.
//
// The hint under each row and inside the editor is the *same sentence* the service screen shows
// (`propertyHint` in web/src/views/service.ts), deliberately copied rather than reworded: an
// operator who learned the sentence on one screen must not have to learn a second phrasing on
// this one.
//
// # Ordering and deletion
//
// `sort_order` is what a service page and an invoice print in, so it is part of the catalogue
// rather than a per-service preference. Reordering is one request naming the whole order, sent
// as up/down moves, because two single-row renumbers are two chances to leave two properties
// claiming the same place.
//
// A definition a service still carries cannot be deleted: the database refuses it, and the
// refusal arrives *after* the click. So this screen reads the usage count first and explains
// before offering the delete. Renaming a label is not free either — an invoice's property lines
// are rendered from the definitions at print time, so a rename changes what a re-generated
// historical invoice says. That is said once, in the banner, rather than discovered.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import type { Ctx } from "../lib/types";
import {
  badge,
  banner,
  btn,
  card,
  emptyState,
  field,
  iconBtn,
  input,
  pageHead,
  resource,
  textarea,
  withBusy,
} from "../ui/kit";
import { bindForm, openDialog } from "../ui/dialog";

export const meta = { title: "Service properties", crumb: "Service properties" };

// ---------------------------------------------------------------- payloads --

/** One definition, from `propertyDefinitionJSON` in internal/httpapi/service_metadata.go. */
interface PropertyDefinition {
  key: string;
  label: string;
  description: string;
  show_on_invoice: boolean;
  customer_visible: boolean;
  sort_order: number;
  created_at?: string;
  updated_at?: string;
}

interface DefinitionList {
  properties: PropertyDefinition[];
}

/** How many services carry each definition, from `handlePropertyUsage`. */
interface UsageList {
  data: Array<{ key: string; services: number }>;
}

/** propertyHint is the sentence that explains the two flags, for one definition.
 *
 *  It is copied verbatim from `propertyHint` in web/src/views/service.ts: the same sentence has
 *  to mean the same thing on the screen that reads a property and the screen that defines it, and
 *  two phrasings for one rule is how an operator learns the wrong one. */
function propertyHint(def: Pick<PropertyDefinition, "show_on_invoice" | "customer_visible">): string {
  if (def.show_on_invoice && def.customer_visible) {
    return "Shown in the customer's portal and printed on invoices.";
  }
  if (def.show_on_invoice) {
    return "Printed on invoices but hidden from the customer's portal — check that is intended.";
  }
  if (def.customer_visible) {
    return "Shown in the customer's portal, not printed on invoices.";
  }
  return "Internal only: neither printed nor shown to the customer.";
}

export default async function servicePropertiesPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("service:write:all")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Service properties" }),
      banner(
        "danger",
        "You cannot configure service properties",
        "This screen needs the service:write:all permission: a property definition decides what every service may carry and what prints on a customer's invoice.",
        btn("Back to the dashboard", { href: "/dashboard", size: "sm", variant: "ghost" }),
      ),
    );
  }

  const load = async (signal: AbortSignal) => {
    const [definitions, usage] = await Promise.all([
      ctx.api.get<DefinitionList>("/api/v1/staff/service-properties", { cache: false, signal }),
      ctx.api.get<UsageList>("/api/v1/staff/service-properties/usage", { cache: false, signal }),
    ]);
    const counts = new Map<string, number>();
    for (const row of usage.data ?? []) counts.set(row.key, row.services);
    return { definitions: definitions.properties ?? [], counts };
  };

  // The definitions the screen is showing, kept for the header button and for the new row's
  // place in the order.
  let shown: PropertyDefinition[] = [];

  const host = h("div");

  /** reload re-reads the catalogue and the usage counts.
   *
   *  Both, always: a create adds a row to each, and a delete changes both. A screen that refreshed
   *  one and not the other would show "no services" beside a key three services carry. */
  const reload = () => {
    swap(
      host,
      resource(ctx, load, (data) => {
        shown = data.definitions;
        return render(data.definitions, data.counts);
      }),
    );
  };

  const render = (definitions: PropertyDefinition[], counts: Map<string, number>): Node => {
    if (definitions.length === 0) {
      return card({
        flush: true,
        body: emptyState({
          title: "No service properties are defined",
          body: "A property is a fact about a service that does not fit the catalog: an asset tag, a rack, a control-panel URL. Define one here and it becomes available on every service.",
          icon: "layers",
          action: btn("Define the first property", {
            variant: "primary",
            icon: "plus",
            onclick: () => definitionDialog(ctx, null, [], reload),
          }),
        }),
      });
    }

    const rows = definitions.map((def, index) =>
      definitionRow(ctx, def, definitions, index, counts.get(def.key) ?? 0, reload),
    );

    return card({
      title: "Defined properties",
      sub: `${definitions.length} defined`,
      flush: true,
      body: h(
        // A row is five cells wide — the property, where it appears, how many services use it, its
        // order and its actions — and at 390px the actions column sat past the right edge of the
        // viewport, reachable only by scrolling the table sideways. `table-wrap--cards` rebuilds
        // the row as a card below 780px (tables.css) and `data-label` keeps each column's name.
        "div",
        { class: "table-wrap table-wrap--cards" },
        h(
          "table",
          { class: "table" },
          h(
            "thead",
            null,
            h(
              "tr",
              null,
              h("th", { scope: "col" }, "Property"),
              h("th", { scope: "col" }, "Where it appears"),
              h("th", { scope: "col" }, "Used by"),
              h("th", { scope: "col" }, "Order"),
              h("th", { scope: "col", class: "num" }, ""),
            ),
          ),
          h("tbody", null, ...rows),
        ),
      ),
    });
  };

  reload();

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Service properties",
      sub: "The keys a service may carry, in the order they are shown and printed. The values are set on the service itself.",
      actions: btn("New property", {
        variant: "primary",
        icon: "plus",
        onclick: () => definitionDialog(ctx, null, shown, reload),
      }),
    }),
    banner(
      "info",
      "Two flags, two different questions",
      "customer_visible decides whether the customer's own page shows the value. show_on_invoice decides whether it prints on an invoice or credit note. A property can be one without the other — and one combination is almost always a mistake, so every row says which it is.",
    ),
    banner(
      "warning",
      "A rename reaches the documents",
      "An invoice's property lines are rendered from these definitions when the document is produced, so changing a label changes what a re-generated historical invoice says. The value is the service's; the words around it are this catalogue's.",
    ),
    host,
  );
}

// -------------------------------------------------------------------- row --

function definitionRow(
  ctx: Ctx,
  def: PropertyDefinition,
  all: PropertyDefinition[],
  index: number,
  services: number,
  refresh: () => void,
): HTMLElement {
  /** move sends the whole order with one swap applied. */
  const move = (direction: -1 | 1, button: HTMLButtonElement) => {
    const target = index + direction;
    if (target < 0 || target >= all.length) return;
    const order = all.map((d) => d.key);
    const here = order[index];
    const there = order[target];
    if (here === undefined || there === undefined) return;
    order[index] = there;
    order[target] = here;
    void withBusy(ctx, button, async () => {
      await ctx.api.post("/api/v1/staff/service-properties/reorder", { keys: order });
      ctx.toast("Order saved", { tone: "success" });
      refresh();
    });
  };

  const remove = () => {
    if (services > 0) {
      // The database refuses this, and it is right to: the value would become unrenderable. The
      // refusal is explained here rather than thrown, because the fix (clear the value on each
      // service, or rename this definition instead) is not obvious from a 409.
      ctx.toast(`${def.label} is still in use`, {
        tone: "danger",
        detail: `${services} ${services === 1 ? "service carries" : "services carry"} this key. Clear the value on each service, or rename the definition instead of deleting it.`,
      });
      return;
    }
    void (async () => {
      const ok = await ctx.confirm({
        title: `Delete the definition “${def.label}”?`,
        body: `No service carries ${def.key} today. Deleting the definition removes the key from the catalogue, so it can no longer be set on any service. A value added between now and the delete still blocks it.`,
        confirmLabel: "Delete the definition",
        danger: true,
      });
      if (!ok) return;
      try {
        await ctx.api.del(`/api/v1/staff/service-properties/${encodeURIComponent(def.key)}`);
        ctx.toast("Definition deleted", { tone: "success", detail: def.key });
        refresh();
      } catch (err) {
        if (err instanceof ApiError && err.code === "property_in_use") {
          ctx.toast("A service picked up this key", {
            tone: "danger",
            detail: "Reload the page: the usage count has changed since it was read.",
          });
          refresh();
          return;
        }
        ctx.toast(
          err instanceof ApiError ? err.detail || "The definition could not be deleted" : "The definition could not be deleted",
          { tone: "danger", detail: err instanceof ApiError ? err.code : undefined },
        );
      }
    })();
  };

  return h(
    "tr",
    null,
    h(
      "td",
      { class: "cell-primary" },
      h(
        "div",
        { class: "cell-main" },
        h("div", { class: "primary" }, def.label),
        h("div", { class: "secondary mono" }, def.key),
        def.description ? h("div", { class: "secondary" }, def.description) : null,
      ),
    ),
    h(
      "td",
      // Two badges and a sentence: beside a column label the sentence would be a column two words
      // wide. `cell-block` stacks the label above the value in card mode (pages.css).
      { class: "cell-block", "data-label": "Where it appears" },
      h(
        "div",
        { class: "stack-sm" },
        h(
          "span",
          { class: "row" },
          badge(def.customer_visible ? "Portal" : "Hidden", def.customer_visible ? "info" : "neutral", { dot: true }),
          badge(def.show_on_invoice ? "Invoiced" : "Not invoiced", def.show_on_invoice ? "accent" : "neutral", { dot: true }),
        ),
        h("span", { class: "field-hint" }, propertyHint(def)),
      ),
    ),
    h(
      "td",
      { "data-label": "Used by" },
      services > 0
        ? h("span", { class: "nowrap" }, `${services} ${services === 1 ? "service" : "services"}`)
        : h("span", { class: "faint nowrap" }, "none"),
    ),
    h(
      "td",
      { "data-label": "Order" },
      h(
        "div",
        { class: "row" },
        iconBtn("chevronUp", `Move ${def.label} up`, (event) => move(-1, event.currentTarget as HTMLButtonElement), { size: "sm" }),
        iconBtn("chevronDown", `Move ${def.label} down`, (event) => move(1, event.currentTarget as HTMLButtonElement), { size: "sm" }),
        h("span", { class: "mono faint" }, String(def.sort_order)),
      ),
    ),
    h(
      "td",
      // The row's actions: in card mode this is a block under the facts, right aligned, with no
      // column label (tables.css already styles `td.cell-actions` that way).
      { class: "num cell-actions" },
      h(
        "div",
        { class: "row" },
        btn("Edit", {
          size: "sm",
          variant: "ghost",
          icon: "edit",
          onclick: () => definitionDialog(ctx, def, all, refresh),
        }),
        btn("Delete", {
          size: "sm",
          variant: "ghost",
          icon: "trash",
          title: services > 0 ? "In use: the delete would be refused" : "Delete this definition",
          onclick: remove,
        }),
      ),
    ),
  );
}

// ----------------------------------------------------------------- dialog --

/** definitionDialog creates or edits one definition.
 *
 *  The key is the identity: every service's `custom_properties` object is written against it, so
 *  re-keying a definition would orphan every stored value. It is therefore fixed on an edit and
 *  the hint says why. */
function definitionDialog(
  ctx: Ctx,
  existing: PropertyDefinition | null,
  all: PropertyDefinition[],
  refresh: () => void,
): void {
  const isEdit = existing !== null;

  const key = input({
    id: "prop-key",
    value: existing?.key ?? "",
    placeholder: "asset_tag",
    class: "input--mono",
    maxlength: 64,
    autocomplete: "off",
  });
  if (isEdit) key.disabled = true;

  const label = input({
    id: "prop-label",
    value: existing?.label ?? "",
    placeholder: "Asset tag",
    maxlength: 120,
  });
  const description = textarea({
    id: "prop-description",
    value: existing?.description ?? "",
    maxlength: 500,
    placeholder: "What this property records, in one sentence.",
  });
  description.rows = 3;

  const onInvoice = h("input", { type: "checkbox", checked: existing?.show_on_invoice ?? false });
  const customerVisible = h("input", { type: "checkbox", checked: existing?.customer_visible ?? true });

  // The same sentence the row shows, updated as the switches move: the flags are what an
  // operator gets wrong, so the consequence is visible while the choice is being made.
  const hint = h("p", { class: "field-hint" });
  const paintHint = () => {
    hint.textContent = propertyHint({
      show_on_invoice: onInvoice.checked,
      customer_visible: customerVisible.checked,
    });
  };
  onInvoice.addEventListener("change", paintHint);
  customerVisible.addEventListener("change", paintHint);
  paintHint();

  const errorHost = h("div");
  const form = h(
    "form",
    { class: "form-grid", novalidate: true },
    h(
      "div",
      { class: "span-2" },
      field({
        label: "Key",
        control: key,
        required: true,
        hint: isEdit
          ? "The key is what every service's value is stored against, so it cannot be renamed. Rename the label instead."
          : "Lowercase letters, digits and underscores, starting with a letter. It is the JSON key, so it is written once and never again.",
      }),
    ),
    h("div", { class: "span-2" }, field({ label: "Label", control: label, required: true, hint: "What a person reads. Never the key." })),
    h("div", { class: "span-2" }, field({ label: "Description", control: description, hint: "Optional. Shown beside the field on the service screen." })),
    h("div", { class: "span-2" }, h("label", { class: "switch" }, onInvoice, "Print this property on invoices and credit notes")),
    h("div", { class: "span-2" }, h("label", { class: "switch" }, customerVisible, "Show this property in the customer's portal")),
    h("div", { class: "span-2" }, hint),
  );

  const submit = btn(isEdit ? "Save definition" : "Create definition", {
    variant: "primary",
    type: "submit",
  });
  const cancel = btn("Cancel", { variant: "ghost" });
  bindForm(form, submit);

  const handle = openDialog({
    title: isEdit ? `Edit ${existing.label}` : "New service property",
    subtitle: "The definition decides what a service may carry and where the value appears. The value itself is set on each service.",
    body: h("div", { class: "stack" }, errorHost, form),
    footer: h("div", { class: "row spacer" }, cancel, submit),
    wide: true,
    locked: () => submit instanceof HTMLButtonElement && submit.disabled,
  });
  cancel.addEventListener("click", () => handle.close());

  form.addEventListener("submit", (event) => {
    event.preventDefault();
    void (async () => {
      errorHost.replaceChildren();
      const trimmedKey = (existing?.key ?? key.value).trim();
      if (!isEdit && !/^[a-z][a-z0-9_]{0,63}$/.test(trimmedKey)) {
        swap(
          errorHost,
          banner(
            "danger",
            "That key is not valid",
            "A key is lowercase letters, digits and underscores, starting with a letter, at most 64 characters.",
          ),
        );
        return;
      }
      if (!label.value.trim()) {
        swap(errorHost, banner("danger", "A property needs a label", "The label is what a person reads on the service and on the document."));
        return;
      }
      // sort_order is sent so an edit does not reset the position: the save replaces the row's
      // order, and the order is owned by the arrows on the list, not by this form.
      const payload = {
        label: label.value.trim(),
        description: description.value.trim(),
        show_on_invoice: onInvoice.checked,
        customer_visible: customerVisible.checked,
        sort_order: existing?.sort_order ?? nextSortOrder(all),
      };

      if (submit instanceof HTMLButtonElement) submit.disabled = true;
      try {
        await ctx.api.put(`/api/v1/staff/service-properties/${encodeURIComponent(trimmedKey)}`, payload);
        ctx.toast(isEdit ? "Definition saved" : "Definition created", {
          tone: "success",
          detail: `${label.value.trim()} · ${propertyHint({ show_on_invoice: onInvoice.checked, customer_visible: customerVisible.checked })}`,
        });
        handle.close();
        // The catalogue and the usage count are both re-read: a create adds a row to each.
        refresh();
      } catch (err) {
        swap(errorHost, saveError(err));
      } finally {
        if (submit instanceof HTMLButtonElement) submit.disabled = false;
      }
    })();
  });
}

/** nextSortOrder puts a new definition at the end of the list, leaving gaps for later inserts. */
function nextSortOrder(all: PropertyDefinition[]): number {
  let max = 0;
  for (const def of all) max = Math.max(max, def.sort_order);
  return max + 10;
}

/** saveError turns a refused definition into a banner with a reason to act on. */
function saveError(err: unknown): HTMLElement {
  if (!(err instanceof ApiError)) {
    return banner("danger", "The definition was not saved", "The server could not be reached. Nothing was changed.");
  }
  switch (err.code) {
    case "invalid_label":
      return banner("danger", "That label is not acceptable", err.detail || "A label is 1–120 characters.");
    case "invalid_input":
      return banner("danger", "The definition was refused", err.detail || "Check the key and the description.");
    case "forbidden":
      return banner("danger", "You do not hold service:write:all", "The definition was not saved.");
    default:
      return banner("danger", "The definition was not saved", err.detail || `The server refused it (${err.code}).`);
  }
}
