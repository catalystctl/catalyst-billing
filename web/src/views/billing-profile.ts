// Billing details, shared by the staff customer screen and the customer portal.
//
// One form, two doors. The API has the same shape on both sides (`/portal/billing-profile`
// and `/staff/customers/{id}/billing-profile`), and the validation is the server's
// (`billing.ValidateProfile`). Duplicating the form for the two audiences is how the two
// drift — a customer entering an address and an operator entering the same address must
// produce the same record.
//
// The note at the bottom is not decoration: saving a profile does not change an issued
// invoice, and somebody will otherwise assume it does.

import type { Api } from "../lib/api";
import { h } from "../lib/dom";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { banner, btn, card, errorState, field, input, sectionTitle, select, skeletonLines } from "../ui/kit";

export interface BillingProfile {
  customer_id: string;
  legal_name: string;
  address_line1: string;
  address_line2: string;
  city: string;
  postal_code: string;
  region: string;
  country_code: string;
  tax_id: string;
}

const COUNTRIES = [
  "US", "GB", "CA", "AU", "NZ", "IE", "DE", "FR", "NL", "BE", "ES", "IT", "PT", "AT", "CH",
  "SE", "NO", "DK", "FI", "PL", "CZ", "SK", "HU", "RO", "BG", "GR", "HR", "SI", "EE", "LV",
  "LT", "LU", "MT", "CY", "JP", "SG", "HK", "KR", "IN", "BR", "MX", "AR", "CL", "CO", "ZA",
  "AE", "IL", "TR", "IS", "LI",
];

export interface ProfileFormOptions {
  /** The API path that reads and writes the profile. */
  path: string;
  /** Called after a successful save. */
  onSaved?: (profile: BillingProfile) => void;
  /** Read-only rendering, with an "Edit" button that switches to the form. */
  readOnly?: boolean;
  title?: string;
}

export function profileForm(ctx: Ctx, options: ProfileFormOptions): HTMLElement {
  const host = h("div", { class: "stack" }, skeletonLines(5));
  let editing = !options.readOnly;
  let current: BillingProfile | null = null;

  const load = async () => {
    try {
      current = await ctx.api.get<BillingProfile>(options.path, { cache: false });
      render();
    } catch (err) {
      host.replaceChildren(errorState(err, () => void load()));
    }
  };

  const render = () => {
    if (!current) return;
    if (!editing) {
      host.replaceChildren(view(current), editButton());
      return;
    }
    host.replaceChildren(form(current));
  };

  const editButton = () =>
    h(
      "div",
      { class: "row" },
      btn("Edit billing details", {
        icon: "edit",
        size: "sm",
        onclick: () => {
          editing = true;
          render();
        },
      }),
    );

  const view = (profile: BillingProfile) => {
    const lines = [
      profile.legal_name || "No legal name",
      profile.address_line1,
      profile.address_line2,
      [profile.city, profile.region, profile.postal_code].filter(Boolean).join(", "),
      profile.country_code,
    ].filter((line) => line && line.trim() !== "");
    return h(
      "div",
      { class: "stack" },
      h(
        "div",
        { class: "doc-party" },
        lines.length > 0 ? lines.join("\n") : "No billing address on file. Invoices will be issued without one.",
      ),
      h(
        "div",
        { class: "meta-grid" },
        meta("Tax ID", profile.tax_id || "Not set"),
        meta("Country", profile.country_code || "Not set"),
      ),
      h(
        "div",
        { class: "banner banner--info" },
        icon("info", 15),
        h(
          "span",
          null,
          h("strong", null, "This is input to the next invoice, not a rewrite of the last one."),
          " An issued invoice keeps the details it was issued with; correcting an address today changes what is billed from now on.",
        ),
      ),
    );
  };

  const form = (profile: BillingProfile) => {
    const values: Record<string, HTMLInputElement> = {};
    const make = (name: keyof BillingProfile, label: string, opts: { placeholder?: string; span?: boolean; hint?: string; mono?: boolean } = {}) => {
      const control = input({
        id: `profile-${name}`,
        value: profile[name] ?? "",
        placeholder: opts.placeholder,
        autocomplete: name === "legal_name" ? "organization" : "off",
        class: opts.mono ? "input--mono" : undefined,
      });
      values[name] = control;
      return field({ label, control, id: `profile-${name}`, hint: opts.hint, class: opts.span ? "span-2" : undefined });
    };

    const country = select(
      [{ value: "", label: "Not specified" }, ...COUNTRIES.map((c) => ({ value: c, label: c }))],
      { id: "profile-country", value: profile.country_code ?? "" },
    );

    const taxId = input({
      id: "profile-tax",
      value: profile.tax_id ?? "",
      placeholder: "VAT / GST / EIN",
      class: "input--mono",
    });

    const errorHost = h("div");
    const save = btn("Save billing details", { variant: "primary", type: "submit" });
    const cancel = options.readOnly
      ? btn("Cancel", {
          variant: "ghost",
          onclick: () => {
            editing = false;
            render();
          },
        })
      : null;

    const formEl = h(
      "form",
      { class: "stack", novalidate: true },
      errorHost,
      sectionTitle("Legal entity"),
      h(
        "div",
        { class: "form-grid" },
        make("legal_name", "Legal name", { placeholder: "Acme Holdings Ltd", span: true, hint: "The name that must appear on a tax invoice." }),
        make("address_line1", "Address", { placeholder: "1 Ledger Street", span: true }),
        make("address_line2", "Address line 2", { placeholder: "Suite 400", span: true }),
        make("city", "City", { placeholder: "Springfield" }),
        make("region", "State / region", { placeholder: "IL" }),
        make("postal_code", "Postal code", { placeholder: "62704" }),
        field({ label: "Country", control: country, id: "profile-country", hint: "ISO 3166-1 alpha-2." }),
        field({
          label: "Tax ID",
          control: taxId,
          id: "profile-tax",
          hint: "Stored as given. It is not validated against a jurisdiction.",
          class: "span-2",
        }),
      ),
      h("div", { class: "form-actions" }, cancel, save),
    );

    formEl.addEventListener("submit", async (event) => {
      event.preventDefault();
      errorHost.replaceChildren();
      if (save instanceof HTMLButtonElement) save.disabled = true;
      const body = {
        legal_name: values.legal_name?.value.trim() ?? "",
        address_line1: values.address_line1?.value.trim() ?? "",
        address_line2: values.address_line2?.value.trim() ?? "",
        city: values.city?.value.trim() ?? "",
        postal_code: values.postal_code?.value.trim() ?? "",
        region: values.region?.value.trim() ?? "",
        country_code: country.value,
        tax_id: taxId.value.trim(),
      };
      try {
        const saved = await ctx.api.put<BillingProfile>(options.path, body);
        current = saved;
        editing = false;
        ctx.toast("Billing details saved", { tone: "success" });
        options.onSaved?.(saved);
        render();
      } catch (err) {
        const message =
          err instanceof Error && "detail" in err && typeof (err as { detail?: string }).detail === "string"
            ? (err as { detail: string }).detail
            : "The details could not be saved.";
        errorHost.replaceChildren(banner("danger", "Not saved", message));
      } finally {
        if (save instanceof HTMLButtonElement) save.disabled = false;
      }
    });

    return formEl;
  };

  const meta = (label: string, value: string) =>
    h("div", { class: "meta" }, h("span", { class: "meta-label" }, label), h("span", { class: "meta-value" }, value));

  void load();

  return card({
    title: options.title ?? "Billing details",
    sub: "Copied onto the next invoice; issued invoices keep their snapshot.",
    body: host,
  });
}

/** Loads a profile for a page that needs the values but not the form. */
export async function fetchProfile(api: Api, path: string): Promise<BillingProfile | null> {
  try {
    return await api.get<BillingProfile>(path, { cache: false });
  } catch {
    return null;
  }
}
