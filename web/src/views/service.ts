// One service.
//
// The screen an operator opens when a customer says "my server is down" or "why was I cut
// off". It answers, in order: what is this, what state is it in, what is the queue doing
// with it, what is holding it, and what has it been billed.
//
// "What is this" is its own read (`GET /api/v1/staff/services/{id}/metadata`) rather than a
// field on the detail response, because it is the same object the customer's own page shows and
// the same object an invoice can print from. The card renders the *effective* label and
// description — the ones the server derived from the product and the selected options when
// nobody wrote any — so a support agent never reads an empty heading, and the edit dialog offers
// the derived text as the starting point.
//
// Three rules shape the actions:
//
//  - Suspension eligibility is a *deliberate* button, never a page-load side effect. The
//    endpoint writes a suspension_decisions row, so calling it on render would fill the
//    audit trail with noise and make "who decided this" unanswerable.
//  - Suspend, resume and terminate are queued (202). The page polls the operation with
//    backoff for ~30s and renders the transition, because "queued" is not an answer.
//  - Every refusal is rendered as itself: a hold is not an ineligibility, and an approval
//    is not an error. Each one names the next step instead of a generic failure toast.

import { ApiError } from "../lib/api";
import { h, type Child } from "../lib/dom";
import { dateTime, humanStatus, relative, shortId, titleCase, truncate } from "../lib/format";
import type { Amount } from "../lib/format";
import { icon, type IconName } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  badge,
  banner,
  btn,
  card,
  copyButton,
  emptyState,
  errorState,
  field,
  input,
  metaGrid,
  moneyEl,
  mono,
  pageHead,
  select,
  skeletonPage,
  spinner,
  statusBadge,
  tabs,
  textarea,
} from "../ui/kit";

export const meta = { title: "Service" };

// ---------------------------------------------------------------- payloads --

interface CustomerRef {
  id: string;
  email: string;
}

interface Service {
  id: string;
  customer: CustomerRef;
  product: { id: string; name: string };
  status: string;
  external_id: string;
  generation: number;
  next_bill_at: string | null;
  created_at: string;
  open_operations: number;
  last_error: string | null;
}

interface Subscription {
  id: string;
  status: string;
  interval: string;
  product: string;
  next_billing_at: string;
  current_period_start: string;
  current_period_end: string;
  cancel_at_period_end: boolean;
}

interface Operation {
  id: string;
  service: { id: string; customer: CustomerRef };
  kind: string;
  operation_key: string;
  generation: number;
  status: string;
  attempts: number;
  last_error: string | null;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
  lease_expires_at?: string | null;
}

interface Hold {
  id: string;
  service_id: string;
  reason: string;
  expires_at: string | null;
  created_by: string;
  created_at: string;
}

interface Invoice {
  id: string;
  number: string;
  status: string;
  currency: string;
  total: Amount;
  paid: Amount;
  due: Amount;
  overdue: boolean;
  issued_at: string | null;
  due_at: string | null;
  customer?: CustomerRef;
}

interface ServiceDetail {
  service: Service;
  subscription: Subscription | null;
  operations: Operation[];
  holds: Hold[];
  invoices: Invoice[];
}

/** One property with its definition attached (SVC-08). `show_on_invoice` is the definition's
 *  decision about documents; `customer_visible` is a separate question with a separate answer. */
interface ServiceProperty {
  key: string;
  label: string;
  description: string;
  value: string;
  show_on_invoice: boolean;
  customer_visible: boolean;
  sort_order: number;
}

/** GET /api/v1/staff/services/{id}/metadata. `label` and `description` are what a human wrote
 *  and may be empty; the `effective_*` fields are what to display and are never empty. */
interface ServiceMetadata {
  service_id: string;
  status: string;
  customer: { id: string; email: string };
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
  order_id?: string;
  label: string;
  description: string;
  effective_label: string;
  effective_description: string;
  derived_description: string;
  custom_properties: Record<string, string>;
  properties: ServiceProperty[];
  config: Array<{ config_option_id: string; name: string; value: string }>;
  updated_at: string;
}

/** One line under a property's field: what the two visibility flags mean for this key.
 *
 *  They are separate on purpose and confusing when conflated. `customer_visible` decides whether
 *  the customer's portal shows the property at all; `show_on_invoice` decides whether it prints
 *  on a document they keep. A property can be visible and not printed — an internal note the
 *  customer may read but that does not belong on an invoice — and the reverse is a property that
 *  appears on a document without appearing anywhere else, which is worth naming because it is
 *  almost always a mistake.
 *
 *  Both facts live on the definition, so the hint is read-only text rather than a control: a
 *  property's document behaviour is a catalogue decision, not something a support agent edits
 *  while looking at one service.
 */
function propertyHint(def: PropertyDefinition): string {
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

/** One configured property definition, as the staff properties screen edits it. */
interface PropertyDefinition {
  key: string;
  label: string;
  description: string;
  show_on_invoice: boolean;
  customer_visible: boolean;
  sort_order: number;
}

/** GET /api/v1/staff/services/{id}/suspension-eligibility. `outstanding` is the server's
 *  formatted amount ("USD 1,240.00"), not an Amount, because the engine compares across
 *  currencies before it formats. */
interface Eligibility {
  service_id: string;
  eligible: boolean;
  reason: string;
  generation: number;
  invoice_id?: string;
  outstanding?: string;
}

/** The 202 body of a queued action. */
interface Accepted {
  operation_id: string;
  status_url: string;
}

/** One action an installed extension contributes to a service (SVC-11).
 *
 *  The extension declares it in `ui.describe`, the core refuses a declaration whose permission
 *  it does not itself declare, and the call is checked against the *declared* permission rather
 *  than the route's. So a button the session may not press is never rendered, and hiding it is
 *  presentation rather than enforcement. */
interface ExtensionAction {
  extension: string;
  id: string;
  label: string;
  method: string;
  permission: string;
  /** Confirmation copy the extension asked for, when it asks for one. */
  confirm?: string;
}

interface HooksResponse {
  nav_items: unknown[] | null;
  service_actions: ExtensionAction[] | null;
}

/** One input the configured provisioner needs per purchase (UPG-12).
 *
 *  The key is also the metadata key the adapter reads on create and upgrade, so the form, the
 *  stored value and the adapter cannot disagree about what a field is called. */
interface ProvisioningField {
  key: string;
  label: string;
  type: string;
  required: boolean;
  secret: boolean;
  choices?: string[];
  hint?: string;
}

/** GET /api/v1/staff/services/{id}/provisioning-fields. */
interface ProvisioningFields {
  service_id: string;
  provider: string;
  extension: string;
  configured: boolean;
  supported: boolean;
  fields: ProvisioningField[] | null;
  values: Record<string, string>;
  capabilities?: string[];
}

/** One side of a price recalculation (SVC-13). Every amount is the API's money shape. */
interface RecalcSide {
  price_version_id: string;
  version: number;
  name: string;
  interval: string;
  currency: string;
  plan: Amount;
  options: Amount;
  setup: Amount;
  total: Amount;
  plan_id: string;
  region: string;
}

/** POST /api/v1/staff/services/{id}/recalculate-price. */
interface RecalcResult {
  service_id: string;
  applied: boolean;
  changed: boolean;
  plan_changed: boolean;
  repriced_options: number;
  from: RecalcSide;
  to: RecalcSide;
  operation_id?: string;
}

interface Failure {
  kind: string;
  code: string;
  status: number;
  title: string;
  detail: string;
  approvalId?: string;
  outstanding?: unknown;
}

interface Transition {
  kind: string;
  operationId: string;
  operation: Operation | null;
  state: "polling" | "settled" | "timeout";
  startedAt: number;
}

type TabKey = "overview" | "operations" | "holds" | "invoices";

const TERMINAL = new Set(["succeeded", "failed", "needs_review", "unknown"]);
const POLL_BUDGET_MS = 30_000;

// -------------------------------------------------------------------- page --

export default async function servicePage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("service:read:all")) {
    return h(
      "div",
      { class: "page" },
      errorState(
        new Error("Your account cannot read services. Ask an administrator for the service:read:all permission."),
      ),
    );
  }

  const id = ctx.route.params.id ?? "";
  const host = h("div", null, h("div", { class: "page" }, skeletonPage()));
  const controller = new AbortController();
  ctx.onDispose(() => controller.abort());

  // Timers are tracked so navigation cannot leave a poll running (or a promise dangling).
  const timers = new Map<number, () => void>();
  ctx.onDispose(() => {
    for (const [timer, resolve] of timers) {
      window.clearTimeout(timer);
      resolve();
    }
    timers.clear();
  });

  let detail: ServiceDetail | null = null;
  let metadata: ServiceMetadata | null = null;
  let eligibility: Eligibility | null = null;
  let eligibilityError: string | null = null;
  let eligibilityBusy = false;
  let failure: Failure | null = null;
  let transition: Transition | null = null;
  let tab: TabKey = tabFrom(ctx.route.query.get("tab"));
  let extensionActions: ExtensionAction[] = [];
  let provisioning: ProvisioningFields | null = null;

  const sleep = (ms: number): Promise<void> =>
    new Promise((resolve) => {
      const timer = window.setTimeout(() => {
        timers.delete(timer);
        resolve();
      }, ms);
      timers.set(timer, resolve);
    });

  const paint = () => {
    host.replaceChildren(renderPage());
  };

  // ------------------------------------------------------------------ load --

  /**
   * The metadata read is separate from the detail read, and a failure is not a page failure:
   * the rest of the screen (the queue, the holds, the invoices) is still the answer to "why was I
   * cut off", and a support agent must not lose it because one endpoint is unhappy. A missing
   * metadata object simply omits the card.
   */
  const loadMetadata = async (): Promise<void> => {
    try {
      metadata = await ctx.api.get<ServiceMetadata>(`/api/v1/staff/services/${id}/metadata`, {
        cache: false,
        signal: controller.signal,
      });
    } catch (err) {
      if (isAbort(err)) return;
      metadata = null;
    }
  };

  /**
   * The extension-contributed actions (SVC-11) and the provisioner's own inputs (UPG-12).
   *
   * Both are tolerant reads: a screen about "why is my server down" must not lose the queue and
   * the holds because an extension is not installed or a panel is unreachable. A missing list
   * simply omits its card.
   */
  const loadSideReads = async (): Promise<void> => {
    try {
      const hooks = await ctx.api.get<HooksResponse>("/api/v1/extensions/hooks", {
        cache: false,
        signal: controller.signal,
      });
      extensionActions = (hooks.service_actions ?? []).filter((action) => ctx.can(action.permission));
    } catch (err) {
      if (isAbort(err)) return;
      extensionActions = [];
    }
    try {
      provisioning = await ctx.api.get<ProvisioningFields>(`/api/v1/staff/services/${id}/provisioning-fields`, {
        cache: false,
        signal: controller.signal,
      });
    } catch (err) {
      if (isAbort(err)) return;
      provisioning = null;
    }
  };

  const load = async (): Promise<void> => {
    try {
      const next = await ctx.api.get<ServiceDetail>(`/api/v1/staff/services/${id}`, {
        cache: false,
        signal: controller.signal,
      });
      if (controller.signal.aborted) return;
      detail = next;
      await loadMetadata();
      await loadSideReads();
      if (controller.signal.aborted) return;
      describe(next.service);
      paint();
    } catch (err) {
      if (controller.signal.aborted || isAbort(err)) return;
      host.replaceChildren(
        h(
          "div",
          { class: "page" },
          errorState(err, () => {
            host.replaceChildren(h("div", { class: "page" }, skeletonPage()));
            void load();
          }),
        ),
      );
    }
  };

  /** A silent re-read after a mutation: the transition panel stays, the facts refresh. */
  const refresh = async (): Promise<void> => {
    try {
      const next = await ctx.api.get<ServiceDetail>(`/api/v1/staff/services/${id}`, {
        cache: false,
        signal: controller.signal,
      });
      if (controller.signal.aborted) return;
      detail = next;
      await loadMetadata();
      await loadSideReads();
      if (controller.signal.aborted) return;
      describe(next.service);
      paint();
    } catch (err) {
      if (controller.signal.aborted || isAbort(err)) return;
      ctx.toast("Could not reload the service", {
        tone: "danger",
        detail: err instanceof ApiError ? err.detail || err.code : String(err),
      });
    }
  };

  const describe = (svc: Service) => {
    // The label the customer chose is the name of this service. Falling back to the external id
    // is right for a service nobody has named, because that is the identifier the customer quotes
    // in a ticket; a label exists precisely so it can be quoted instead.
    const title = metadata?.effective_label || nameOf(svc);
    ctx.setTitle(title);
    ctx.setCrumbs([{ label: "Services", href: "/services" }, { label: title }]);
  };

  // A committed change to this service (the worker suspending it, a colleague adding a hold)
  // re-reads the page. The feed is per-permission, so nothing here leaks a record the
  // session could not already fetch.
  ctx.onLive((event) => {
    if (!event.kind.startsWith("service.")) return;
    if (String(event.service_id ?? "") !== id) return;
    void refresh();
  });

  // --------------------------------------------------------------- actions --

  const recordFailure = (kind: string, err: unknown) => {
    failure = failureFrom(kind, err);
    paint();
    // The four refusals a person must read are rendered in place with a next step; a toast
    // would scroll away before they finished reading it.
    if (!["hold_in_place", "approval_required", "not_eligible", "still_owing"].includes(failure.code)) {
      ctx.toast(failure.title, { tone: "danger", detail: failure.code });
    }
  };

  const postAction = async (kind: "suspend" | "resume", body: Record<string, unknown>) => {
    failure = null;
    try {
      const accepted = await ctx.api.post<Accepted>(`/api/v1/staff/services/${id}/${kind}`, body);
      ctx.toast(`${titleCase(kind)} queued`, { tone: "success", detail: `Operation ${shortId(accepted.operation_id)}` });
      startTransition(kind, accepted.operation_id);
    } catch (err) {
      recordFailure(kind, err);
    }
  };

  const suspend = async () => {
    if (!detail) return;
    const reason = textarea({ id: "suspend-reason", placeholder: "Why is this service being suspended?" });
    const ok = await ctx.confirm({
      title: "Suspend this service?",
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "p",
          null,
          "The dunning engine re-checks the account before it acts: it refuses when the customer owes nothing, or when a manual hold is in place. This page will say which.",
        ),
        field({
          label: "Reason",
          hint: "Recorded on the suspension decision and in the audit log.",
          control: reason,
        }),
      ),
      confirmLabel: "Suspend service",
      danger: true,
      note: "The suspension is queued as an operation, so it is not immediate.",
    });
    if (!ok) return;
    await postAction("suspend", {
      reason: reason.value.trim(),
      override_hold: false,
      approval_id: "",
      request_approval: false,
    });
  };

  const resume = async () => {
    if (!detail) return;
    const reason = textarea({ id: "resume-reason", placeholder: "Why is this service being resumed?" });
    const ok = await ctx.confirm({
      title: "Resume this service?",
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "p",
          null,
          "Resume is refused while the customer owes anything at all, or while a manual hold is in place. That is stricter than one paid invoice on purpose: a second overdue invoice would suspend the service again within the hour.",
        ),
        field({ label: "Reason", hint: "Recorded on the decision and in the audit log.", control: reason }),
      ),
      confirmLabel: "Resume service",
    });
    if (!ok) return;
    await postAction("resume", {
      reason: reason.value.trim(),
      override_hold: false,
      approval_id: "",
      request_approval: false,
    });
  };

  const requestOverride = async () => {
    try {
      const accepted = await ctx.api.post<Accepted>(`/api/v1/staff/services/${id}/suspend`, {
        reason: "Override the manual hold (requested from the staff panel)",
        override_hold: true,
        approval_id: "",
        request_approval: true,
      });
      // A 2xx means the override went through without a second actor, which the server only
      // does when the hold has gone; treat it as an ordinary enqueue.
      ctx.toast("Suspension queued", { tone: "success", detail: `Operation ${shortId(accepted.operation_id)}` });
      startTransition("suspend", accepted.operation_id);
    } catch (err) {
      if (err instanceof ApiError && err.code === "approval_required") {
        const approvalId = approvalIdOf(err);
        failure = {
          kind: "suspend",
          code: "approval_required",
          status: err.status,
          title: "Approval required",
          detail: err.detail,
          approvalId,
        };
        paint();
        ctx.toast("Approval requested", {
          tone: "info",
          detail: approvalId
            ? `Approval ${shortId(approvalId)} needs a different staff member.`
            : "A second staff member must approve the override.",
        });
        return;
      }
      recordFailure("suspend", err);
    }
  };

  const overrideWithApproval = async () => {
    const approval = input({ id: "approval-id", class: "input--mono", placeholder: "approval uuid", autocomplete: "off" });
    await ctx.formDialog({
      title: "Override the hold with an approval",
      subtitle: "The approval must have been granted by a different staff member, for this service.",
      body: field({ label: "Approval id", required: true, control: approval }),
      submitLabel: "Suspend with this approval",
      danger: true,
      onSubmit: async () => {
        const value = approval.value.trim();
        if (!value) return "Paste the approval id.";
        try {
          const accepted = await ctx.api.post<Accepted>(`/api/v1/staff/services/${id}/suspend`, {
            reason: "Override the manual hold (approved)",
            override_hold: true,
            approval_id: value,
            request_approval: false,
          });
          failure = null;
          ctx.toast("Suspension queued", { tone: "success", detail: `Operation ${shortId(accepted.operation_id)}` });
          startTransition("suspend", accepted.operation_id);
        } catch (err) {
          if (err instanceof ApiError) return err.detail || humanStatus(err.code);
          return String(err);
        }
        return;
      },
    });
  };

  const addHold = async () => {
    const reason = textarea({ id: "hold-reason", placeholder: "Why is this hold being placed?" });
    const expiry = selectExpiry();
    await ctx.formDialog({
      title: "Add a manual hold",
      subtitle: "A hold stops automatic suspension and automatic resume until it is cleared.",
      body: h(
        "div",
        { class: "stack" },
        field({
          label: "Reason",
          required: true,
          hint: "At least 3 characters. Every operator who opens this service sees it.",
          control: reason,
        }),
        field({
          label: "Expires",
          hint: "A hold with no expiry is deliberate: it blocks automation until a human clears it.",
          control: expiry,
        }),
      ),
      submitLabel: "Add hold",
      onSubmit: async () => {
        const text = reason.value.trim();
        if (text.length < 3) return "A hold needs a reason of at least 3 characters.";
        try {
          await ctx.api.post(`/api/v1/staff/services/${id}/holds`, {
            reason: text,
            expires_in_seconds: Number(expiry.value) || 0,
          });
        } catch (err) {
          if (err instanceof ApiError) return err.detail || humanStatus(err.code);
          return String(err);
        }
        ctx.toast("Hold added", { tone: "success", detail: truncate(text, 80) });
        await refresh();
        return;
      },
    });
  };

  const clearHold = async (hold: Hold) => {
    const ok = await ctx.confirm({
      title: "Clear this hold?",
      body: h(
        "div",
        { class: "stack-sm" },
        h("p", null, "Clearing the hold re-enables automatic suspension and resume for this service."),
        h("p", { class: "mono" }, hold.reason),
      ),
      confirmLabel: "Clear hold",
    });
    if (!ok) return;
    failure = null;
    try {
      await ctx.api.del(`/api/v1/staff/holds/${hold.id}`);
      ctx.toast("Hold cleared", { tone: "success", detail: truncate(hold.reason, 80) });
      await refresh();
    } catch (err) {
      recordFailure("clear-hold", err);
    }
  };

  const terminate = async (svc: Service) => {
    // The phrase is the identifier the operator already sees on the page: external id when
    // the service has one, otherwise the internal id. The server accepts either.
    const phrase = nameOf(svc);
    const reason = textarea({ id: "terminate-reason", placeholder: "Why is this service being terminated?" });
    const ok = await ctx.confirm({
      title: "Terminate this service?",
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "p",
          null,
          "Termination destroys the provider's resource and stops billing. It is queued as an operation, it cannot be reconciled automatically, and it cannot be undone.",
        ),
        h("p", { class: "faint" }, `Service ${phrase} · ${svc.customer.email} · ${svc.product.name}`),
        field({ label: "Reason", hint: "Recorded in the audit log.", control: reason }),
      ),
      phrase,
      danger: true,
      confirmLabel: "Terminate service",
      note: "Type the service identifier exactly. A mis-click cannot terminate a service; this can.",
    });
    if (!ok) return;
    failure = null;
    try {
      const accepted = await ctx.api.post<Accepted>(`/api/v1/staff/services/${id}/terminate`, {
        confirm: phrase,
        reason: reason.value.trim(),
      });
      ctx.toast("Termination queued", {
        tone: "success",
        detail: `Operation ${shortId(accepted.operation_id)}`,
      });
      startTransition("terminate", accepted.operation_id);
    } catch (err) {
      recordFailure("terminate", err);
    }
  };

  // -------------------------------------------------------------- metadata --

  /**
   * The metadata edit dialog (SVC-03, SVC-08).
   *
   * One dialog rather than a separate screen, because the three fields are one answer to one
   * question. The property fields are generated from the installation's definitions, not from the
   * service's values: a property the service does not carry yet still has to be settable, and a
   * value with no definition is refused by the server anyway.
   */
  const editMetadata = async () => {
    const current = metadata;
    if (!current) return;

    let definitions: PropertyDefinition[] = [];
    try {
      const res = await ctx.api.get<{ properties: PropertyDefinition[] }>(
        "/api/v1/staff/service-properties",
        { cache: false },
      );
      definitions = res.properties ?? [];
    } catch (err) {
      ctx.toast("Could not load the property definitions", {
        tone: "danger",
        detail: err instanceof ApiError ? err.detail || err.code : String(err),
      });
      return;
    }

    const labelInput = input({
      id: "metadata-label",
      value: current.label,
      placeholder: current.product.name,
      maxlength: 120,
      autocomplete: "off",
    });
    const descriptionInput = textarea({
      id: "metadata-description",
      value: current.description,
      placeholder: current.derived_description,
      maxlength: 2000,
    });
    const propertyFields = definitions.map((definition) => ({
      definition,
      control: input({
        id: `metadata-property-${definition.key}`,
        value: current.custom_properties[definition.key] ?? "",
        maxlength: 200,
        autocomplete: "off",
      }),
    }));

    await ctx.formDialog({
      title: "Edit service metadata",
      subtitle: "The customer reads the label and the description on their own service page.",
      submitLabel: "Save metadata",
      body: h(
        "div",
        { class: "stack" },
        field({
          label: "Label",
          hint: "Shown instead of the product name. Leave it empty to use the product's name.",
          control: labelInput,
        }),
        field({
          label: "Description",
          hint: `Leave it empty to use the derived description: ${truncate(current.derived_description, 140)}`,
          control: descriptionInput,
        }),
        propertyFields.length > 0
          ? h(
              "div",
              { class: "stack-sm" },
              h("p", { class: "field-hint" }, "Properties"),
              ...propertyFields.map((entry) =>
                field({
                  label: entry.definition.label,
                  hint: propertyHint(entry.definition),
                  control: entry.control,
                }),
              ),
              h(
                "p",
                { class: "field-hint" },
                "An empty property is unset. Whether a property prints on an invoice is decided on its definition, not here.",
              ),
            )
          : h(
              "p",
              { class: "field-hint" },
              "No custom properties are defined on this installation.",
            ),
      ),
      onSubmit: async () => {
        // Every field is sent, including the untouched ones: an empty value unsets a property, and
        // the form shows the current value, so a faithful edit is exactly what the operator sees.
        const properties: Record<string, string> = {};
        for (const entry of propertyFields) {
          properties[entry.definition.key] = entry.control.value.trim();
        }
        try {
          await ctx.api.put(`/api/v1/staff/services/${id}/metadata`, {
            label: labelInput.value.trim(),
            description: descriptionInput.value.trim(),
            custom_properties: properties,
          });
        } catch (err) {
          if (err instanceof ApiError) return err.detail || humanStatus(err.code);
          return String(err);
        }
        ctx.toast("Metadata saved", {
          tone: "success",
          detail: labelInput.value.trim() || current.product.name,
        });
        await refresh();
        return;
      },
    });
  };

  const checkEligibility = async () => {
    if (eligibilityBusy) return;
    // Deliberate: this endpoint records a suspension_decisions row. It must never run on
    // load, and it must never run because a panel re-rendered.
    eligibilityBusy = true;
    paint();
    try {
      eligibility = await ctx.api.get<Eligibility>(`/api/v1/staff/services/${id}/suspension-eligibility`, {
        cache: false,
        signal: controller.signal,
      });
      eligibilityError = null;
    } catch (err) {
      eligibility = null;
      eligibilityError = err instanceof ApiError ? err.detail || humanStatus(err.code) : String(err);
    } finally {
      eligibilityBusy = false;
    }
    if (!controller.signal.aborted) paint();
  };

  // --------------------------------------------------------------- polling --

  const startTransition = (kind: string, operationId: string) => {
    transition = { kind, operationId, operation: null, state: "polling", startedAt: Date.now() };
    paint();
    void pollTransition(transition);
  };

  const pollTransition = async (current: Transition): Promise<void> => {
    let delay = 400;
    while (!controller.signal.aborted && transition === current && Date.now() - current.startedAt < POLL_BUDGET_MS) {
      await sleep(delay);
      if (controller.signal.aborted || transition !== current) return;
      try {
        const operation = await ctx.api.get<Operation>(`/api/v1/staff/operations/${current.operationId}`, {
          cache: false,
          signal: controller.signal,
        });
        current.operation = operation;
        if (TERMINAL.has(operation.status)) {
          current.state = "settled";
          paint();
          const ok = operation.status === "succeeded";
          ctx.toast(`${titleCase(current.kind)} ${ok ? "succeeded" : humanStatus(operation.status).toLowerCase()}`, {
            tone: ok ? "success" : "danger",
            detail: operation.last_error || `Operation ${shortId(operation.id)}`,
          });
          await refresh();
          return;
        }
        paint();
      } catch (err) {
        if (controller.signal.aborted || isAbort(err)) return;
        // A read that fails mid-poll is not a failed operation: keep the panel and retry.
      }
      delay = Math.min(Math.round(delay * 1.6), 4000);
    }
    if (transition === current && current.state === "polling") {
      current.state = "timeout";
      paint();
    }
  };

  // --------------------------------------------------- operator actions --

  /**
   * One extension-contributed action (SVC-11).
   *
   * The confirmation copy is the extension's, because only it knows what the action does. The
   * call is the core's, because the core is what checks the declared permission — a client that
   * posted to an extension directly would be trusting the extension's own authorisation.
   */
  const runExtensionAction = async (action: ExtensionAction): Promise<void> => {
    if (action.confirm) {
      const ok = await ctx.confirm({
        title: action.label,
        body: h("p", null, action.confirm),
        confirmLabel: action.label,
      });
      if (!ok) return;
    }
    failure = null;
    try {
      await ctx.api.post(
        `/api/v1/staff/services/${id}/hooks/${encodeURIComponent(action.extension)}/${encodeURIComponent(action.id)}`,
        {},
      );
      ctx.toast(`${action.label} sent`, { tone: "success", detail: `Handled by ${action.extension}` });
      await refresh();
    } catch (err) {
      recordFailure("extension", err);
    }
  };

  /**
   * Recalculate the price from the catalogue (SVC-13).
   *
   * It reads first and writes second, in one flow: the operator sees the exact difference before
   * anything moves, because a recalculation can raise a customer's next bill and the preview is
   * the only place that is stated in money rather than in a paragraph.
   */
  const recalculatePrice = async (): Promise<void> => {
    failure = null;
    let preview: RecalcResult;
    try {
      preview = await ctx.api.post<RecalcResult>(`/api/v1/staff/services/${id}/recalculate-price`, {
        apply: false,
      });
    } catch (err) {
      recordFailure("recalculate", err);
      return;
    }
    if (!preview.changed) {
      ctx.toast("Already at the catalogue price", {
        tone: "info",
        detail: `${preview.to.name || preview.to.price_version_id} · ${preview.to.total.display ?? ""}`,
      });
      return;
    }
    const body = h(
      "div",
      { class: "stack-sm" },
      h(
        "p",
        null,
        "The catalogue now prices this service differently from the subscription. Applying it moves the subscription to the catalogue's current version and re-prices the selected options.",
      ),
      h(
        "div",
        { class: "kv" },
        h("span", { class: "kv-key" }, "Now"),
        h("span", { class: "kv-value" }, `${preview.from.name || preview.from.price_version_id} · `, moneyEl(preview.from.total)),
      ),
      h(
        "div",
        { class: "kv" },
        h("span", { class: "kv-key" }, "Catalogue"),
        h("span", { class: "kv-value" }, `${preview.to.name || preview.to.price_version_id} · `, moneyEl(preview.to.total)),
      ),
      preview.plan_changed
        ? h(
            "p",
            { class: "field-hint" },
            `The provisioner plan changes from ${preview.from.plan_id || "none"} to ${preview.to.plan_id || "none"}, so a resize is queued.`,
          )
        : h("p", { class: "field-hint" }, "The provisioner plan is unchanged, so nothing is sent to the server."),
    );
    const ok = await ctx.confirm({
      title: "Apply the catalogue price?",
      body,
      confirmLabel: "Recalculate and apply",
    });
    if (!ok) return;
    try {
      const applied = await ctx.api.post<RecalcResult>(`/api/v1/staff/services/${id}/recalculate-price`, {
        apply: true,
      });
      ctx.toast("Price recalculated", {
        tone: "success",
        detail: applied.operation_id
          ? `Resize queued as operation ${shortId(applied.operation_id)}`
          : `${applied.repriced_options} option(s) re-priced`,
      });
      await refresh();
    } catch (err) {
      recordFailure("recalculate", err);
    }
  };

  /** Cancel the provider-side subscription (SVC-14). */
  const cancelExternalSubscription = async (): Promise<void> => {
    const subscriptionId = input({
      id: "external-subscription",
      class: "input--mono",
      placeholder: "the provider's subscription id",
      autocomplete: "off",
    });
    const reason = textarea({ id: "external-subscription-reason", placeholder: "Why is it being cancelled?" });
    await ctx.formDialog({
      title: "Cancel the provider-side subscription",
      subtitle:
        "The gateway is told to stop billing the customer. The panel's own billing schedule is untouched — cancel the service for that.",
      body: h(
        "div",
        { class: "stack" },
        field({
          label: "Subscription id",
          hint: "The id the payment provider holds. Leave it empty to use the one already recorded on this service.",
          control: subscriptionId,
        }),
        field({ label: "Reason", hint: "Recorded in the audit log.", control: reason }),
      ),
      submitLabel: "Queue the cancellation",
      onSubmit: async () => {
        try {
          const accepted = await ctx.api.post<Accepted>(`/api/v1/staff/services/${id}/cancel-subscription`, {
            subscription_id: subscriptionId.value.trim(),
            reason: reason.value.trim(),
          });
          ctx.toast("Cancellation queued", {
            tone: "success",
            detail: `Operation ${shortId(accepted.operation_id)}`,
          });
          await refresh();
        } catch (err) {
          if (err instanceof ApiError) return err.detail || humanStatus(err.code);
          return String(err);
        }
        return;
      },
    });
  };

  /** Destroy the provider's resource and keep the service (SVC-12). */
  const deleteRemote = async (svc: Service): Promise<void> => {
    const reason = textarea({ id: "delete-remote-reason", placeholder: "Why is the server being removed?" });
    const ok = await ctx.confirm({
      title: "Delete the server at the provider?",
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "p",
          null,
          "The provider's resource is destroyed and this service forgets its id. The service, its billing and its status are untouched — the customer keeps what they bought. This is not a termination.",
        ),
        h("p", { class: "faint" }, `Service ${nameOf(svc)} · ${svc.customer.email} · ${svc.product.name}`),
        field({ label: "Reason", hint: "Recorded in the audit log.", control: reason }),
      ),
      danger: true,
      confirmLabel: "Delete the remote resource",
    });
    if (!ok) return;
    failure = null;
    try {
      const accepted = await ctx.api.post<Accepted>(`/api/v1/staff/services/${id}/delete-remote`, {
        reason: reason.value.trim(),
      });
      ctx.toast("Remote deletion queued", {
        tone: "success",
        detail: `Operation ${shortId(accepted.operation_id)}`,
      });
      startTransition("Remote deletion", accepted.operation_id);
    } catch (err) {
      recordFailure("delete-remote", err);
    }
  };

  /** Send the customer a message about this service (SVC-12). */
  const notifyCustomer = async (svc: Service): Promise<void> => {
    const subject = input({ id: "notify-subject", value: `${svc.product.name} — update`, maxlength: 200 });
    const body = textarea({ id: "notify-body", maxlength: 8000, placeholder: "What does the customer need to know?" });
    await ctx.formDialog({
      title: "Message the customer",
      subtitle: `Sent to ${svc.customer.email} and shown in their portal inbox.`,
      body: h(
        "div",
        { class: "stack" },
        field({ label: "Subject", required: true, control: subject }),
        field({ label: "Message", required: true, control: body }),
      ),
      submitLabel: "Send the message",
      onSubmit: async () => {
        if (subject.value.trim() === "") return "A subject is required.";
        if (body.value.trim() === "") return "A message is required.";
        try {
          await ctx.api.post(`/api/v1/staff/services/${id}/notify`, {
            subject: subject.value.trim(),
            body: body.value.trim(),
          });
        } catch (err) {
          if (err instanceof ApiError) return err.detail || humanStatus(err.code);
          return String(err);
        }
        ctx.toast("Message queued", { tone: "success", detail: subject.value.trim() });
        return;
      },
    });
  };

  /** Delete a service that has no history (SVC-12). */
  const deleteService = async (svc: Service): Promise<void> => {
    const reason = textarea({ id: "delete-service-reason", placeholder: "Why is this service being deleted?" });
    const ok = await ctx.confirm({
      title: "Delete this service?",
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "p",
          null,
          "Only a service with no invoices, operations, upgrades, cancellations, holds, tickets or suspension decisions behind it can be deleted. Anything else is refused — terminate it instead, which keeps the record.",
        ),
        h("p", { class: "faint" }, `Service ${nameOf(svc)} · ${svc.customer.email}`),
        field({ label: "Reason", control: reason }),
      ),
      danger: true,
      confirmLabel: "Delete the service",
    });
    if (!ok) return;
    failure = null;
    try {
      await ctx.api.del(`/api/v1/staff/services/${id}`, { reason: reason.value.trim() });
      ctx.toast("Service deleted", { tone: "success", detail: nameOf(svc) });
      ctx.navigate(`/customers/${svc.customer.id}`);
    } catch (err) {
      recordFailure("delete-service", err);
    }
  };

  /** Store the provisioner's per-purchase inputs (UPG-12). */
  const editProvisioningFields = async (): Promise<void> => {
    const current = provisioning;
    if (!current) return;
    const inputs = new Map<string, HTMLInputElement | HTMLTextAreaElement>();
    const rows: Node[] = (current.fields ?? []).map((f) => {
      const value = current.values[f.key] ?? "";
      const control =
        f.type === "password"
          ? input({ type: "password", id: `pf-${f.key}`, name: f.key, value, autocomplete: "off" })
          : input({ type: "text", id: `pf-${f.key}`, name: f.key, value, autocomplete: "off" });
      inputs.set(f.key, control);
      return field({
        label: f.label,
        required: f.required,
        hint: f.hint,
        control,
      });
    });
    await ctx.formDialog({
      title: "Provisioning inputs",
      subtitle: `These are the values the ${current.provider} adapter needs to build this service. They are sent to it verbatim.`,
      body: h("div", { class: "stack" }, ...rows),
      submitLabel: "Save inputs",
      onSubmit: async () => {
        const values: Record<string, string> = {};
        for (const [key, el] of inputs) values[key] = el.value.trim();
        try {
          await ctx.api.put(`/api/v1/staff/services/${id}/provisioning-fields`, { values });
        } catch (err) {
          if (err instanceof ApiError) return err.detail || humanStatus(err.code);
          return String(err);
        }
        ctx.toast("Provisioning inputs saved", { tone: "success" });
        await refresh();
        return;
      },
    });
  };

  // --------------------------------------------------------------- renders --

  const renderPage = (): Node => {
    const data = detail;
    if (!data) return skeletonPage();
    const svc = data.service;

    return h(
      "div",
      { class: "page" },
      pageHead({
        title: metadata?.effective_label || nameOf(svc),
        badge: serviceStatusBadge(svc.status),
        sub: h(
          "span",
          { class: "row-wrap" },
          h("a", { class: "linkish", href: `/customers/${svc.customer.id}` }, svc.customer.email),
          h("span", { class: "faint" }, "·"),
          h("a", { class: "linkish", href: `/catalog-management/${svc.product.id}` }, svc.product.name),
          h("span", { class: "faint" }, "·"),
          h("span", null, `generation ${svc.generation}`),
          h("span", { class: "faint" }, "·"),
          h("span", { title: dateTime(svc.created_at) }, `created ${relative(svc.created_at)}`),
        ),
        actions: actionButtons(svc),
      }),
      failure ? failureBanner(failure) : null,
      eligibilityBanner(),
      transition ? transitionBanner(transition) : null,
      tabsNav(data),
      tabBody(data),
      dangerZone(svc),
    );
  };

  const actionButtons = (svc: Service): Node => {
    const items: Node[] = [];
    items.push(
      btn("Check suspension eligibility", {
        size: "sm",
        icon: "shieldCheck",
        title: "Evaluates eligibility and records a suspension decision",
        disabled: eligibilityBusy,
        onclick: () => void checkEligibility(),
      }),
    );
    if (ctx.can("service:hold") && svc.status !== "terminated") {
      items.push(btn("Add hold", { size: "sm", icon: "lock", onclick: () => void addHold() }));
    }
    if (ctx.can("service:suspend")) {
      if (svc.status === "active") {
        items.push(btn("Suspend", { size: "sm", icon: "pauseCircle", onclick: () => void suspend() }));
      }
      if (svc.status === "suspended") {
        items.push(btn("Resume", { size: "sm", icon: "playCircle", onclick: () => void resume() }));
      }
    }
    return h("div", { class: "row-wrap" }, ...items);
  };

  const failureBanner = (f: Failure): Node => {
    const extra: Node[] = [];
    if (f.kind === "suspend" && f.code === "hold_in_place") {
      extra.push(
        btn("Show the hold", {
          size: "sm",
          icon: "lock",
          onclick: () => {
            tab = "holds";
            ctx.replaceQuery({ tab: "holds" });
            paint();
          },
        }),
      );
      extra.push(
        btn("Request an override approval", {
          size: "sm",
          icon: "shieldCheck",
          onclick: () => void requestOverride(),
        }),
      );
    }
    if (f.kind === "suspend" && f.code === "approval_required") {
      extra.push(btn("Enter an approval id", { size: "sm", icon: "key", onclick: () => void overrideWithApproval() }));
    }
    if (f.code === "not_eligible" || f.code === "still_owing") {
      extra.push(btn("Re-check eligibility", { size: "sm", icon: "refresh", onclick: () => void checkEligibility() }));
    }
    extra.push(
      btn("Dismiss", {
        size: "sm",
        variant: "ghost",
        onclick: () => {
          failure = null;
          paint();
        },
      }),
    );

    const outstanding = outstandingNode(f.outstanding);
    const body = h(
      "div",
      { class: "stack-sm" },
      h("span", null, f.detail || "The request was refused."),
      outstanding
        ? h("span", { class: "row-wrap" }, h("span", { class: "faint" }, "Outstanding"), outstanding)
        : null,
      f.approvalId
        ? h(
            "span",
            { class: "row-wrap" },
            h("span", { class: "faint" }, "Approval"),
            mono(f.approvalId),
            copyButton(ctx, f.approvalId, "Copy"),
          )
        : null,
    );

    return banner("danger", f.title || humanStatus(f.code), body, h("div", { class: "row-wrap" }, ...extra));
  };

  const eligibilityBanner = (): Node | null => {
    if (eligibilityError) {
      return banner(
        "danger",
        "The eligibility check failed",
        h("span", null, eligibilityError),
        btn("Dismiss", {
          size: "sm",
          variant: "ghost",
          onclick: () => {
            eligibilityError = null;
            paint();
          },
        }),
      );
    }
    const result = eligibility;
    if (!result) return null;

    const body = h(
      "div",
      { class: "stack-sm" },
      h("span", null, result.reason),
      h(
        "div",
        { class: "row-wrap" },
        h("span", { class: "faint" }, `generation ${result.generation}`),
        result.outstanding
          ? h("span", { class: "row-wrap" }, h("span", { class: "faint" }, "outstanding"), mono(result.outstanding))
          : null,
        result.invoice_id
          ? h(
              "span",
              { class: "row-wrap" },
              h("a", { class: "linkish", href: `/invoices/${result.invoice_id}` }, "Open the invoice"),
              copyButton(ctx, result.invoice_id, "Copy id"),
            )
          : null,
      ),
    );

    return banner(
      result.eligible ? "success" : "warning",
      result.eligible ? "Eligible for suspension" : "Not eligible for suspension",
      body,
      btn("Dismiss", {
        size: "sm",
        variant: "ghost",
        onclick: () => {
          eligibility = null;
          paint();
        },
      }),
    );
  };

  const transitionBanner = (t: Transition): Node => {
    const operation = t.operation;
    const status = operation?.status ?? "queued";
    const tone = transitionTone(t);
    const heading =
      t.state === "polling"
        ? `${titleCase(t.kind)} queued`
        : t.state === "timeout"
          ? `${titleCase(t.kind)} has not settled`
          : `${titleCase(t.kind)} ${humanStatus(status).toLowerCase()}`;

    const body = h(
      "div",
      { class: "stack-sm" },
      h(
        "div",
        { class: "row-wrap" },
        t.state === "polling" ? spinner(12) : null,
        statusBadge(status, { dot: true }),
        mono(shortId(t.operationId)),
        h("a", { class: "linkish", href: `/operations/${t.operationId}` }, "Open the operation"),
      ),
      operation && operation.attempts > 0
        ? h("span", { class: "faint" }, `attempt ${operation.attempts}`)
        : null,
      operation?.last_error ? h("span", { class: "mono" }, truncate(operation.last_error, 200)) : null,
      t.state === "timeout"
        ? h(
            "span",
            null,
            "The operation did not settle within 30 seconds. It is still queued or running; the queue page shows its lease and attempts.",
          )
        : null,
    );

    return banner(
      tone,
      heading,
      body,
      btn("Dismiss", {
        size: "sm",
        variant: "ghost",
        onclick: () => {
          transition = null;
          paint();
        },
      }),
    );
  };

  const tabsNav = (data: ServiceDetail): Node =>
    tabs(
      [
        { key: "overview", label: "Overview" },
        { key: "operations", label: "Operations", count: data.operations.length },
        { key: "holds", label: "Holds", count: data.holds.length },
        { key: "invoices", label: "Invoices", count: data.invoices.length },
      ],
      tab,
      (key) => {
        tab = tabFrom(key);
        ctx.replaceQuery({ tab: tab === "overview" ? null : tab });
        paint();
      },
    );

  const tabBody = (data: ServiceDetail): Node => {
    switch (tab) {
      case "operations":
        return operationsTab(data);
      case "holds":
        return holdsTab(data);
      case "invoices":
        return invoicesTab(data);
      default:
        return overviewTab(data);
    }
  };

  const overviewTab = (data: ServiceDetail): Node => {
    const svc = data.service;

    const facts = card({
      title: "Facts",
      body: metaGrid([
        {
          label: "Service id",
          value: h("span", { class: "row" }, mono(svc.id), copyButton(ctx, svc.id, "id")),
        },
        {
          label: "External id",
          value: svc.external_id
            ? h("span", { class: "row" }, mono(svc.external_id), copyButton(ctx, svc.external_id, "id"))
            : h("span", { class: "faint" }, "not provisioned yet"),
        },
        {
          label: "Customer",
          value: h("a", { class: "linkish", href: `/customers/${svc.customer.id}` }, svc.customer.email),
        },
        {
          label: "Product",
          value: h("a", { class: "linkish", href: `/catalog-management/${svc.product.id}` }, svc.product.name),
        },
        { label: "Status", value: serviceStatusBadge(svc.status) },
        { label: "Generation", value: mono(svc.generation) },
        {
          label: "Next bill",
          value: svc.next_bill_at
            ? h("span", { title: dateTime(svc.next_bill_at) }, `${relative(svc.next_bill_at)} · ${dateTime(svc.next_bill_at)}`)
            : h("span", { class: "faint" }, "not scheduled"),
        },
        { label: "Created", value: h("span", { title: dateTime(svc.created_at) }, relative(svc.created_at)) },
        {
          label: "Open operations",
          value:
            svc.open_operations > 0
              ? badge(String(svc.open_operations), "danger", { dot: true })
              : h("span", { class: "faint" }, "none"),
        },
      ]),
    });

    const lastError = svc.last_error
      ? banner("danger", "The last operation reported an error", h("span", { class: "mono" }, svc.last_error))
      : null;

    const metadataPanel = metadata ? metadataCard(metadata) : null;

    const subscription = data.subscription
      ? card({
          title: "Subscription",
          sub: data.subscription.cancel_at_period_end ? "cancels at period end" : undefined,
          body: h(
            "div",
            { class: "stack-sm" },
            kvRow("Subscription", h("span", { class: "row" }, mono(data.subscription.id), copyButton(ctx, data.subscription.id, "id"))),
            kvRow("Status", statusBadge(data.subscription.status, { dot: true })),
            kvRow("Interval", h("span", null, titleCase(data.subscription.interval))),
            kvRow("Product", h("span", null, data.subscription.product)),
            kvRow(
              "Next billing",
              h("span", { title: dateTime(data.subscription.next_billing_at) }, relative(data.subscription.next_billing_at)),
            ),
            kvRow(
              "Current period",
              h(
                "span",
                null,
                `${dateTime(data.subscription.current_period_start)} → ${dateTime(data.subscription.current_period_end)}`,
              ),
            ),
            kvRow(
              "Renewal",
              data.subscription.cancel_at_period_end
                ? badge("Cancels at period end", "warning", { dot: true })
                : badge("Renews", "success", { dot: true }),
            ),
          ),
        })
      : card({
          title: "Subscription",
          body: h(
            "p",
            { class: "faint" },
            "This service has no subscription. It was provisioned outside the subscription engine, so renewals are not scheduled from here.",
          ),
        });

    const related = card({
      title: "Related",
      flush: true,
      body: h(
        "div",
        null,
        linkRow(`/customers/${svc.customer.id}`, "user", "Customer", svc.customer.email),
        linkRow(`/catalog-management/${svc.product.id}`, "box", "Product", svc.product.name),
        linkRow(`/operations?service_id=${svc.id}`, "activity", "Operations for this service", "Every attempt, with its lease and error"),
        linkRow(`/invoices?customer_id=${svc.customer.id}`, "invoice", "Invoices for this customer", "The whole account, not just this service"),
      ),
    });

    return h(
      "div",
      { class: "split" },
      h("div", { class: "stack" }, facts, lastError, subscription, serviceActionsCard(), extensionActionsCard()),
      h("div", { class: "stack" }, metadataPanel, provisioningCard(), related),
    );
  };

  /**
   * "What this is": the answer a support agent needs before anything else on this page.
   *
   * It renders the effective values rather than the written ones, and says which is which: an
   * operator who sees a description has to know whether the customer wrote it or the server
   * derived it, because "fix the description" means two different things in those two cases.
   */
  const metadataCard = (m: ServiceMetadata): Node => {
    const items: Array<{ label: Child; value: Child }> = [
      { label: "Name", value: h("span", null, m.effective_label) },
      {
        label: "Named by",
        value: m.label
          ? h("span", { class: "row-wrap" }, badge("The customer", "info", { dot: true }))
          : h("span", { class: "faint" }, "the product (no label written)"),
      },
      {
        label: "Plan",
        value: h(
          "span",
          { class: "row-wrap" },
          m.plan.name || h("span", { class: "faint" }, "no subscription"),
          m.plan.interval ? h("span", { class: "faint" }, `· ${titleCase(m.plan.interval)}`) : null,
        ),
      },
      {
        label: "Price",
        value: m.plan.amount
          ? moneyEl(m.plan.amount)
          : h("span", { class: "faint" }, "not priced from a subscription"),
      },
      { label: "Quantity", value: mono(m.quantity) },
      {
        label: "Order",
        value: m.order_id
          ? h("a", { class: "linkish", href: `/orders/${m.order_id}` }, shortId(m.order_id, 12))
          : h("span", { class: "faint" }, "no order (provisioned from an invoice)"),
      },
    ];

    const description = h(
      "div",
      { class: "stack-sm" },
      h("p", { class: "svc-meta-description" }, m.effective_description),
      m.description
        ? null
        : h(
            "p",
            { class: "field-hint" },
            "Derived from the product and the selected options; nobody has written a description.",
          ),
    );

    const properties = m.properties.length
      ? h(
          "div",
          { class: "stack-sm" },
          h("p", { class: "field-hint" }, "Properties"),
          ...m.properties.map((property) =>
            h(
              "div",
              { class: "kv" },
              h("span", { class: "kv-key" }, property.label),
              h(
                "span",
                { class: "kv-value row-wrap" },
                mono(property.value),
                property.show_on_invoice
                  ? badge("On invoices", "info", { dot: true })
                  : null,
                property.customer_visible ? null : badge("Internal", "warning", { dot: true }),
              ),
            ),
          ),
        )
      : null;

    return card({
      title: "What this is",
      sub: "Label, description and properties",
      actions: ctx.can("service:write:all")
        ? btn("Edit metadata", { size: "sm", icon: "edit", onclick: () => void editMetadata() })
        : null,
      body: h("div", { class: "stack-sm" }, description, metaGrid(items), properties),
    });
  };

  /**
   * The operator actions the panel grew for parity (SVC-12, SVC-13, SVC-14).
   *
   * Each button states the queue-or-immediate distinction where it matters: a recalculation is a
   * synchronous pricing change, while everything that touches a provider is queued and answers
   * 202. A screen that implied immediacy for a queued action would make an operator click twice.
   */
  const serviceActionsCard = (): Node | null => {
    const allowed = ctx.can("service:write:all");
    const svc = detail?.service;
    if (!allowed || !svc) return null;
    const terminated = svc.status === "terminated";
    const buttons: Node[] = [
      btn("Recalculate price", {
        size: "sm",
        icon: "refresh",
        title: "Recompute this service's price from the catalogue",
        onclick: () => void recalculatePrice(),
      }),
    ];
    if (!terminated) {
      buttons.push(
        btn("Cancel provider subscription", {
          size: "sm",
          icon: "x",
          title: "Stop the payment gateway billing this customer",
          onclick: () => void cancelExternalSubscription(),
        }),
      );
    }
    if (svc.external_id !== "") {
      buttons.push(
        btn("Delete remote server", {
          size: "sm",
          icon: "trash",
          title: "Destroy the provider's resource and keep the service",
          onclick: () => void deleteRemote(svc),
        }),
      );
    }
    buttons.push(
      btn("Message customer", {
        size: "sm",
        icon: "mail",
        title: "Send the customer a message about this service",
        onclick: () => void notifyCustomer(svc),
      }),
    );
    return card({
      title: "Price and provider",
      sub: "The actions that change what the customer pays or where their server lives",
      body: h(
        "div",
        { class: "stack-sm" },
        h("div", { class: "row-wrap" }, ...buttons),
        h(
          "p",
          { class: "field-hint" },
          "Recalculating applies the catalogue's current price to the subscription. The provider actions are queued as operations, so the outcome appears in the transition panel rather than in a toast.",
        ),
      ),
    });
  };

  /** The actions the installed extensions contribute (SVC-11). */
  const extensionActionsCard = (): Node | null => {
    if (extensionActions.length === 0) return null;
    return card({
      title: "Extension actions",
      sub: "Contributed by the installed extensions; each one runs in the extension that declared it",
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "div",
          { class: "row-wrap" },
          ...extensionActions.map((action) =>
            btn(action.label, {
              size: "sm",
              icon: "zap",
              title: `Declared by ${action.extension} · requires ${action.permission}`,
              onclick: () => void runExtensionAction(action),
            }),
          ),
        ),
      ),
    });
  };

  /**
   * The provisioner's own per-purchase inputs (UPG-12).
   *
   * The fields are the adapter's declaration, not the core's list, which is why this card says
   * where they came from: an operator who sees "Domain" has to know that it is cPanel asking and
   * not a property somebody invented.
   */
  const provisioningCard = (): Node | null => {
    const data = provisioning;
    if (!data || !data.configured) return null;
    const fields = data.fields ?? [];
    if (fields.length === 0) {
      return card({
        title: "Provisioning inputs",
        sub: `${data.provider} · ${data.extension}`,
        body: h(
          "p",
          { class: "field-hint" },
          data.supported
            ? "This provisioner declares no per-service inputs, so there is nothing to fill in."
            : "This provisioner does not declare its inputs. Anything it needs arrives through the product's config options.",
        ),
      });
    }
    const rows = fields.map((f) => {
      const value = data.values[f.key];
      return kvRow(
        f.label,
        value ? mono(value) : h("span", { class: "faint" }, f.required ? "required, not set" : "not set"),
      );
    });
    return card({
      title: "Provisioning inputs",
      sub: `${data.provider} asked for these`,
      actions: ctx.can("service:write:all")
        ? btn("Edit inputs", { size: "sm", icon: "edit", onclick: () => void editProvisioningFields() })
        : null,
      body: h(
        "div",
        { class: "stack-sm" },
        ...rows,
        h(
          "p",
          { class: "field-hint" },
          "These are sent to the provisioner verbatim when the service is created or resized. A missing required input is refused before the purchase, not after it.",
        ),
      ),
    });
  };

  const operationsTab = (data: ServiceDetail): Node => {
    // The API already caps the detail at the 20 most recent operations; slicing again keeps
    // the promise explicit if that changes.
    const operations = data.operations.slice(0, 20);
    if (operations.length === 0) {
      return card({
        body: emptyState({
          title: "No operations yet",
          body: "Suspend, resume and terminate requests, and every renewal, appear here once they are queued.",
          icon: "activity",
        }),
      });
    }

    const head = h(
      "thead",
      null,
      h(
        "tr",
        null,
        h("th", { scope: "col" }, "Operation"),
        h("th", { scope: "col" }, "Status"),
        h("th", { scope: "col", class: "num" }, "Generation"),
        h("th", { scope: "col", class: "num" }, "Attempts"),
        h("th", { scope: "col" }, "Created"),
        h("th", { scope: "col", class: "hide-sm" }, "Last error"),
      ),
    );

    const body = h(
      "tbody",
      null,
      ...operations.map((operation) => {
        const row = h(
          "tr",
          { class: "is-clickable" },
          h(
            "td",
            { class: "cell-primary" },
            h(
              "div",
              { class: "cell-main" },
              h(
                "div",
                { class: "primary" },
                h("a", { class: "row-title", href: `/operations/${operation.id}` }, titleCase(operation.kind)),
              ),
              h("div", { class: "secondary" }, mono(shortId(operation.id, 12))),
            ),
          ),
          h("td", { "data-label": "Status" }, statusBadge(operation.status, { dot: true })),
          h("td", { class: "num", "data-label": "Generation" }, mono(operation.generation)),
          h("td", { class: "num", "data-label": "Attempts" }, mono(operation.attempts)),
          h(
            "td",
            { "data-label": "Created", title: dateTime(operation.created_at) },
            h("span", { class: "dim" }, relative(operation.created_at)),
          ),
          h(
            "td",
            { class: "hide-sm", "data-label": "Last error" },
            operation.last_error
              ? h(
                  "span",
                  { class: "mono svc-truncate", title: operation.last_error },
                  truncate(operation.last_error, 40),
                )
              : h("span", { class: "faint" }, "—"),
          ),
        );
        row.addEventListener("click", (event) => {
          if ((event.target as Element).closest("a,button")) return;
          ctx.navigate(`/operations/${operation.id}`);
        });
        return row;
      }),
    );

    return card({
      title: "Operations",
      sub: `${operations.length} most recent`,
      actions: h(
        "a",
        { class: "linkish", href: `/operations?service_id=${data.service.id}` },
        "Open the queue",
        icon("chevronRight", 12),
      ),
      flush: true,
      body: h("div", { class: "table-wrap table-wrap--cards" }, h("table", { class: "table" }, head, body)),
    });
  };

  const holdsTab = (data: ServiceDetail): Node => {
    if (data.holds.length === 0) {
      return card({
        body: emptyState({
          title: "No holds",
          body: "A hold stops automatic suspension and automatic resume. Add one when the account must not be touched by automation.",
          icon: "lock",
          action:
            ctx.can("service:hold") && data.service.status !== "terminated"
              ? btn("Add hold", { size: "sm", icon: "lock", onclick: () => void addHold() })
              : null,
        }),
      });
    }

    // The endpoint returns every hold, expired ones included: an expired hold is history, not
    // a block, so the summary says how many are actually in force.
    const active = data.holds.filter((hold) => holdIsActive(hold)).length;
    const expired = data.holds.length - active;

    return card({
      title: "Holds",
      sub: expired > 0 ? `${active} in place · ${expired} expired` : `${active} in place`,
      flush: true,
      body: h("div", { class: "svc-holds" }, ...data.holds.map(holdRow)),
    });
  };

  const holdRow = (hold: Hold): Node => {
    const active = holdIsActive(hold);
    return h(
      "div",
      { class: "svc-hold" },
      h(
        "div",
        { class: "svc-hold-main" },
        h("span", { class: "svc-hold-reason" }, hold.reason),
        h(
          "span",
          { class: "svc-hold-meta" },
          h("span", { title: dateTime(hold.created_at) }, `placed ${relative(hold.created_at)}`),
          h("span", { class: "faint" }, "·"),
          h("span", { class: "row" }, "by", mono(shortId(hold.created_by, 10), hold.created_by)),
        ),
      ),
      h(
        "div",
        { class: "svc-hold-side" },
        hold.expires_at === null
          ? h(
              "span",
              { title: "A hold with no expiry blocks automation until a human clears it." },
              badge("Permanent", "warning", { dot: true }),
            )
          : active
            ? h(
                "span",
                { title: dateTime(hold.expires_at) },
                badge(`expires ${relative(hold.expires_at)}`, "info", { dot: true }),
              )
            : h(
                "span",
                { title: dateTime(hold.expires_at) },
                badge(`expired ${relative(hold.expires_at)}`, "neutral", { dot: true }),
              ),
        ctx.can("service:hold")
          ? btn("Clear", { size: "sm", variant: "ghost", icon: "x", onclick: () => void clearHold(hold) })
          : null,
      ),
    );
  };

  const invoicesTab = (data: ServiceDetail): Node => {
    if (data.invoices.length === 0) {
      return card({
        body: emptyState({
          title: "No invoices",
          body: "This service has not been billed yet. The first renewal, or a manual invoice, will appear here.",
          icon: "invoice",
        }),
      });
    }

    const head = h(
      "thead",
      null,
      h(
        "tr",
        null,
        h("th", { scope: "col" }, "Invoice"),
        h("th", { scope: "col" }, "Status"),
        h("th", { scope: "col", class: "hide-sm" }, "Issued"),
        h("th", { scope: "col", class: "hide-sm" }, "Due"),
        h("th", { scope: "col", class: "num" }, "Total"),
        h("th", { scope: "col", class: "num" }, "Outstanding"),
      ),
    );

    const body = h(
      "tbody",
      null,
      ...data.invoices.map((invoice) => {
        const row = h(
          "tr",
          { class: "is-clickable" },
          h(
            "td",
            { class: "cell-primary" },
            h(
              "div",
              { class: "cell-main" },
              h(
                "div",
                { class: "primary" },
                h("a", { class: "row-title", href: `/invoices/${invoice.id}` }, invoice.number || "Draft"),
              ),
              h("div", { class: "secondary" }, mono(shortId(invoice.id, 12))),
            ),
          ),
          h("td", { "data-label": "Status" }, statusBadge(invoice.overdue ? "overdue" : invoice.status, { dot: true })),
          h(
            "td",
            { class: "hide-sm", "data-label": "Issued" },
            h("span", { class: "dim" }, invoice.issued_at ? relative(invoice.issued_at) : "—"),
          ),
          h(
            "td",
            { class: "hide-sm", "data-label": "Due" },
            h("span", { class: "dim" }, invoice.due_at ? relative(invoice.due_at) : "—"),
          ),
          h("td", { class: "num", "data-label": "Total" }, moneyEl(invoice.total)),
          h(
            "td",
            { class: "num", "data-label": "Outstanding" },
            invoice.due.minor > 0 ? moneyEl(invoice.due) : h("span", { class: "faint" }, "Settled"),
          ),
        );
        row.addEventListener("click", (event) => {
          if ((event.target as Element).closest("a,button")) return;
          ctx.navigate(`/invoices/${invoice.id}`);
        });
        return row;
      }),
    );

    return card({
      title: "Invoices",
      sub: `${data.invoices.length} on this service`,
      actions: h(
        "a",
        { class: "linkish", href: `/invoices?customer_id=${data.service.customer.id}` },
        "All invoices for this customer",
        icon("chevronRight", 12),
      ),
      flush: true,
      body: h("div", { class: "table-wrap table-wrap--cards" }, h("table", { class: "table" }, head, body)),
    });
  };

  const dangerZone = (svc: Service): Node => {
    const terminated = svc.status === "terminated";
    const allowed = ctx.can("service:terminate");
    return card({
      class: "svc-danger",
      title: "Danger zone",
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "p",
          null,
          "Termination destroys the provider's resource and stops billing. It is queued as an operation, it cannot be reconciled automatically, and it cannot be undone.",
        ),
        h(
          "div",
          { class: "row-wrap" },
          allowed && !terminated
            ? btn("Terminate service", {
                variant: "danger",
                icon: "trash",
                onclick: () => {
                  const current = detail?.service;
                  if (current) void terminate(current);
                },
              })
            : null,
          // Deleting is offered beside termination because the difference between them is the
          // point: one ends the service and keeps its history, the other removes a row that has
          // none.
          allowed && !terminated
            ? btn("Delete service", {
                variant: "ghost",
                icon: "trash",
                onclick: () => {
                  const current = detail?.service;
                  if (current) void deleteService(current);
                },
              })
            : null,
          terminated ? badge("Terminated", "danger", { dot: true }) : null,
          !allowed ? h("span", { class: "faint" }, "Your account does not hold service:terminate.") : null,
        ),
      ),
    });
  };

  void load();
  return host;
}

// ----------------------------------------------------------------- helpers --

function nameOf(svc: Service): string {
  return svc.external_id || svc.id;
}

/** A hold with no expiry never lapses; one with an expiry is only in force until it passes. */
function holdIsActive(hold: Hold): boolean {
  if (hold.expires_at === null) return true;
  const when = new Date(hold.expires_at).getTime();
  return Number.isNaN(when) ? true : when > Date.now();
}

/** "error" is not in the shared tone map, and it is the status an operator must not miss. */
function serviceStatusBadge(status: string): HTMLElement {
  if (status === "error") return badge("Error", "danger", { dot: true });
  return statusBadge(status, { dot: true });
}

function tabFrom(raw: string | null): TabKey {
  return raw === "operations" || raw === "holds" || raw === "invoices" ? raw : "overview";
}

function kvRow(label: string, value: Node): HTMLElement {
  return h("div", { class: "kv" }, h("span", { class: "kv-key" }, label), h("span", { class: "kv-value" }, value));
}

function linkRow(href: string, iconName: IconName, title: string, sub: string): HTMLElement {
  return h(
    "a",
    { class: "card-link", href },
    icon(iconName, 13),
    h("div", { class: "cell-main" }, h("div", { class: "primary" }, title), h("div", { class: "secondary" }, sub)),
    icon("chevronRight", 13),
  );
}

function selectExpiry(): HTMLSelectElement {
  // Zero means "until a human clears it": a hold that expires on its own is a hold nobody
  // notices has lapsed, so the permanent option is the default rather than an afterthought.
  return select(
    [
      { value: "0", label: "Until a human clears it (permanent)" },
      { value: "3600", label: "1 hour" },
      { value: "86400", label: "24 hours" },
      { value: "604800", label: "7 days" },
    ],
    { id: "hold-expiry", value: "0" },
  );
}

function transitionTone(t: Transition): "info" | "success" | "danger" | "warning" {
  if (t.state === "timeout") return "warning";
  const status = t.operation?.status;
  if (status === "succeeded") return "success";
  if (status === "failed" || status === "needs_review" || status === "unknown") return "danger";
  return "info";
}

function approvalIdOf(err: ApiError): string | undefined {
  const problem = err.problem as Record<string, unknown>;
  const approval = problem.approval;
  if (approval && typeof approval === "object" && "id" in approval) {
    const id = (approval as { id?: unknown }).id;
    if (typeof id === "string" && id !== "") return id;
  }
  const flat = problem.approval_id;
  if (typeof flat === "string" && flat !== "") return flat;
  return undefined;
}

function failureFrom(kind: string, err: unknown): Failure {
  if (err instanceof ApiError) {
    const problem = err.problem as Record<string, unknown>;
    const title = typeof problem.title === "string" && problem.title !== "" ? problem.title : humanStatus(err.code);
    return {
      kind,
      code: err.code,
      status: err.status,
      title,
      detail: err.detail,
      approvalId: approvalIdOf(err),
      outstanding: problem.outstanding,
    };
  }
  return {
    kind,
    code: "unknown",
    status: 0,
    title: "Something went wrong",
    detail: err instanceof Error ? err.message : String(err),
  };
}

/** `outstanding` travels as a formatted string on an eligibility refusal and, in the older
 *  still_owing shape, as a list of Amounts. Both are rendered as money, never as a float. */
function outstandingNode(raw: unknown): Node | null {
  if (raw === null || raw === undefined || raw === "") return null;
  if (typeof raw === "string") return mono(raw);
  if (Array.isArray(raw)) {
    return h("span", { class: "row-wrap" }, ...raw.map((item) => amountNode(item)));
  }
  return mono(String(raw));
}

function amountNode(item: unknown): Node {
  if (item && typeof item === "object" && "minor" in item && "currency" in item) {
    return moneyEl(item as Amount);
  }
  return h("span", { class: "mono" }, String(item));
}

function isAbort(err: unknown): boolean {
  return err instanceof DOMException && err.name === "AbortError";
}
