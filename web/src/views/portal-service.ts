// One service, and everything a customer may change about it from here.
//
// Four things are theirs: schedule a cancellation, ask for a different plan, change the config
// options the operator made changeable, and rename/describe the service plus edit the properties
// marked `customer_visible`. Each is a separate endpoint, and each is reached from this page —
// before this screen existed the endpoints were served and unreachable, so every plan change in
// the product was a support ticket.
//
// # The rules this page states are the server's, not its own
//
//   - `can_change` is the server's answer to "may I re-price this at all?", and
//     `change_blocked_reason` is the sentence to show when the answer is no. It arrives with the
//     options read, because it is the same question, and the page renders the plan and option
//     forms only when it is true — so it cannot offer a form that would come back 409 after the
//     customer filled it in.
//   - The prices are the server's rows: `upgrade-options` prices each target against this
//     service's plan and states the prorated consequence, and `config-options` carries the amount
//     each choice has on this service's price version — the same rows the charges are computed
//     from. Nothing here multiplies, rounds or converts.
//   - A customer's service that is not theirs answers 404, and the API makes that
//     indistinguishable from an id that does not exist. The page renders "not available" for both,
//     because a page that turned one into a stack trace would leak the difference.
//
// # Why the outcome of a change is quoted from the response
//
// An upgrade issues a prorated invoice; a downgrade records a credit an operator still has to post;
// a config change does one or the other. The response says which, with the invoice number or the
// credit amount, and this page repeats that rather than assuming. A screen that said "upgraded!"
// over a pending invoice would be telling the customer something the service does not yet do.

import { ApiError } from "../lib/api";
import { h } from "../lib/dom";
import { dateOnly, dateTime, moneyWithCode, shortId, type Amount } from "../lib/format";
import type { Ctx } from "../lib/types";
import {
  banner,
  btn,
  card,
  emptyState,
  errorState,
  field,
  input,
  metaGrid,
  pageHead,
  select,
  skeletonPage,
  statusBadge,
  textarea,
  withBusy,
} from "../ui/kit";
import { mountServerCard } from "../ui/game-server";
import type { PortalService } from "./portal";

export const meta = { title: "Service", crumb: "Service" };

/** The answer to "what may I change on this service?", from `handlePortalConfigOptions`.
 *
 *  It carries the flag as well as the options because they are one fact: a page asks what it may
 *  change and gets one answer, priced on the plan the service renews at. The flag is computed by
 *  the server from the same rules the upgrade and config handlers enforce, so the forms are only
 *  offered where the server would accept them. */
interface ChangeableOptions {
  service_id: string;
  price_version_id: string;
  currency: string;
  can_change: boolean;
  change_blocked_reason: string;
  options: ConfigOption[];
}

// ---------------------------------------------------------------- payloads --

/** The 201 from `POST .../cancellation`, i.e. `cancellationJSON`.
 *
 *  `status` is `scheduled` for an end-of-period request and `pending` for an immediate one, and
 *  `effective_at` is the paid-through date for the first. The page states which, because "your
 *  service is cancelled" and "your request is waiting for support" are different promises. */
interface CancellationRequestResult {
  id: string;
  service_id: string;
  type: string;
  status: string;
  reason: string;
  created_at: string;
  effective_at?: string;
  executed_at?: string;
}

/** One price a service can move to, from `upgradePriceJSON`.
 *
 *  `prorated` is what the move costs (positive) or returns (negative) for the rest of the current
 *  period, computed by the server from the same proration an upgrade charges from. */
interface UpgradePrice {
  price_version_id: string;
  version: number;
  name: string;
  interval: string;
  amount: Amount;
  setup: Amount;
  prorated: Amount;
}

/** One reachable product with its prices, from `handleUpgradeOptions`. */
interface UpgradeOption {
  product_id: string;
  product_name: string;
  product_slug: string;
  short_description: string;
  prices: UpgradePrice[];
}

/** One choice of a config option with its amount on this service's plan, from
 *  `handlePortalConfigOptions`. Integer minor units with the response's `currency`, like every
 *  other money field the API emits. */
interface ConfigChoice {
  choice: string;
  amount_minor: number;
  setup_minor: number;
}

/** One option of a plan the customer may move to, from `handlePortalPlanOptions`.
 *
 *  The amounts are the rows `handlePortalChangePlan` charges from, priced on that plan's own
 *  price version — so the price on screen and the price on the invoice are the same rows. */
interface PlanOption {
  config_option_id: string;
  name: string;
  description: string;
  type: string;
  env_variable: string;
  required: boolean;
  upgradable: boolean;
  value: string;
  choices: ConfigChoice[];
}

/** `GET .../plan-options` — the target plan's options, priced on that plan. */
interface PlanOptions {
  service_id: string;
  product_id: string;
  price_version_id: string;
  options: PlanOption[];
}

/** One config option a customer may change, with its current value. */
interface ConfigOption {
  config_option_id: string;
  name: string;
  description: string;
  type: string;
  env_variable: string;
  required: boolean;
  upgradable: boolean;
  value: string;
  choices: ConfigChoice[];
}

/** One property value with its definition attached, from `propertyValueJSON`.
 *
 *  The API returns only the properties the operator marked `customer_visible`, so a page that
 *  renders this list cannot show an internal one by accident. */
interface ServiceProperty {
  key: string;
  label: string;
  description: string;
  value: string;
  show_on_invoice: boolean;
  customer_visible: boolean;
  sort_order: number;
}

/** The service's own description, from `serviceMetadataJSON`.
 *
 *  `label`/`description` are what a human wrote; `effective_*` is what to display. Both are here
 *  because the edit form needs the first and the heading needs the second. */
interface ServiceDetails {
  service_id: string;
  status: string;
  product: { id: string; name: string };
  plan: {
    price_version_id: string;
    name: string;
    interval: string;
    currency: string;
    amount_minor: number;
    setup_minor: number;
    amount?: Amount;
  };
  quantity: number;
  label: string;
  description: string;
  effective_label: string;
  effective_description: string;
  derived_description: string;
  custom_properties: Record<string, string>;
  properties: ServiceProperty[];
  updated_at?: string;
}

/** The outcome of an upgrade or a config change, from `upgradeResultJSON`. */
interface ChangeResult {
  upgrade_id: string;
  service_id: string;
  direction: string;
  currency: string;
  prorated_minor: number;
  status: string;
  proration: { period_start: string; period_end: string; as_of: string; ratio: string };
  invoice_id?: string;
  invoice_number?: string;
  credit_minor?: number;
  credit_pending?: boolean;
  message?: string;
}

// ------------------------------------------------------------------- page --

export default async function portalServicePage(ctx: Ctx): Promise<Node> {
  const id = ctx.route.params.id ?? "";
  const host = h("div", { class: "page" }, skeletonPage());

  let current: PortalService | null = null;
  let details: ServiceDetails | null = null;
  let targets: Array<UpgradePrice & { productName: string }> = [];
  let configOptions: ConfigOption[] = [];
  /** Whether the server would accept a re-price on this service, and why not when it would not. */
  let canChange = false;
  let blockedReason = "";
  /** The reason the two change reads failed, when they did. It is shown inside the cards rather
   *  than replacing the page: a service the customer can still cancel and rename must not become
   *  a blank screen because one read failed. */
  let changeReadError: string | null = null;
  let notice: Node | null = null;

  /** Fetches one optional read, remembering why it failed instead of failing the page. */
  const optional = async <T>(path: string): Promise<T | null> => {
    try {
      return await ctx.api.get<T>(path, { cache: false });
    } catch (err) {
      changeReadError ??= err instanceof ApiError ? err.detail || err.code : String(err);
      return null;
    }
  };

  const load = async (): Promise<void> => {
    let svc: PortalService;
    let md: ServiceDetails;
    try {
      // The two reads answer two halves of one question — "what is this, and what may I do with
      // it" — and the page is wrong if either is missing, so they are fetched together.
      [svc, md] = await Promise.all([
        ctx.api.get<PortalService>(`/api/v1/portal/services/${encodeURIComponent(id)}`, {
          cache: false,
        }),
        ctx.api.get<ServiceDetails>(`/api/v1/portal/services/${encodeURIComponent(id)}/metadata`, { cache: false }),
      ]);
    } catch (err) {
      if (err instanceof ApiError && (err.status === 403 || err.status === 404)) {
        host.replaceChildren(unavailable());
        return;
      }
      host.replaceChildren(
        h(
          "div",
          { class: "stack" },
          pageHead({
            title: "Service",
            actions: btn("Back to services", { href: "/portal/services", size: "sm", icon: "arrowLeft" }),
          }),
          errorState(err, () => {
            host.replaceChildren(skeletonPage());
            void load();
          }),
        ),
      );
      return;
    }
    current = svc;
    details = md;
    targets = [];
    configOptions = [];
    canChange = false;
    blockedReason = "";
    changeReadError = null;

    // The options read always runs: it is what carries `can_change`, so it is also how this page
    // learns whether to offer the plan form at all.
    const options = await optional<ChangeableOptions>(
      `/api/v1/portal/services/${encodeURIComponent(id)}/config-options`,
    );
    canChange = options?.can_change ?? false;
    blockedReason = options?.change_blocked_reason ?? "";
    configOptions = options?.options ?? [];
    if (canChange) {
      // Only asked for when the server says a re-price is possible: a service with no billing
      // schedule has no plan to price a target against.
      const upgrades = await optional<{ options: UpgradeOption[] }>(
        `/api/v1/portal/services/${encodeURIComponent(id)}/upgrade-options`,
      );
      targets = (upgrades?.options ?? []).flatMap((option) =>
        option.prices.map((price) => ({ ...price, productName: option.product_name })),
      );
    }
    render();
  };

  // --------------------------------------------------------------- actions --

  /** Reports what a change did, in the words the response used. */
  const reportChange = (result: ChangeResult, heading: string): void => {
    const amount = (minor: number): Amount => ({
      minor: Math.abs(minor),
      currency: result.currency,
      scale: details?.plan.amount?.scale ?? 2,
    });
    if (result.invoice_number) {
      notice = banner(
        "success",
        heading,
        h(
          "span",
          null,
          `Invoice ${result.invoice_number} covers the prorated difference of `,
          h("strong", null, moneyWithCode(amount(result.prorated_minor))),
          ". The change takes effect once that invoice is paid.",
        ),
      );
    } else if (result.credit_pending) {
      notice = banner(
        "success",
        heading,
        h(
          "span",
          null,
          `You are owed ${moneyWithCode(amount(result.credit_minor ?? result.prorated_minor))} for the ` +
            "rest of this period. ",
          result.message || "It will be credited to your account balance.",
        ),
      );
    } else {
      notice = banner(
        "success",
        heading,
        h("span", null, "Nothing further is due for the rest of this period, so there is no invoice."),
      );
    }
    ctx.toast(heading, { tone: "success" });
  };

  /** Reports a failed change with the code the API chose, never with a guess. */
  const reportChangeError = (err: unknown, fallback: string): void => {
    if (!(err instanceof ApiError)) {
      ctx.toast(fallback, {
        tone: "danger",
        detail: "The server could not be reached. Nothing has changed.",
      });
      return;
    }
    switch (err.code) {
      case "not_found":
      case "forbidden":
        ctx.toast("This service is not available", {
          tone: "danger",
          detail: "It may belong to another account, or it may no longer exist.",
        });
        return;
      case "not_upgradable":
      case "option_not_upgradable":
      case "option_not_available":
      case "currency_mismatch":
      case "interval_mismatch":
        ctx.toast("That change is not available", { tone: "danger", detail: err.detail || err.code });
        return;
      case "rate_limited":
        ctx.toast("Too many attempts", { tone: "danger", detail: "Wait a minute and try again." });
        return;
      default:
        ctx.toast(fallback, { tone: "danger", detail: err.detail || err.code });
    }
  };

  /** Asks the server for a plan change: the target plan and the options chosen on it, priced as
   *  one prorated change.
   *
   *  `selection` is `undefined` when the target plan's options could not be read. That is a
   *  deliberate difference from an empty list: absent means "leave my options alone" and empty
   *  means "this plan has none, take mine away", and the server prices them differently. Sending
   *  an empty list because a read failed would silently drop options the customer pays for. */
  const requestPlanChange = async (
    target: UpgradePrice & { productName: string },
    selection: Array<{ config_option_id: string; value: string }> | undefined,
  ): Promise<void> => {
    const prorated = target.prorated.minor;
    const chosen = selection?.length
      ? ` With ${selection.length} option${selection.length === 1 ? "" : "s"} on the new plan, `
      : " ";
    const confirmed = await ctx.confirm({
      title: `Move to ${target.productName}?`,
      body:
        `${target.productName} — ${target.name || `version ${target.version}`} costs ` +
        `${moneyWithCode(target.amount)} per ${target.interval}.` +
        chosen +
        (prorated > 0
          ? `the prorated difference for the rest of the period you are in is charged now (the plan difference alone is ${moneyWithCode(target.prorated)}; the options are added to it).`
          : prorated < 0
            ? `you are credited the prorated difference for the rest of the period you are in (the plan difference alone is ${moneyWithCode({ ...target.prorated, minor: -prorated })}; the options are added to it).`
            : "no plan difference is due for the rest of this period; any option you add is prorated separately."),
      confirmLabel: "Request the change",
      cancelLabel: "Keep the current plan",
      note: "The change is applied once its invoice is paid. Nothing is charged before you confirm.",
    });
    if (!confirmed) return;
    try {
      const result = await ctx.api.post<ChangeResult>(`/api/v1/portal/services/${encodeURIComponent(id)}/change-plan`, {
        price_version_id: target.price_version_id,
        config_options: selection,
        reason: "requested from the portal",
      });
      reportChange(result, "Plan change requested");
      await load();
    } catch (err) {
      reportChangeError(err, "The plan change could not be requested");
    }
  };

  /** Asks the server to change one config option, after stating the price difference. */
  const changeOption = async (option: ConfigOption, value: string): Promise<void> => {
    const currentAmount = option.choices.find((c) => c.choice === option.value)?.amount_minor ?? 0;
    const nextAmount = option.choices.find((c) => c.choice === value)?.amount_minor ?? 0;
    const delta = nextAmount - currentAmount;
    const scale = details?.plan.amount?.scale ?? 2;
    const currency = details?.plan.currency ?? "USD";
    const money = (minor: number): Amount => ({ minor: Math.abs(minor), currency, scale });

    const confirmed = await ctx.confirm({
      title: `Change ${option.name} to ${value}?`,
      body:
        delta > 0
          ? `${option.name} becomes ${moneyWithCode(money(delta))} more per billing period, and the prorated difference for the rest of this period is charged now.`
          : delta < 0
            ? `${option.name} becomes ${moneyWithCode(money(delta))} less per billing period, and the difference for the rest of this period is credited to you.`
            : `${option.name} does not change the price of this service.`,
      confirmLabel: "Change the option",
      cancelLabel: "Leave it as it is",
    });
    if (!confirmed) return;
    try {
      const result = await ctx.api.put<ChangeResult>(`/api/v1/portal/services/${encodeURIComponent(id)}/config`, {
        config_option_id: option.config_option_id,
        value,
        reason: "changed from the portal",
      });
      reportChange(result, `${option.name} updated`);
      await load();
    } catch (err) {
      reportChangeError(err, `${option.name} could not be changed`);
    }
  };

  /** The metadata edit: the label, the description and the visible properties. */
  const editDetails = async (): Promise<void> => {
    const md = details;
    if (!md) return;

    const labelInput = input({
      id: "service-label",
      value: md.label,
      placeholder: md.product.name,
      maxlength: 120,
      autocomplete: "off",
    });
    const descriptionInput = textarea({
      id: "service-description",
      value: md.description,
      placeholder: md.derived_description,
      maxlength: 2000,
    });
    // Only the properties the server returned are editable, and only those are sent back: the API
    // refuses a key whose definition is not customer_visible, and inventing one here would be a
    // 403 the customer cannot act on.
    const propertyFields = md.properties.map((property) => ({
      property,
      control: input({
        id: `service-property-${property.key}`,
        value: property.value,
        maxlength: 200,
        autocomplete: "off",
      }),
    }));

    await ctx.formDialog({
      title: "Rename and describe this service",
      subtitle: "This is what your own page shows. Support sees the same name.",
      submitLabel: "Save",
      body: h(
        "div",
        { class: "stack" },
        field({
          label: "Name",
          hint: `Shown instead of ${md.product.name}. Leave it empty to use the product's name.`,
          control: labelInput,
        }),
        field({
          label: "Description",
          hint: "Leave it empty to use the description derived from your plan and options.",
          control: descriptionInput,
        }),
        propertyFields.length > 0
          ? h(
              "div",
              { class: "stack-sm" },
              h("p", { class: "field-hint" }, "Details support recorded for this service"),
              ...propertyFields.map((entry) =>
                field({
                  label: entry.property.label,
                  hint: entry.property.description || undefined,
                  control: entry.control,
                }),
              ),
            )
          : null,
      ),
      onSubmit: async () => {
        const properties: Record<string, string> = {};
        for (const entry of propertyFields) {
          properties[entry.property.key] = entry.control.value.trim();
        }
        try {
          await ctx.api.put(`/api/v1/portal/services/${encodeURIComponent(id)}/metadata`, {
            label: labelInput.value.trim(),
            description: descriptionInput.value.trim(),
            custom_properties: properties,
          });
        } catch (err) {
          if (err instanceof ApiError) {
            if (err.code === "property_not_visible") {
              return "One of these details is not yours to edit. Reload the page and try again.";
            }
            return err.detail || err.code;
          }
          return String(err);
        }
        ctx.toast("Saved", { tone: "success", detail: labelInput.value.trim() || md.product.name });
        await load();
        return;
      },
    });
  };

  /** Files a cancellation request: when it should take effect, and why.
   *
   *  Two statements reach the server and both are recorded. The type decides what happens — an
   *  end-of-period request stops the schedule at the date the customer has paid to, and an
   *  immediate one is a *request for a decision*, which support executes — and the reason is what
   *  the operator reads in the queue. The page used to post an end-of-period cancellation with no
   *  reason at all, which meant a customer could not ask for the one thing a cancellation is
   *  usually about and no operator ever saw the request (CAN-01…CAN-03). */
  const requestCancellation = async (): Promise<void> => {
    const type = select(
      [
        { value: "end_of_period", label: "At the end of the period I have paid for" },
        { value: "immediate", label: "Immediately — support reviews it first" },
      ],
      { id: "cancel-type", value: "end_of_period" },
    );
    const reason = textarea({
      id: "cancel-reason",
      placeholder: "Why are you cancelling? Support reads this, and it is kept with the request.",
      maxlength: 255,
    });
    const consequence = h("p", { class: "field-hint" });
    const explain = (): void => {
      consequence.replaceChildren(
        type.value === "immediate"
          ? "The service keeps running until support has acted on the request. Cancelling immediately " +
              "ends time you have already paid for, so it is a decision an operator makes, not a button."
          : "The service runs until the end of the period you have paid for" +
              (current && current.next_bill_at ? ` (next bill ${dateOnly(current.next_bill_at)})` : "") +
              ", and then stops. Nothing further is billed.",
      );
    };
    type.addEventListener("change", explain);
    explain();

    await ctx.formDialog({
      title: "Cancel this service?",
      subtitle: current?.name || "This service",
      submitLabel: "Send the request",
      body: h(
        "div",
        { class: "stack" },
        field({ label: "When should it stop?", control: type, id: "cancel-type" }),
        consequence,
        field({
          label: "Reason",
          control: reason,
          id: "cancel-reason",
          hint: "Optional, but a reason lets support help if there is a problem worth solving.",
        }),
      ),
      onSubmit: async () => {
        try {
          const res = await ctx.api.post<CancellationRequestResult>(
            `/api/v1/portal/services/${encodeURIComponent(id)}/cancellation`,
            { type: type.value, reason: reason.value.trim() },
          );
          notice = banner(
            "success",
            res.type === "immediate" ? "Cancellation requested" : "Cancellation scheduled",
            h(
              "span",
              null,
              res.type === "immediate"
                ? "Your request has been recorded and support will act on it. The service keeps running until they do."
                : res.effective_at
                  ? `Your service ends on ${dateTime(res.effective_at)}, at the end of the period you have paid for.`
                  : "Your service ends at the end of the period you have paid for.",
            ),
          );
          ctx.toast(
            res.type === "immediate" ? "Cancellation requested" : "Cancellation scheduled",
            {
              tone: "success",
              detail:
                res.type === "immediate"
                  ? "Support will review it."
                  : res.effective_at
                    ? `Your service ends on ${dateOnly(res.effective_at)}.`
                    : "At the end of the paid period.",
            },
          );
          await load();
          return;
        } catch (err) {
          return cancellationProblem(err);
        }
      },
    });
  };

  // ---------------------------------------------------------------- render --

  const render = (): void => {
    if (!current) return;
    const svc = current;
    const md = details;
    const nodes: Node[] = [
      pageHead({
        title: md?.effective_label || svc.name || "Service",
        badge: statusBadge(svc.status),
        sub: `Service ${shortId(svc.id)}`,
        actions: btn("Back to services", { href: "/portal/services", size: "sm", icon: "arrowLeft" }),
      }),
    ];
    if (notice) nodes.push(notice);
    const options = optionsCard();
    nodes.push(
      h(
        "div",
        { class: "split" },
        h(
          "div",
          { class: "stack" },
          serviceCard(svc, md),
          // One card, shared with the staff page: a support agent has to see what the customer
          // sees, and two implementations drift into a conversation about a screen the agent
          // cannot reproduce.
          mountServerCard(ctx, {
            serviceId: svc.id,
            basePath: "/api/v1/portal/services",
            canPower: true,
            pendingNote: "This service has no server yet. It appears here once it has been provisioned.",
          }),
          planCard(),
          options,
          actionCard(svc, requestCancellation),
        ),
        h(
          "div",
          { class: "stack" },
          detailsCard(md),
          helpCard(svc),
        ),
      ),
    );
    host.replaceChildren(...nodes);
  };

  // ------------------------------------------------------------- the cards --

  /** What the service is, and the plan it renews at. */
  const serviceCard = (svc: PortalService, md: ServiceDetails | null): HTMLElement =>
    card({
      title: "Service",
      body: metaGrid([
        { label: "Status", value: statusBadge(svc.status) },
        {
          label: "Plan",
          value: md
            ? `${md.plan.name || md.product.name}` +
              (md.plan.amount ? ` · ${moneyWithCode(md.plan.amount)} / ${md.plan.interval}` : "")
            : "—",
        },
        {
          label: "Next bill",
          value: svc.next_bill_at ? dateTime(svc.next_bill_at) : "No renewal scheduled",
        },
        { label: "Service id", value: h("span", { class: "mono" }, svc.id) },
      ]),
    });

  /** The plans this service can move to, priced for the rest of the period. */
  const planCard = (): HTMLElement => {
    if (!canChange) {
      return card({
        title: "Change plan",
        body: h(
          "div",
          { class: "stack-sm" },
          h(
            "p",
            { class: "page-sub" },
            blockedReason || "This service cannot be re-priced from the portal right now.",
          ),
          h(
            "div",
            { class: "row-wrap" },
            btn("Ask support", { href: "/tickets/new", size: "sm", icon: "message" }),
          ),
        ),
      });
    }
    if (changeReadError) {
      return card({
        title: "Change plan",
        body: banner(
          "warning",
          "The plans could not be loaded",
          `${changeReadError}. Nothing has changed; try again in a moment.`,
        ),
      });
    }
    if (targets.length === 0) {
      return card({
        title: "Change plan",
        body: h(
          "p",
          { class: "page-sub" },
          "There is no other plan you can move to from this one. Contact support if you need something different.",
        ),
      });
    }

    const hint = h("p", { class: "field-hint" });
    const control = select(
      targets.map((target) => ({
        value: target.price_version_id,
        label:
          `${target.productName} — ${target.name || `version ${target.version}`}` +
          ` · ${moneyWithCode(target.amount)} / ${target.interval}`,
      })),
      { id: "upgrade-target" },
    );

    // The target plan's own options, and the values chosen on them. Both are keyed by target
    // because the options of one plan are not the options of another: the amounts come from that
    // plan's price version, so carrying a selection across would price one plan with another's
    // prices.
    const optionsHost = h("div", { class: "stack-sm" });
    let loadedFor: string | null = null;
    let controls: Array<{ option: PlanOption; read: () => string; control?: HTMLElement }> = [];
    let optionsError: string | null = null;

    const describe = (): void => {
      const target = targets.find((t) => t.price_version_id === control.value);
      if (!target) {
        hint.replaceChildren("");
        return;
      }
      const base =
        target.prorated.minor > 0
          ? `The plan difference is ${moneyWithCode(target.prorated)} for the rest of the period you have paid for`
          : target.prorated.minor < 0
            ? `The plan difference credits you ${moneyWithCode({ ...target.prorated, minor: -target.prorated.minor })} for the rest of the period you have paid for`
            : "The plan itself costs nothing extra for the rest of the period you have paid for";
      hint.replaceChildren(
        `${base}. ` +
          (loadedFor && controls.length > 0
            ? "Your options are priced with it — the exact difference is on the invoice."
            : "Options are priced with it when the plan has any."),
      );
    };

    /** Loads the chosen plan's options, if they are not loaded already. */
    const loadOptions = async (): Promise<void> => {
      const target = targets.find((t) => t.price_version_id === control.value);
      if (!target || loadedFor === target.price_version_id) return;
      loadedFor = target.price_version_id;
      controls = [];
      optionsError = null;
      optionsHost.replaceChildren(h("p", { class: "field-hint" }, "Loading the options of this plan…"));
      let payload: PlanOptions;
      try {
        payload = await ctx.api.get<PlanOptions>(
          `/api/v1/portal/services/${encodeURIComponent(id)}/plan-options`,
          { query: { price_version_id: target.price_version_id }, cache: false },
        );
      } catch (err) {
        optionsError = err instanceof ApiError ? err.detail || err.code : String(err);
        optionsHost.replaceChildren(
          banner(
            "warning",
            "The options of this plan could not be read",
            `${optionsError}. You can still request the plan itself; your current options are left as they are.`,
          ),
        );
        describe();
        return;
      }
      const options = payload.options ?? [];
      if (options.length === 0) {
        optionsHost.replaceChildren(
          h("p", { class: "field-hint" }, "This plan has no configurable options."),
        );
        describe();
        return;
      }
      controls = options.map((option) => {
        if (!option.upgradable) {
          // Fixed by the plan we are moving to. It is still part of the selection, so it is sent
          // as its only offered value rather than omitted — omitting it would leave the service
          // without a value the plan requires.
          const fixed = option.value || option.choices[0]?.choice || "";
          return {
            option,
            read: () => fixed,
          };
        }
        const picker = select(
          option.choices.map((choice) => ({
            value: choice.choice,
            label: `${choice.choice}${choice.amount_minor ? ` · +${moneyWithCode({ minor: choice.amount_minor, currency: details?.plan.currency ?? "USD", scale: details?.plan.amount?.scale ?? 2 })}` : " · included"}`,
          })),
          { id: `plan-option-${option.config_option_id}`, value: option.value || undefined },
        );
        return { option, read: () => picker.value, control: picker };
      });
      optionsHost.replaceChildren(
        h(
          "div",
          { class: "stack-sm" },
          h("p", { class: "field-hint" }, "Options on this plan"),
          ...controls.map((entry) =>
            field({
              label: entry.option.name,
              hint:
                entry.option.description ||
                (entry.option.upgradable ? undefined : "Fixed by this plan."),
              control: entry.control ?? h("p", { class: "kv-value" }, entry.read() || "—"),
            }),
          ),
        ),
      );
      describe();
    };

    control.addEventListener("change", () => {
      void loadOptions();
    });
    describe();
    void loadOptions();

    const button = btn("Request change", { variant: "primary", icon: "arrowUpRight" }) as HTMLButtonElement;
    button.addEventListener("click", () => {
      const target = targets.find((t) => t.price_version_id === control.value);
      if (!target) return;
      // `undefined` when the options could not be read: absent means "leave mine alone", which is
      // the only safe answer when the page does not know what the target plan offers.
      const selection = loadedFor === target.price_version_id && !optionsError
        ? controls.map((entry) => ({ config_option_id: entry.option.config_option_id, value: entry.read() }))
        : undefined;
      void withBusy(ctx, button, () => requestPlanChange(target, selection));
    });

    return card({
      title: "Change plan",
      sub: "Pick a plan and its options; the difference is prorated for the rest of the period",
      body: h(
        "div",
        { class: "stack" },
        field({ label: "New plan", control }),
        optionsHost,
        hint,
        h("div", { class: "row-wrap" }, button),
        h(
          "p",
          { class: "field-hint" },
          "The plan and the options you choose are one change, priced together. An increase issues an " +
            "invoice for the prorated difference and takes effect when it is paid. A decrease credits you " +
            "for the rest of the period and is applied once the credit is posted.",
        ),
      ),
    });
  };

  /** The config options the operator made changeable, priced on this service's plan.
   *
   *  Returns null when the server says the service cannot be re-priced: the plan card already
   *  explains why, and a second card repeating the same sentence is noise. */
  const optionsCard = (): HTMLElement | null => {
    if (!canChange) return null;
    if (changeReadError) {
      return card({
        title: "Options",
        body: banner("warning", "The options could not be loaded", `${changeReadError}.`),
      });
    }
    if (configOptions.length === 0) {
      return card({
        title: "Options",
        body: h(
          "p",
          { class: "page-sub" },
          "This plan has no options you can change from here. Contact support if you need a different configuration.",
        ),
      });
    }

    const scale = details?.plan.amount?.scale ?? 2;
    const currency = details?.plan.currency ?? "USD";
    const price = (minor: number): string =>
      moneyWithCode({ minor, currency, scale });

    const rows = configOptions.map((option) => {
      if (!option.upgradable || option.choices.length === 0) {
        return field({
          label: option.name,
          hint: option.description || "Fixed by your plan.",
          control: h(
            "p",
            { class: "kv-value" },
            option.value || "—",
            h("span", { class: "field-hint" }, " · fixed by your plan"),
          ),
        });
      }
      const control = select(
        option.choices.map((choice) => ({
          value: choice.choice,
          label: `${choice.choice}${choice.amount_minor ? ` · +${price(choice.amount_minor)}` : " · included"}`,
        })),
        { id: `config-${option.config_option_id}`, value: option.value },
      );
      const button = btn("Change", { size: "sm", icon: "sliders" }) as HTMLButtonElement;
      button.addEventListener("click", () => {
        void withBusy(ctx, button, () => changeOption(option, control.value));
      });
      return field({
        label: option.name,
        hint: option.description || undefined,
        control: h("div", { class: "row-wrap" }, control, button),
      });
    });

    return card({
      title: "Options",
      sub: "Priced on your current plan",
      body: h(
        "div",
        { class: "stack" },
        ...rows,
        h(
          "p",
          { class: "field-hint" },
          "A change re-prices this service for the rest of the period and from the next renewal. " +
            "The prorated difference is charged or credited exactly as a plan change is.",
        ),
      ),
    });
  };

  /** The name, the description and the properties the operator made visible. */
  const detailsCard = (md: ServiceDetails | null): HTMLElement => {
    if (!md) {
      return card({
        title: "Name and details",
        body: banner("warning", "The details could not be loaded", "Try reloading the page."),
      });
    }
    const button = btn("Edit", { size: "sm", icon: "edit" }) as HTMLButtonElement;
    button.addEventListener("click", () => void editDetails());

    const properties = md.properties.length
      ? h(
          "div",
          { class: "stack-sm" },
          ...md.properties.map((property) =>
            h(
              "div",
              { class: "kv" },
              h("span", { class: "kv-key" }, property.label),
              h("span", { class: "kv-value" }, property.value || "—"),
            ),
          ),
        )
      : h(
          "p",
          { class: "field-hint" },
          "Support has not recorded any details you can see on this service.",
        );

    return card({
      title: "Name and details",
      sub: "What your page shows",
      actions: button,
      body: h(
        "div",
        { class: "stack-sm" },
        h("p", { class: "page-sub" }, md.effective_description),
        md.label ? null : h("p", { class: "field-hint" }, `Named after the product: ${md.product.name}.`),
        properties,
        h(
          "p",
          { class: "field-hint" },
          `Updated ${dateTime(md.updated_at ?? "")}. Only the details support made visible to you can be edited.`,
        ),
      ),
    });
  };

  /** What this page can and cannot do, stated once. */
  const helpCard = (svc: PortalService): HTMLElement =>
    card({
      title: "What you can do here",
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "p",
          { class: "page-sub" },
          "You can rename and describe this service, change the options your plan makes changeable, " +
            "ask for a different plan, and schedule a cancellation.",
        ),
        h(
          "p",
          { class: "page-sub" },
          "Cancelling normally stops the service at the end of the period you have paid for. An immediate " +
            "cancellation ends time you have paid for, so it is a request support decides on — either way " +
            "you can say why, and the request reaches the support queue.",
        ),
        h(
          "div",
          { class: "row-wrap" },
          btn("See your invoices", { href: "/portal/invoices", size: "sm", icon: "invoice" }),
          svc.actions.includes("cancel") || canChange
            ? null
            : btn("Ask support", { href: "/tickets/new", size: "sm", icon: "message" }),
          btn("Back to overview", { href: "/portal", size: "sm", icon: "arrowLeft" }),
        ),
      ),
    });

  void load();
  return host;
}

/** The action card, chosen from the server-computed `actions` list. */
function actionCard(
  svc: PortalService,
  onCancel: (button: HTMLButtonElement) => Promise<void>,
): HTMLElement {
  if (svc.actions.includes("cancel")) {
    const button = btn("Request cancellation", { variant: "danger", icon: "x" }) as HTMLButtonElement;
    button.addEventListener("click", () => void onCancel(button));
    return card({
      title: "Cancel this service",
      sub: "At the end of the paid period, or ask support to do it now",
      body: h(
        "div",
        { class: "stack" },
        h(
          "p",
          { class: "page-sub" },
          "By default the service runs until the end of the period you have already paid for" +
            (svc.next_bill_at ? ` (next bill ${dateOnly(svc.next_bill_at)})` : "") +
            ", and then stops, and nothing further is billed. You can also ask for it to stop now — " +
            "that ends time you have paid for, so support reviews it before acting.",
        ),
        h("div", { class: "row-wrap" }, button),
      ),
    });
  }

  if (svc.actions.includes("pay_to_restore")) {
    return card({
      title: "This service is suspended",
      body: h(
        "div",
        { class: "stack" },
        banner(
          "warning",
          "Suspended until the outstanding invoice is paid",
          "Paying the open invoice restores the service. The portal cannot restore it any other way.",
        ),
        h(
          "div",
          { class: "row-wrap" },
          btn("See what is owed", { href: "/portal/invoices", variant: "primary", icon: "invoice", size: "sm" }),
        ),
      ),
    });
  }

  return card({
    title: "No changes available",
    body: h(
      "div",
      { class: "stack-sm" },
      h(
        "p",
        { class: "page-sub" },
        svc.status === "active"
          ? "Cancellation is already scheduled for this service, so there is nothing left to change from here. It runs until the end of the period you have paid for."
          : "There is nothing you can change on this service from the portal. Contact support if you need something.",
      ),
      h(
        "div",
        { class: "row-wrap" },
        btn("See your invoices", { href: "/portal/invoices", size: "sm", icon: "invoice" }),
      ),
    ),
  });
}

/** Plain copy for the documented refusals of a cancellation request, shown in the dialog so the
 *  customer can act on it without reopening the form. */
function cancellationProblem(err: unknown): string {
  if (!(err instanceof ApiError)) {
    return "The server could not be reached, so nothing has changed. Try again in a moment.";
  }
  switch (err.code) {
    case "already_requested":
      return "A cancellation request is already open for this service. Reload the page to see it.";
    case "not_cancellable":
      return err.detail || "This service cannot be cancelled in its current state. Contact support.";
    case "invalid_input":
      return err.detail || "Check the request and try again.";
    case "not_found":
    case "forbidden":
      return "This service is not available. It may belong to another account, or it may no longer exist.";
    case "rate_limited":
      return "Too many attempts. Wait a minute and try again.";
    default:
      return err.detail || `The request could not be sent (${err.code}).`;
  }
}

function unavailable(): Node {
  return h(
    "div",
    { class: "stack" },
    pageHead({
      title: "Service",
      actions: btn("Back to services", { href: "/portal/services", size: "sm", icon: "arrowLeft" }),
    }),
    card({
      flush: true,
      body: emptyState({
        title: "This service is not available",
        body: "It may have been cancelled, or it may belong to another account. Your services list has everything you can see.",
        icon: "lock",
        action: btn("Your services", { href: "/portal/services", size: "sm", icon: "server" }),
      }),
    }),
  );
}
