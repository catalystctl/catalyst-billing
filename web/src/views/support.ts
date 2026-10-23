// Opening a ticket on a customer's behalf.
//
// A customer who telephones cannot be asked to submit a form. Before this screen existed the
// support queue could reply, assign and close but not *open*, so the agent's honest answer was
// "please email us" — which is how a phone call becomes a lost ticket.
//
// Three decisions shape the screen:
//
//   - **The customer is chosen by search, not typed as an id.** An agent on a call has a name or
//     an email address, not a uuid. The picker searches `GET /api/v1/staff/customers?q=` (the same
//     endpoint the customers screen uses) and shows the account it will open the ticket for. The
//     id is what travels; the picker is how it is found.
//   - **The form is the customer's form.** Departments, priorities, the attachment cap and the
//     custom fields all come from `GET /api/v1/staff/tickets/meta`, which is the same
//     configuration the portal form reads. A field the operator made required is required here
//     too — a rule the team can skip when it is convenient is not a rule — and the handler
//     enforces it either way.
//   - **The ticket records who opened it.** The handler writes the staff account into `opened_by`
//     and sets `opened_by_staff`, so six months later "who opened this, and for whom" is a fact
//     in the row rather than an inference from the first message.
//
// Attachments are uploaded after the ticket exists, exactly as on the portal: a file belongs to a
// message, and a message belongs to a ticket. A failed upload does not lose the ticket.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  banner,
  btn,
  card,
  emptyState,
  field,
  input,
  pageHead,
  resource,
  searchInput,
  select,
  skeletonPage,
  textarea,
  withBusy,
} from "../ui/kit";
import { answeredFields } from "./ticket-new";
import {
  bytesLabel,
  customFieldControls,
  normaliseThread,
  ticketHref,
  ticketsMetaPath,
  uploadTicketAttachment,
  type TicketMeta,
  type TicketThread,
} from "./tickets";

export const meta = { title: "Open a ticket for a customer", crumb: "New ticket" };

/** The fallback cap, matching `tickets.MaxAttachmentBytes`. Used only before the meta answers. */
const DEFAULT_MAX_ATTACHMENT_BYTES = 10 * 1024 * 1024;

/** The search needs two characters: the handler refuses one rather than scanning every customer. */
const MIN_SEARCH = 2;

interface CustomerMatch {
  id: string;
  email: string;
  name?: string;
  services?: number;
}

interface CustomerService {
  id: string;
  product?: { name?: string };
  status?: string;
}

export default async function supportNewTicketPage(ctx: Ctx): Promise<Node> {
  // The queue hides this link from a customer and the router bounces them; this is the belt to
  // that braces, because a form that always fails is worse than a sentence.
  if (!ctx.can("ticket:write:all")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Open a ticket for a customer" }),
      card({
        flush: true,
        body: emptyState({
          title: "This screen is for the support team",
          body: "Opening a ticket on someone's behalf needs ticket:write:all. Your own tickets are in your account, under Tickets.",
          icon: "lock",
          action: btn("Your tickets", { href: "/tickets", size: "sm", icon: "message" }),
        }),
      }),
    );
  }

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Open a ticket for a customer",
      sub: "For a phone call or a conversation that did not arrive as an email. The ticket records that you opened it, and for whom.",
      actions: btn("Support queue", { href: "/support", size: "sm", icon: "arrowLeft" }),
    }),
    resource(
      ctx,
      async (signal) => {
        const meta = await ctx.api.get<TicketMeta>(ticketsMetaPath(ctx), { cache: false, signal });
        return meta;
      },
      (meta) => renderForm(ctx, meta),
      { skeleton: () => skeletonPage() },
    ),
  );
}

function renderForm(ctx: Ctx, meta: TicketMeta): Node {
  if (meta.enabled === false) {
    return card({
      flush: true,
      body: emptyState({
        title: "Support tickets are switched off",
        body: "This installation is not taking tickets, including ones the team opens. Turn the desk back on in the ticket settings first.",
        icon: "lock",
        action: btn("Back to the dashboard", { href: "/dashboard", size: "sm", icon: "home" }),
      }),
    });
  }

  const departments = Array.isArray(meta.departments) ? meta.departments.filter(Boolean) : [];
  const priorities =
    Array.isArray(meta.priorities) && meta.priorities.length > 0 ? meta.priorities : ["low", "normal", "high"];
  const maxBytes =
    typeof meta.max_attachment_bytes === "number" && meta.max_attachment_bytes > 0
      ? meta.max_attachment_bytes
      : DEFAULT_MAX_ATTACHMENT_BYTES;
  const maxSubject =
    typeof meta.max_subject_length === "number" && meta.max_subject_length > 0 ? meta.max_subject_length : 200;

  // ------------------------------------------------------------ the customer --

  let chosen: CustomerMatch | null = null;
  let services: CustomerService[] = [];
  /** The chosen service id, read at submit time. Kept out of the DOM so the form does not have to
   *  find its own control by id. */
  let serviceID = "";

  const chosenHost = h("div");
  const resultsHost = h("div", { class: "stack-sm" });
  const serviceHost = h("div");

  const renderChosen = () => {
    if (!chosen) {
      swap(
        chosenHost,
        banner("info", "No customer chosen yet", "Search by name or email address. The ticket is opened for that account."),
      );
      return;
    }
    swap(
      chosenHost,
      banner(
        "success",
        chosen.name || chosen.email,
        chosen.email,
        btn("Change", {
          size: "sm",
          variant: "ghost",
          onclick: () => {
            chosen = null;
            services = [];
            serviceID = "";
            renderChosen();
            renderServices();
            void search.query("");
          },
        }),
      ),
    );
  };

  const renderServices = () => {
    if (!chosen) {
      serviceHost.replaceChildren();
      return;
    }
    if (services.length === 0) {
      swap(
        serviceHost,
        field({
          label: "Service",
          control: h("p", { class: "field-hint" }, "This account has no services, so there is nothing to pick."),
          id: "ticket-staff-service",
        }),
      );
      return;
    }
    const picker = select(
      [
        { value: "", label: "Not about a specific service" },
        ...services.map((svc) => ({
          value: svc.id,
          label: svc.product?.name ? `${svc.product.name} (${svc.status ?? "unknown"})` : svc.id,
        })),
      ],
      { id: "ticket-staff-service", onchange: (event) => (serviceID = (event.target as HTMLSelectElement).value) },
    );
    serviceID = picker.value;
    swap(
      serviceHost,
      field({
        label: "Service",
        control: picker,
        id: "ticket-staff-service",
        hint: "Optional. Naming the service puts its record in front of whoever answers.",
      }),
    );
  };

  const choose = async (match: CustomerMatch) => {
    chosen = match;
    renderChosen();
    resultsHost.replaceChildren();
    // The services come from the customer's own record: the picker must not offer a service that
    // belongs to somebody else, and the handler refuses one anyway.
    try {
      const detail = await ctx.api.get<{ services?: unknown }>(
        `/api/v1/staff/customers/${encodeURIComponent(match.id)}`,
        { cache: false },
      );
      services = Array.isArray(detail.services) ? (detail.services as CustomerService[]) : [];
    } catch {
      services = [];
    }
    renderServices();
  };

  const search = buildCustomerSearch(ctx, resultsHost, choose);

  // A deep link from the customer screen carries the id, so an agent arriving from a customer's
  // page does not have to search for the person they are already looking at.
  const preselect = ctx.route.query.get("customer_id");
  if (preselect) {
    void (async () => {
      try {
        const detail = await ctx.api.get<{ customer?: CustomerMatch }>(
          `/api/v1/staff/customers/${encodeURIComponent(preselect)}`,
          { cache: false },
        );
        if (detail.customer?.id) await choose(detail.customer);
      } catch {
        // A stale link is not an error worth a banner: the search is right there.
      }
    })();
  }

  // ------------------------------------------------------------- the ticket --

  const subject = input({
    id: "staff-ticket-subject",
    placeholder: "A one-line summary, e.g. “Called about a 502 on example.com”",
    maxlength: maxSubject,
    autocomplete: "off",
  });
  const department = select(
    departments.map((value) => ({ value, label: titleFromKey(value) })),
    { id: "staff-ticket-department", value: departments[0] ?? "" },
  );
  const priority = select(
    priorities.map((value) => ({ value, label: titleFromKey(value) })),
    { id: "staff-ticket-priority", value: priorities.includes("normal") ? "normal" : (priorities[0] ?? "") },
  );
  const message = textarea({
    id: "staff-ticket-message",
    placeholder: "What the customer reported, in their words where possible.",
    maxlength: 32_000,
  });
  message.rows = 7;

  const custom = customFieldControls(meta.custom_fields);

  // The picker is offered whether or not the client can upload: a file chosen and silently dropped
  // is worse than no picker, so when the client cannot send one the note says so and the form
  // still creates the ticket.
  const files = rawFilePicker();

  const errorHost = h("div");
  const submit = btn("Open the ticket", { variant: "primary", type: "submit", icon: "send" }) as HTMLButtonElement;

  const form = h(
    "form",
    { class: "stack", novalidate: true },
    errorHost,
    chosenHost,
    resultsHost,
    field({
      label: "Find the customer",
      control: search.el,
      id: "staff-ticket-customer",
      hint: "Two characters is enough: the search covers names and email addresses.",
    }),
    h(
      "div",
      { class: "grid-2" },
      field({ label: "Subject", control: subject, id: "staff-ticket-subject", required: true }),
      field({ label: "Department", control: department, id: "staff-ticket-department" }),
      field({ label: "Priority", control: priority, id: "staff-ticket-priority" }),
      serviceHost,
    ),
    field({ label: "Message", control: message, id: "staff-ticket-message", required: true }),
    custom ? h("div", { class: "stack-sm" }, h("div", { class: "field-hint" }, "The fields this desk asks for."), custom.el) : null,
    field({
      label: "Attachments",
      control: files.el,
      id: "staff-ticket-files",
      hint: `Optional, up to ${bytesLabel(maxBytes)} per file. They are uploaded after the ticket is created.`,
    }),
    h("div", { class: "row-wrap" }, submit, btn("Cancel", { href: "/support", variant: "ghost" })),
  );

  form.addEventListener("submit", (event) => {
    event.preventDefault();
    void withBusy(ctx, submit, async () => {
      errorHost.replaceChildren();

      if (!chosen) {
        errorHost.replaceChildren(
          banner("danger", "Choose the customer first", "A ticket the team opens is always opened for somebody."),
        );
        search.focus();
        return;
      }
      const subjectValue = subject.value.trim();
      if (!subjectValue) {
        errorHost.replaceChildren(banner("danger", "A subject is required", "One line is enough: it is the queue's first impression."));
        subject.focus();
        return;
      }
      const bodyValue = message.value.trim();
      if (!bodyValue) {
        errorHost.replaceChildren(banner("danger", "The first message cannot be empty", "Write down what the customer reported."));
        message.focus();
        return;
      }
      const missing = custom?.missingRequired() ?? [];
      if (missing.length > 0) {
        errorHost.replaceChildren(
          banner(
            "danger",
            missing.length === 1 ? `${missing[0]} is required` : `${missing.length} required fields are blank`,
            `This desk asks for them on every ticket: ${missing.join(", ")}.`,
          ),
        );
        custom?.focusFirst(missing);
        return;
      }
      const tooBig = files.files().filter((file) => file.size > maxBytes);
      if (tooBig.length > 0) {
        errorHost.replaceChildren(
          banner(
            "danger",
            `That file is over the ${bytesLabel(maxBytes)} limit`,
            `${tooBig.map((file) => `${file.name} (${bytesLabel(file.size)})`).join(", ")} cannot be attached.`,
          ),
        );
        return;
      }

      let thread: TicketThread;
      try {
        const created = await ctx.api.post<unknown>("/api/v1/staff/tickets", {
          customer_id: chosen.id,
          subject: subjectValue,
          department: department.value,
          priority: priority.value,
          service_id: serviceID || undefined,
          message: { body: bodyValue },
          fields: answeredFields(custom),
        });
        thread = normaliseThread(created);
      } catch (err) {
        errorHost.replaceChildren(createFailure(err));
        return;
      }

      const chosenFiles = files.files();
      const failed: string[] = [];
      for (const file of chosenFiles) {
        try {
          await uploadTicketAttachment(ctx, thread.ticket.id, file, thread.messages[0]?.id);
        } catch (err) {
          failed.push(`${file.name}${err instanceof ApiError ? ` (${err.detail || err.code})` : ""}`);
        }
      }

      if (failed.length > 0) {
        ctx.toast(`Ticket ${thread.ticket.number || "created"}, but ${failed.length} file(s) did not upload`, {
          tone: "danger",
          duration: 0,
          detail: `${failed.join("; ")}. You can attach them from the ticket.`,
        });
      } else {
        ctx.toast(`Ticket ${thread.ticket.number || "opened"} for ${chosen.name || chosen.email}`, {
          tone: "success",
          detail: "It is in the queue, and the customer can see it in their portal.",
        });
      }
      ctx.api.invalidate("/api/v1/staff/tickets");
      ctx.api.invalidate("/api/v1/portal/tickets");
      ctx.navigate(ticketHref(thread.ticket.id));
    });
  });

  return h(
    "div",
    { class: "stack" },
    card({
      title: "New ticket on a customer's behalf",
      sub: "The customer is told nothing by this screen; reply on the ticket to send them a message.",
      body: form,
    }),
    card({
      body: h(
        "p",
        { class: "field-hint" },
        icon("info", 12),
        " The ticket records your account as the opener and the customer as the account it was opened for, so the audit trail says who started the conversation.",
      ),
    }),
  );
}

// ------------------------------------------------------------ the picker --

interface CustomerSearch {
  el: HTMLElement;
  query(value: string): Promise<void>;
  focus(): void;
}

/** A debounced search over the staff customer list.
 *
 *  It searches the same endpoint the customers screen uses rather than a new one: "find a customer"
 *  is a solved problem in this panel, and a second implementation would be a second set of
 *  filters that disagree. */
function buildCustomerSearch(
  ctx: Ctx,
  resultsHost: HTMLElement,
  onChoose: (match: CustomerMatch) => void | Promise<void>,
): CustomerSearch {
  let timer: number | undefined;
  let latest = 0;

  const run = async (value: string) => {
    const term = value.trim();
    if (term.length < MIN_SEARCH) {
      swap(resultsHost, h("span", { class: "field-hint" }, "Type at least two characters."));
      return;
    }
    const request = ++latest;
    swap(resultsHost, h("span", { class: "field-hint" }, "Searching…"));
    try {
      const body = await ctx.api.get<{ data?: unknown }>(
        `/api/v1/staff/customers?q=${encodeURIComponent(term)}&limit=8`,
        { cache: false },
      );
      if (request !== latest) return;
      const rows = Array.isArray(body.data) ? (body.data as CustomerMatch[]) : [];
      if (rows.length === 0) {
        swap(resultsHost, h("span", { class: "field-hint" }, `No customer matches “${term}”.`));
        return;
      }
      swap(
        resultsHost,
        h(
          "div",
          { class: "stack-sm ticket-customer-results" },
          ...rows.map((row) =>
            h(
              "button",
              {
                type: "button",
                class: "btn btn--ghost btn--sm ticket-customer-result",
                onclick: () => void onChoose(row),
              },
              icon("user", 12),
              h("span", null, row.name || row.email),
              h("span", { class: "faint" }, row.email),
            ),
          ),
        ),
      );
    } catch (err) {
      if (request !== latest) return;
      swap(
        resultsHost,
        h(
          "span",
          { class: "field-hint" },
          err instanceof ApiError ? `The search failed: ${err.detail || err.code}` : "The search failed.",
        ),
      );
    }
  };

  const box = searchInput({
    placeholder: "Search by name or email address",
    oninput: (value: string) => {
      if (timer !== undefined) window.clearTimeout(timer);
      timer = window.setTimeout(() => void run(value), 220);
    },
  });

  return {
    el: box,
    query: run,
    focus() {
      const element = box.querySelector("input");
      if (element) element.focus();
    },
  };
}

// ------------------------------------------------------------ attachments --

interface RawFilePicker {
  el: HTMLElement;
  files: () => File[];
}

/** A native multi-file input with the selected names listed. The list is the point: an agent
 *  should see which file is the 11 MB one before the ticket is created, not after. */
function rawFilePicker(): RawFilePicker {
  let chosen: File[] = [];
  const list = h("div", { class: "ticket-files" });
  const picker = h("input", { type: "file", id: "staff-ticket-file-input", class: "sr-only", multiple: true });
  const label = h(
    "label",
    { class: "btn btn--sm ticket-file-btn", for: "staff-ticket-file-input" },
    icon("plus", 13),
    "Choose files",
  );

  const render = () => {
    if (chosen.length === 0) {
      swap(list, h("span", { class: "faint" }, "No files selected."));
      return;
    }
    swap(
      list,
      h(
        "div",
        { class: "stack-sm" },
        ...chosen.map((file, index) =>
          h(
            "div",
            { class: "ticket-file" },
            icon("download", 12),
            h("span", { class: "ticket-file-name", title: file.name }, file.name),
            h("span", { class: "ticket-file-size" }, bytesLabel(file.size)),
            h(
              "button",
              {
                class: "btn btn--ghost btn--sm ticket-file-remove",
                type: "button",
                title: `Remove ${file.name}`,
                "aria-label": `Remove ${file.name}`,
                onclick: () => {
                  chosen = chosen.filter((_, i) => i !== index);
                  render();
                },
              },
              icon("x", 12),
            ),
          ),
        ),
      ),
    );
  };

  picker.addEventListener("change", () => {
    for (const file of Array.from(picker.files ?? [])) {
      if (!chosen.some((existing) => existing.name === file.name && existing.size === file.size)) {
        chosen.push(file);
      }
    }
    picker.value = "";
    render();
  });

  render();

  return {
    el: h("div", { class: "stack-sm" }, h("div", { class: "row-wrap" }, label), list),
    files: () => chosen,
  };
}

// ----------------------------------------------------------------- copy --

function titleFromKey(value: string): string {
  if (!value) return "—";
  return value
    .replace(/[_-]+/g, " ")
    .replace(/\b\w/g, (character) => character.toUpperCase());
}

/** One sentence per documented failure. The code is the contract; the detail is the hint. */
function createFailure(err: unknown): HTMLElement {
  if (!(err instanceof ApiError)) {
    return banner("danger", "The ticket could not be opened", "The server could not be reached. Nothing was created.");
  }
  switch (err.code) {
    case "required_field_missing":
      // The handler names the field in the detail.
      return banner("danger", "A field this desk asks for is blank", err.detail || "Fill it in and send again.");
    case "invalid_input":
      return banner("danger", "Something in the form was refused", err.detail || "Check the subject, department and message.");
    case "not_found":
      return banner(
        "danger",
        "That record is not available",
        "The customer or service may have been removed. Search for the customer again.",
      );
    case "tickets_disabled":
      return banner("warning", "Support tickets are switched off", "This installation is not taking tickets right now.");
    case "rate_limited":
      return banner("warning", "Too many attempts", "Wait a minute before opening another ticket.");
    default:
      return banner("danger", "The ticket could not be opened", err.detail || err.code);
  }
}
