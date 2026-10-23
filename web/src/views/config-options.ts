// Config options.
//
// A config option is a choice attached to a product — "RAM", "Region", "Add-on" — and each
// choice has a price *per price version*. That last clause is the whole design of this screen:
//
//  - A price version is immutable, so an option's price is not one number on the option. It is
//    one amount per (choice, price version). The editor therefore shows a matrix: the product's
//    price versions down one side, the option's choices across the other, and an amount in each
//    cell. A price change is a new version, which adds a column, not an edit.
//  - The option itself is catalog configuration, but it needs `config:write` rather than
//    `catalog:write`, because changing an option changes what every subscription on that product
//    renews at.
//
// The screen is two steps: a product picker, then the options for it. The list endpoint is
// `GET /api/v1/staff/config-options?product_id=…` and answers `{options, types}`, so there is no
// cursor to page: one product's options are the whole list. The package that owns the endpoint
// is still landing, so a 404 renders an explanation rather than an error — that is a state of
// the installation, not a failure of the request.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import type { Amount } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  amountFromInput,
  badge,
  banner,
  btn,
  card,
  emptyState,
  errorState,
  field,
  input,
  pageHead,
  resource,
  select,
  skeletonPage,
  textarea,
} from "../ui/kit";
import { bindForm, openDialog } from "../ui/dialog";

export const meta = { title: "Config options", crumb: "Config options" };

// ---------------------------------------------------------------- payloads --

/** One per-price-version amount, from `configOptionJSON` in internal/httpapi/service_config.go.
 *  `amount_minor`/`setup_minor` are raw minor units in the price version's currency. */
interface OptionPrice {
  id: string;
  price_version_id: string;
  choice: string;
  amount_minor: number;
  setup_minor: number;
}

/** One option, from `configOptionJSON`. `prices` is always an array. */
interface ConfigOption {
  id: string;
  product_id: string;
  parent_id?: string;
  name: string;
  description: string;
  type: string;
  choices: string[] | null;
  env_variable: string;
  required: boolean;
  hidden: boolean;
  upgradable: boolean;
  sort_order: number;
  created_at: string;
  updated_at: string;
  prices: OptionPrice[] | null;
}

/** The list response: `{options, types}`. `types` is the closed set the database CHECK allows,
 *  served by the handler so the form cannot offer a type the server will refuse. */
interface OptionsResponse {
  options: ConfigOption[] | null;
  types: string[] | null;
}

/** The staff catalog product, narrowed to what this screen needs: its price versions. */
interface CatalogProduct {
  id: string;
  slug: string;
  name: string;
  hidden: boolean;
  prices: Array<{ id: string; version: number; name: string; currency: string; amount: Amount; setup: Amount }>;
}

const TYPE_LABELS: Record<string, string> = {
  dropdown: "Dropdown",
  radio: "Radio buttons",
  checkbox: "Checkbox",
  text: "Free text",
  number: "Number",
};

/** Types that take a list of choices. A checkbox or free-text option has none. */
const CHOICE_TYPES = new Set(["dropdown", "radio"]);

export default async function configOptionsPage(ctx: Ctx): Promise<Node> {
  const canWrite = ctx.can("config:write");

  // The product is chosen in the URL so a refresh keeps the screen, and so a link to "the
  // options for this product" is a real link.
  let productId = ctx.route.query.get("product_id") ?? "";

  const host = h("div");

  // The picker is loaded once; the options are reloaded per product.
  const loadProducts = async (signal: AbortSignal): Promise<CatalogProduct[]> => {
    const res = await ctx.api.get<{ data: CatalogProduct[] }>("/api/v1/staff/catalog/products", {
      cache: false,
      signal,
    });
    return res.data ?? [];
  };

  let products: CatalogProduct[] = [];
  let productProblem = "";

  const paint = (): void => {
    const selected = products.find((p) => p.id === productId) ?? null;
    swap(
      host,
      h(
        "div",
        { class: "stack" },
        pickerCard(),
        productProblem
          ? banner(
              "warning",
              "The catalog could not be read",
              h("span", null, productProblem, " The options below cannot be shown without a product to show them for."),
            )
          : null,
        !selected
          ? card({
              flush: true,
              body: emptyState({
                title: products.length === 0 ? "No products to configure" : "Choose a product",
                body:
                  products.length === 0
                    ? "A config option belongs to a product, and the catalog is empty. Create a product and a price first."
                    : "Options are per product. Pick one above to see and edit the choices a customer can make when buying it.",
                icon: "sliders",
                action: products.length === 0 ? btn("Open the catalog", { href: "/catalog-management", size: "sm", icon: "box" }) : null,
              }),
            })
          : optionsSection(selected),
      ),
    );
  };

  const pickerCard = (): HTMLElement => {
    const picker = select(
      [
        { value: "", label: products.length === 0 ? "No products in the catalog" : "Choose a product…" },
        ...products.map((p) => ({
          value: p.id,
          label: `${p.name}${p.hidden ? " (hidden)" : ""} · ${p.prices.length} price${p.prices.length === 1 ? "" : "s"}`,
        })),
      ],
      { id: "config-product", value: productId },
    );
    picker.addEventListener("change", () => {
      productId = picker.value;
      ctx.replaceQuery({ product_id: productId || null });
      paint();
    });

    return card({
      title: "Product",
      body: h(
        "div",
        { class: "stack-sm" },
        h("div", { class: "config-picker" }, field({ label: "Product", control: picker, id: "config-product" })),
        h(
          "p",
          { class: "field-hint" },
          "An option's price is set per price version, so a product with no price version has nothing to price against. Add a price in the catalog first.",
        ),
      ),
    });
  };

  // -------------------------------------------------------------- options --

  const optionsSection = (product: CatalogProduct): HTMLElement => {
    const section = h("div", { class: "stack" });

    const load = async (signal: AbortSignal): Promise<OptionsResponse | "unavailable"> => {
      try {
        return await ctx.api.get<OptionsResponse>("/api/v1/staff/config-options", {
          query: { product_id: product.id },
          cache: false,
          signal,
        });
      } catch (err) {
        // The endpoint belongs to the service-lifecycle package. A 404 means this build does
        // not serve it yet, which is a property of the installation, not of this request.
        if (err instanceof ApiError && (err.status === 404 || err.status === 501)) return "unavailable";
        throw err;
      }
    };

    const body = resource(
      ctx,
      load,
      (data) => {
        if (data === "unavailable") return unavailableCard(product);
        const options = data.options ?? [];
        const types = data.types ?? ["dropdown", "radio", "checkbox", "text", "number"];
        return optionsList(product, options, types);
      },
      { skeleton: skeletonPage },
    );

    swap(section, body);
    return section;
  };

  /** What a missing endpoint looks like. It is not an error: retrying produces the same 404, so
   *  the card says what is missing and what still works. */
  const unavailableCard = (product: CatalogProduct): HTMLElement =>
    card({
      title: "Config options are not available on this installation",
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "p",
          { class: "field-hint" },
          `This build does not serve /api/v1/staff/config-options, so the options for ${product.name} cannot be read or changed. The catalog, the prices and every other screen still work.`,
        ),
        h(
          "div",
          { class: "row" },
          btn("Open the product", { href: `/catalog-management/${product.id}`, size: "sm", icon: "box" }),
          btn("Open the catalog", { href: "/catalog-management", size: "sm", variant: "ghost" }),
        ),
      ),
    });

  const optionsList = (product: CatalogProduct, options: ConfigOption[], types: string[]): HTMLElement => {
    const priceVersions = product.prices ?? [];
    const header = h(
      "div",
      { class: "page-actions" },
      canWrite
        ? btn("New option", { variant: "primary", icon: "plus", onclick: () => optionDialog(product, null, types) })
        : null,
    );

    const intro = card({
      title: product.name,
      sub: `${options.length} option${options.length === 1 ? "" : "s"} · ${priceVersions.length} price version${priceVersions.length === 1 ? "" : "s"}`,
      actions: h(
        "div",
        { class: "row" },
        h("a", { class: "linkish", href: `/catalog-management/${product.id}` }, "Open product", icon("chevronRight", 12)),
        header,
      ),
      body:
        priceVersions.length === 0
          ? h(
              "div",
              { class: "banner banner--warning" },
              icon("alertTriangle", 15),
              h(
                "div",
                { class: "stack-sm" },
                h("strong", null, "This product has no price version"),
                h(
                  "span",
                  null,
                  "An option's amount is per price version, so there is nothing to attach a price to. Add a price in the catalog first.",
                ),
              ),
              btn("Open the product", { href: `/catalog-management/${product.id}`, size: "sm" }),
            )
          : h(
              "p",
              { class: "field-hint" },
              "Each option prices every choice on every price version. A renewal reads the same amounts, so a quote and a renewal cannot disagree.",
            ),
    });

    if (options.length === 0) {
      return h(
        "div",
        { class: "stack" },
        intro,
        card({
          flush: true,
          body: emptyState({
            title: "No options on this product",
            body: "An option is a choice a customer makes when buying: a region, a size, an add-on. Without one, the product is sold exactly as its price version describes it.",
            icon: "sliders",
            action: canWrite ? btn("Add the first option", { variant: "primary", icon: "plus", onclick: () => optionDialog(product, null, types) }) : null,
          }),
        }),
      );
    }

    return h(
      "div",
      { class: "stack" },
      intro,
      ...options.map((option) => optionCard(product, option, options, types)),
    );
  };

  const optionCard = (product: CatalogProduct, option: ConfigOption, all: ConfigOption[], types: string[]): HTMLElement => {
    const choices = option.choices ?? [];
    const priceVersions = product.prices ?? [];
    const priced = new Map<string, OptionPrice>();
    for (const price of option.prices ?? []) {
      priced.set(`${price.price_version_id}::${price.choice}`, price);
    }
    // A choice-less option (checkbox, free text) has one implicit "selected" price line, keyed
    // by an empty choice. Showing it as a single row keeps the matrix honest rather than empty.
    const rows = choices.length > 0 ? choices : [""];

    // The matrix is choices across and price versions down: it keeps its table shape at every
    // width (a card per row would break the reading across) and scrolls inside the card when
    // there are more versions than fit.
    const table = h(
      "div",
      { class: "config-matrix table-wrap" },
      h(
        "table",
        { class: "table config-table" },
        h(
          "thead",
          null,
          h(
            "tr",
            null,
            h("th", { scope: "col" }, "Choice"),
            ...priceVersions.map((version) =>
              h(
                "th",
                { scope: "col", class: "num" },
                h("div", { class: "config-version-head" }, h("span", null, version.name || `v${version.version}`), h("span", { class: "faint mono" }, version.currency)),
              ),
            ),
          ),
        ),
        h(
          "tbody",
          null,
          ...rows.map((choice) =>
            h(
              "tr",
              null,
              h(
                "td",
                null,
                choice
                  ? h("span", { class: "mono" }, choice)
                  : h("span", { class: "faint" }, "Selected (no choice)"),
              ),
              ...priceVersions.map((version) => {
                const hit = priced.get(`${version.id}::${choice}`);
                return h(
                  "td",
                  { class: "num" },
                  canWrite
                    ? h(
                        "button",
                        {
                          class: `config-amount${hit ? "" : " config-amount--unset"}`,
                          type: "button",
                          title: hit ? "Change this amount" : "Set an amount for this choice",
                          onclick: () =>
                            amountDialog(product, option, version, choice, hit ?? null),
                        },
                        hit ? moneyElMinor(hit.amount_minor, version.currency) : "+ Set",
                        // `moneyElMinor` returns an element, not a string: interpolating it into a
                        // template literal renders "+ [object HTMLSpanElement] setup". The pieces
                        // are children of the setup line instead.
                        hit && hit.setup_minor > 0
                          ? h(
                              "span",
                              { class: "config-amount-setup" },
                              "+ ",
                              moneyElMinor(hit.setup_minor, version.currency),
                              " setup",
                            )
                          : null,
                      )
                    : hit
                      ? moneyElMinor(hit.amount_minor, version.currency)
                      : h("span", { class: "faint" }, "—"),
                );
              }),
            ),
          ),
        ),
      ),
    );

    return card({
      title: h(
        "span",
        { class: "row" },
        option.name || "Untitled option",
        option.required ? badge("Required", "info") : null,
        option.hidden ? badge("Hidden", "neutral", { dot: true }) : null,
        option.upgradable ? badge("Upgradable", "accent") : null,
      ),
      sub: `${TYPE_LABELS[option.type] ?? option.type}${option.env_variable ? ` · ${option.env_variable}` : ""}`,
      actions: canWrite
        ? h(
            "div",
            { class: "row" },
            btn("Edit", { size: "sm", variant: "ghost", icon: "edit", onclick: () => optionDialog(product, option, types) }),
            btn("Delete", {
              size: "sm",
              variant: "ghost",
              icon: "trash",
              title: "Remove an option no live service has selected",
              onclick: () => void deleteOption(option),
            }),
          )
        : null,
      flush: true,
      body: h(
        "div",
        { class: "stack-sm config-option-body" },
        option.description ? h("p", { class: "config-option-desc" }, option.description) : null,
        priceVersions.length === 0
          ? h("p", { class: "field-hint config-option-note" }, "No price versions to price against yet.")
          : table,
        option.parent_id
          ? h(
              "p",
              { class: "field-hint config-option-note" },
              "This option is nested under ",
              h(
                "span",
                { class: "mono" },
                all.find((candidate) => candidate.id === option.parent_id)?.name ?? "another option",
              ),
              ".",
            )
          : null,
      ),
    });
  };

  // -------------------------------------------------------------- dialogs --

  const amountDialog = (
    product: CatalogProduct,
    option: ConfigOption,
    version: { id: string; name: string; version: number; currency: string },
    choice: string,
    existing: OptionPrice | null,
  ): void => {
    const amount = input({
      id: "opt-amount",
      value: existing ? majorOf(existing.amount_minor, version.currency) : "",
      placeholder: "0.00",
      class: "input--mono",
      inputmode: "decimal",
      required: true,
    });
    const setup = input({
      id: "opt-setup",
      value: existing && existing.setup_minor > 0 ? majorOf(existing.setup_minor, version.currency) : "",
      placeholder: "0.00",
      class: "input--mono",
      inputmode: "decimal",
    });
    const errorHost = h("div");
    const submit = btn("Save amount", { variant: "primary", type: "submit" });
    const cancel = btn("Cancel", { variant: "ghost" });
    const form = h(
      "form",
      { class: "stack", novalidate: true },
      errorHost,
      h(
        "div",
        { class: "form-grid" },
        field({
          label: "Amount",
          control: amount,
          id: "opt-amount",
          required: true,
          hint: `Added to the price of ${version.name || `v${version.version}`}, in ${version.currency}. Major units, e.g. 5.00.`,
        }),
        field({ label: "Setup fee", control: setup, id: "opt-setup", hint: "One-off, charged once." }),
      ),
      existing
        ? h(
            "div",
            { class: "row" },
            btn("Remove this amount", {
              variant: "danger",
              size: "sm",
              icon: "trash",
              onclick: () => void removeAmount(),
            }),
          )
        : null,
    );
    bindForm(form, submit);

    const handle = openDialog({
      title: `${option.name || "Option"} · ${choice || "selected"}`,
      subtitle: `${product.name} — ${version.name || `version ${version.version}`} (${version.currency}). A price version is immutable, so this amount belongs to that version alone.`,
      body: h("div", { class: "stack" }, form),
      footer: h("div", { class: "row spacer" }, cancel, submit),
      locked: () => submit instanceof HTMLButtonElement && submit.disabled,
    });
    cancel.addEventListener("click", () => handle.close());

    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void (async () => {
        errorHost.replaceChildren();
        const scale = currencyScale(version.currency);
        const amountMinor = amountFromInput(amount.value, scale);
        if (amountMinor === null) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "The amount must be a number, e.g. 5.00."));
          return;
        }
        const setupMinor = setup.value.trim() === "" ? 0 : amountFromInput(setup.value, scale);
        if (setupMinor === null) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "The setup fee must be a number, e.g. 0.00."));
          return;
        }
        if (submit instanceof HTMLButtonElement) submit.disabled = true;
        try {
          await ctx.api.put(`/api/v1/staff/config-options/${encodeURIComponent(option.id)}/prices`, {
            price_version_id: version.id,
            choice,
            amount_minor: amountMinor,
            setup_minor: setupMinor,
          });
          ctx.toast("Amount saved", {
            tone: "success",
            detail: `${option.name} · ${choice || "selected"} · ${version.currency}`,
          });
          handle.close();
          paint();
        } catch (err) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, configError(err)));
        } finally {
          if (submit instanceof HTMLButtonElement) submit.disabled = false;
        }
      })();
    });

    async function removeAmount(): Promise<void> {
      try {
        await ctx.api.del(`/api/v1/staff/config-options/${encodeURIComponent(option.id)}/prices`, {
          price_version_id: version.id,
          choice,
        });
        ctx.toast("Amount removed", { tone: "success", detail: "This choice is now free on this version." });
        handle.close();
        paint();
      } catch (err) {
        ctx.toast(err instanceof ApiError ? err.detail || "The amount could not be removed" : "The amount could not be removed", {
          tone: "danger",
          detail: err instanceof ApiError ? err.code : undefined,
        });
      }
    }
  };

  const optionDialog = (product: CatalogProduct, existing: ConfigOption | null, types: string[]): void => {
    const isEdit = existing !== null;
    const name = input({
      id: "opt-name",
      value: existing?.name ?? "",
      placeholder: "Region",
      maxlength: 120,
      required: true,
    });
    const description = textarea({
      id: "opt-desc",
      value: existing?.description ?? "",
      placeholder: "Where the server runs. Latency and data residency depend on it.",
      maxlength: 500,
    });
    const type = select(
      types.map((value) => ({ value, label: TYPE_LABELS[value] ?? value })),
      { id: "opt-type", value: existing?.type ?? types[0] ?? "dropdown" },
    );
    const choices = textarea({
      id: "opt-choices",
      value: (existing?.choices ?? []).join("\n"),
      placeholder: "eu-west\nus-east\nap-south",
      maxlength: 2000,
    });
    const envVariable = input({
      id: "opt-env",
      value: existing?.env_variable ?? "",
      placeholder: "REGION",
      class: "input--mono",
      maxlength: 64,
    });
    const sortOrder = input({ id: "opt-sort", type: "number", value: existing?.sort_order ?? 0, step: 1 });
    const required = h("input", { type: "checkbox", checked: existing?.required ?? false });
    const hidden = h("input", { type: "checkbox", checked: existing?.hidden ?? false });
    const upgradable = h("input", { type: "checkbox", checked: existing?.upgradable ?? false });

    const choicesField = field({
      label: "Choices",
      control: choices,
      id: "opt-choices",
      class: "span-2",
      hint: "One per line. Required for a dropdown or radio option; ignored by the others.",
    });
    const syncChoices = () => {
      choicesField.hidden = !CHOICE_TYPES.has(type.value);
    };
    type.addEventListener("change", syncChoices);
    syncChoices();

    const errorHost = h("div");
    const body = h(
      "div",
      { class: "form-grid" },
      field({ label: "Name", control: name, id: "opt-name", required: true, class: "span-2" }),
      field({ label: "Description", control: description, id: "opt-desc", class: "span-2" }),
      field({ label: "Type", control: type, id: "opt-type", hint: "What the customer sees. A checkbox has no choices." }),
      field({ label: "Environment variable", control: envVariable, id: "opt-env", hint: "Passed to the provisioning extension." }),
      choicesField,
      field({ label: "Sort order", control: sortOrder, id: "opt-sort", hint: "Lower appears first." }),
      h("div", { class: "span-2 stack-sm" },
        h("label", { class: "switch" }, required, "Required — the customer must choose"),
        h("label", { class: "switch" }, upgradable, "Upgradable — the customer may change it later, prorated"),
        h("label", { class: "switch" }, hidden, "Hidden — kept for existing services, not offered on new orders"),
      ),
    );

    const submit = btn(isEdit ? "Save option" : "Create option", { variant: "primary", type: "submit" });
    const cancel = btn("Cancel", { variant: "ghost" });
    const form = h("form", { class: "stack", novalidate: true }, errorHost, body);
    bindForm(form, submit);

    const handle = openDialog({
      title: isEdit ? `Edit ${existing.name}` : `New option on ${product.name}`,
      subtitle: "The option defines the choices. Its amounts are set per price version, on the matrix, after it exists.",
      body: h("div", { class: "stack" }, form),
      footer: h("div", { class: "row spacer" }, cancel, submit),
      wide: true,
      locked: () => submit instanceof HTMLButtonElement && submit.disabled,
    });
    cancel.addEventListener("click", () => handle.close());

    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void (async () => {
        errorHost.replaceChildren();
        if (!name.value.trim()) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "An option needs a name."));
          return;
        }
        const choiceList = choices.value
          .split("\n")
          .map((line) => line.trim())
          .filter((line) => line !== "");
        if (CHOICE_TYPES.has(type.value) && choiceList.length === 0) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "A dropdown or radio option needs at least one choice."));
          return;
        }
        if (new Set(choiceList).size !== choiceList.length) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "A choice is listed twice. Each choice must be unique."));
          return;
        }
        const payload: Record<string, unknown> = {
          product_id: product.id,
          name: name.value.trim(),
          description: description.value.trim(),
          type: type.value,
          // Choices are only meaningful for the types that show them; sending them for a
          // checkbox is ignored by the server rather than refused.
          choices: CHOICE_TYPES.has(type.value) ? choiceList : [],
          env_variable: envVariable.value.trim(),
          required: required.checked,
          hidden: hidden.checked,
          upgradable: upgradable.checked,
          sort_order: Number(sortOrder.value) || 0,
        };
        if (isEdit && existing.parent_id) payload.parent_id = existing.parent_id;

        if (submit instanceof HTMLButtonElement) submit.disabled = true;
        try {
          if (isEdit) {
            await ctx.api.put(`/api/v1/staff/config-options/${encodeURIComponent(existing.id)}`, payload);
          } else {
            await ctx.api.post("/api/v1/staff/config-options", payload);
          }
          ctx.toast(isEdit ? "Option saved" : "Option created", {
            tone: "success",
            detail: isEdit ? undefined : "Set its amounts on the matrix.",
          });
          handle.close();
          paint();
        } catch (err) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, configError(err)));
        } finally {
          if (submit instanceof HTMLButtonElement) submit.disabled = false;
        }
      })();
    });
  };

  const deleteOption = async (option: ConfigOption): Promise<void> => {
    const ok = await ctx.confirm({
      title: `Delete “${option.name}”?`,
      body: "An option can only be removed when no live service has selected it. Its prices go with it. If a service still uses it, the server refuses and the option stays.",
      confirmLabel: "Delete option",
      danger: true,
    });
    if (!ok) return;
    try {
      await ctx.api.del(`/api/v1/staff/config-options/${encodeURIComponent(option.id)}`);
      ctx.toast("Option deleted", { tone: "success" });
      paint();
    } catch (err) {
      if (err instanceof ApiError) {
        ctx.toast(err.detail || "The option could not be deleted", { tone: "danger", detail: err.code });
        return;
      }
      ctx.toast("The option could not be deleted", { tone: "danger", detail: "The server could not be reached." });
    }
  };

  // ------------------------------------------------------------------ load --

  const controller = new AbortController();
  ctx.onDispose(() => controller.abort());
  const loadCatalog = async (): Promise<void> => {
    try {
      products = await loadProducts(controller.signal);
      // A product id in the URL that no longer exists is treated as "none chosen" rather than
      // an error, because a stale bookmark is not a failure.
      if (productId && !products.some((p) => p.id === productId)) productId = "";
      paint();
    } catch (err) {
      if (controller.signal.aborted) return;
      if (err instanceof ApiError && (err.status === 403 || err.status === 404)) {
        productProblem = "You do not have access to the catalog, so no product can be chosen here.";
        swap(host, h("div", { class: "stack" }, pickerCard(), banner("warning", "The catalog is not readable", productProblem)));
        return;
      }
      swap(host, errorState(err, () => void loadCatalog()));
    }
  };

  void loadCatalog();

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Config options",
      sub: "The choices a product offers, and what each one costs on each price version. A renewal reads the same amounts, so a quote and a renewal cannot disagree.",
      actions: canWrite ? null : badge("Read only", "neutral"),
    }),
    host,
  );
}

// ----------------------------------------------------------------- helpers --

/** An amount from raw minor units. The currency's scale comes from the money package's own
 *  rule for the code, which is the same rule the server used to store it. */
function moneyElMinor(minor: number, currency: string): HTMLElement {
  const scale = currencyScale(currency);
  return h("span", { class: "money mono" }, `${currency} ${formatMinorLocal(minor, scale)}`);
}

function formatMinorLocal(minor: number, scale: number): string {
  const negative = minor < 0;
  let digits = String(Math.abs(Math.trunc(minor)));
  if (scale > 0) {
    while (digits.length <= scale) digits = "0" + digits;
    const cut = digits.length - scale;
    digits = `${digits.slice(0, cut)}.${digits.slice(cut)}`;
  }
  const [whole = "0", fraction] = digits.split(".");
  const grouped = whole.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  return `${negative ? "-" : ""}${grouped}${fraction ? "." + fraction : ""}`;
}

/** Renders minor units as a major-unit string for an edit form, without floating point. */
function majorOf(minor: number, currency: string): string {
  const scale = currencyScale(currency);
  const digits = String(Math.abs(Math.trunc(minor))).padStart(scale + 1, "0");
  const cut = digits.length - scale;
  return `${minor < 0 ? "-" : ""}${digits.slice(0, cut)}${scale > 0 ? `.${digits.slice(cut)}` : ""}`;
}

/** The minor-unit exponent for a code. The money package's table is authoritative; these are
 *  the exceptions to the default of 2, which is what every other code uses. */
function currencyScale(code: string): number {
  switch (code.toUpperCase()) {
    case "JPY":
    case "KRW":
    case "VND":
    case "CLP":
    case "ISK":
    case "XOF":
    case "XAF":
      return 0;
    case "BHD":
    case "KWD":
    case "OMR":
    case "TND":
    case "JOD":
    case "IQD":
    case "LYD":
      return 3;
    default:
      return 2;
  }
}

function configError(err: unknown): string {
  if (!(err instanceof ApiError)) return "The server could not be reached, so nothing was saved.";
  switch (err.code) {
    case "invalid_input":
      return err.detail || "The option was refused. Check the name, the type and the choices.";
    case "not_found":
      return "That option or price version no longer exists. Reload the page.";
    case "option_not_available":
    case "option_not_upgradable":
      return err.detail || "The option cannot be changed in that way.";
    case "forbidden":
      return "You do not hold config:write, so nothing was saved.";
    default:
      return err.detail || `The option could not be saved (${err.code}).`;
  }
}
