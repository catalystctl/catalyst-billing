// Opening a ticket.
//
// The form is drawn from `GET /api/v1/portal/tickets/meta`, not from a list in this file: the
// departments are whatever the operator configured, the priorities are the closed set the
// database accepts, and the attachment cap is the same `tickets.MaxAttachmentBytes` the handler
// enforces. A form that hard-coded "general, billing, technical" would refuse a department the
// server accepts and offer one it rejects.
//
// Two refusals happen on this side of the wire, and both are courtesies rather than controls:
//
//   - A file over the cap is refused before anything is uploaded, with the cap and the file's
//     size in the message. The server checks the declared length before reading the body and
//     reads through a LimitReader, so an oversized upload is 413 either way; being told *why*
//     before a 10 MB upload is the difference between a form and a puzzle.
//   - An empty subject or body never leaves the browser, because the handler would answer 400
//     `invalid_input` and the customer would have to guess which field it meant.
//
// Attachments are uploaded after the ticket exists, because the endpoint is
// `POST /api/v1/portal/tickets/{id}/attachments` — a file belongs to a message, and a message
// belongs to a ticket. A failed upload does not lose the ticket: the screen says which files did
// not make it and links to the thread, where the reply box can attach them.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
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
  withBusy,
} from "../ui/kit";
import {
  bytesLabel,
  clientGaps,
  gapExplanation,
  normaliseThread,
  ticketHref,
  ticketsMetaPath,
  unavailableReason,
  uploadTicketAttachment,
  type TicketMeta,
  type TicketThread,
} from "./tickets";

export const meta = { title: "New ticket", crumb: "New ticket" };

/** The fallback cap, used only when the meta request has not answered yet. It is the same
 *  constant the handler enforces (`tickets.MaxAttachmentBytes`), so a screen that renders before
 *  the meta arrives still refuses at the right size instead of accepting a file the server will
 *  reject. */
const DEFAULT_MAX_ATTACHMENT_BYTES = 10 * 1024 * 1024;

interface PortalServiceOption {
  id: string;
  name: string;
  status: string;
}

export default async function ticketNewPage(ctx: Ctx): Promise<Node> {
  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Open a ticket",
      sub: "Describe what is happening. The more precise the first message, the fewer round trips it takes.",
      actions: btn("Back to tickets", { href: "/tickets", size: "sm", icon: "arrowLeft" }),
    }),
    resource(
      ctx,
      async (signal) => {
        // Two requests, one screen: the form's configuration and the customer's services. The
        // service picker is optional, so a failed services request must not take the form down
        // with it — it degrades to "no service" rather than to an error page.
        const meta = await loadMeta(ctx, signal);
        const services = await loadServices(ctx, signal);
        return { meta, services };
      },
      (data) => renderForm(ctx, data.meta, data.services),
      { skeleton: () => skeletonPage() },
    ),
  );
}

async function loadMeta(ctx: Ctx, signal: AbortSignal): Promise<TicketMeta> {
  try {
    return await ctx.api.get<TicketMeta>(ticketsMetaPath(ctx), { cache: false, signal });
  } catch (err) {
    const unavailable = unavailableReason(err);
    if (unavailable) throw unavailable;
    throw err;
  }
}

/** The customer's own services, for the optional "what is this about" picker. A 403/404 here
 *  means the login has no billing customer, which is exactly the state in which the picker
 *  should be absent rather than empty. */
async function loadServices(ctx: Ctx, signal: AbortSignal): Promise<PortalServiceOption[]> {
  try {
    const body = await ctx.api.get<{ services?: unknown }>("/api/v1/portal/services", { cache: false, signal });
    const rows = Array.isArray(body.services) ? (body.services as PortalServiceOption[]) : [];
    return rows.filter((svc) => typeof svc?.id === "string");
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") throw err;
    return [];
  }
}

function renderForm(ctx: Ctx, meta: TicketMeta, services: PortalServiceOption[]): Node {
  // `enabled: false` is the operator having switched the desk off. There is no form to draw and
  // no retry that would help.
  if (meta.enabled === false) {
    return card({
      flush: true,
      body: emptyState({
        title: "Support tickets are switched off",
        body: "This installation is not taking new tickets. Your invoices, services and billing details still work, and the team's address is on the invoices they sent you.",
        icon: "lock",
        action: btn("Back to overview", { href: "/portal", size: "sm", icon: "home" }),
      }),
    });
  }

  const departments = Array.isArray(meta.departments) ? meta.departments.filter(Boolean) : [];
  const priorities = Array.isArray(meta.priorities) && meta.priorities.length > 0 ? meta.priorities : ["low", "normal", "high"];
  const maxBytes =
    typeof meta.max_attachment_bytes === "number" && meta.max_attachment_bytes > 0
      ? meta.max_attachment_bytes
      : DEFAULT_MAX_ATTACHMENT_BYTES;
  const maxSubject = typeof meta.max_subject_length === "number" && meta.max_subject_length > 0 ? meta.max_subject_length : 200;

  const subject = input({
    id: "ticket-subject",
    name: "subject",
    placeholder: "A one-line summary, e.g. “Server unreachable since 09:10”",
    maxlength: maxSubject,
    autocomplete: "off",
    required: true,
  });

  const department = select(
    departments.map((value) => ({ value, label: titleFromKey(value) })),
    { id: "ticket-department", name: "department", value: departments[0] ?? "" },
  );

  // `normal` is the server's own default for an omitted priority; selecting it here means the
  // form shows the value the ticket will actually get.
  const priority = select(
    priorities.map((value) => ({ value, label: titleFromKey(value) })),
    { id: "ticket-priority", name: "priority", value: priorities.includes("normal") ? "normal" : (priorities[0] ?? "") },
  );

  const service = select(
    [
      { value: "", label: services.length > 0 ? "Not about a specific service" : "No services on this account" },
      ...services.map((svc) => ({ value: svc.id, label: svc.name || svc.id })),
    ],
    { id: "ticket-service", name: "service_id", value: "" },
  );
  if (services.length === 0) service.disabled = true;

  const message = textarea({
    id: "ticket-message",
    name: "message",
    placeholder: "What is happening, what you expected, and what you have already tried.",
    maxlength: 32_000,
  });
  message.rows = 8;

  // A file picker that cannot upload is a form that loses work: the customer chooses a file,
  // submits, and only then learns the client cannot send it. So the picker is present only when
  // `ctx.api` can carry a raw body, and when it cannot the form says why and still works.
  const gaps = clientGaps(ctx);
  const files = gaps.canUpload ? filePicker(maxBytes) : null;

  const errorHost = h("div");
  const submit = btn("Open ticket", { variant: "primary", type: "submit", icon: "send" }) as HTMLButtonElement;
  const cancel = btn("Cancel", { href: "/tickets", variant: "ghost" });

  const form = h(
    "form",
    { class: "stack", novalidate: true },
    errorHost,
    h(
      "div",
      { class: "grid-2" },
      field({
        label: "Subject",
        control: subject,
        id: "ticket-subject",
        required: true,
        hint: `Up to ${maxSubject} characters. This is what the queue sees first.`,
      }),
      field({
        label: "Department",
        control: department,
        id: "ticket-department",
        hint: "Where it lands. Choose the closest match; a misrouted ticket is forwarded, not lost.",
      }),
      field({ label: "Priority", control: priority, id: "ticket-priority", hint: "High is for something broken now." }),
      field({
        label: "Service",
        control: service,
        id: "ticket-service",
        hint:
          services.length > 0
            ? "Optional. Naming the service puts its record in front of whoever answers."
            : "This account has no services, so there is nothing to pick.",
      }),
    ),
    field({
      label: "Message",
      control: message,
      id: "ticket-message",
      required: true,
      hint: "Error text, timestamps and what you changed last are all useful.",
    }),
    files
      ? field({
          label: "Attachments",
          control: files.el,
          id: "ticket-files",
          hint: `Optional, up to ${bytesLabel(maxBytes)} per file. They are uploaded after the ticket is created.`,
        })
      : field({
          label: "Attachments",
          control: h("div", { class: "stack-sm" }, banner("info", "File uploads are not available in this build", gapExplanation("upload"))),
          id: "ticket-files",
          hint: `Files are capped at ${bytesLabel(maxBytes)}. This screen cannot attach one yet, so the ticket is created without it — reply from the thread once uploads work.`,
        }),
    h("div", { class: "row-wrap" }, submit, cancel),
  );

  form.addEventListener("submit", (event) => {
    event.preventDefault();
    void withBusy(ctx, submit, async () => {
      errorHost.replaceChildren();

      const subjectValue = subject.value.trim();
      const bodyValue = message.value.trim();
      if (!subjectValue) {
        errorHost.replaceChildren(banner("danger", "A subject is required", "One line is enough: it is the queue's first impression."));
        subject.focus();
        return;
      }
      if (subjectValue.length > maxSubject) {
        errorHost.replaceChildren(
          banner("danger", "That subject is too long", `The limit is ${maxSubject} characters; yours is ${subjectValue.length}.`),
        );
        return;
      }
      if (!bodyValue) {
        errorHost.replaceChildren(banner("danger", "The first message cannot be empty", "Describe what is happening so the team can answer."));
        message.focus();
        return;
      }

      // The client-side cap. A file over it is named in the message rather than silently
      // dropped, and nothing is sent.
      const tooBig = (files?.files() ?? []).filter((file) => file.size > maxBytes);
      if (tooBig.length > 0) {
        errorHost.replaceChildren(
          banner(
            "danger",
            `That file is over the ${bytesLabel(maxBytes)} limit`,
            `${tooBig.map((f) => `${f.name} (${bytesLabel(f.size)})`).join(", ")} cannot be attached. Remove it, or upload it to the ticket afterwards.`,
          ),
        );
        return;
      }

      let thread: TicketThread;
      try {
        const created = await ctx.api.post<unknown>("/api/v1/portal/tickets", {
          subject: subjectValue,
          department: department.value,
          priority: priority.value,
          service_id: service.value || undefined,
          message: { body: bodyValue },
        });
        thread = normaliseThread(created);
      } catch (err) {
        errorHost.replaceChildren(createFailure(err));
        return;
      }

      // The ticket exists. Uploading now keeps the file on the first message, which is where
      // the reader expects it; a failure here is reported and the thread is still reached.
      const chosen = files?.files() ?? [];
      const failed: string[] = [];
      const firstMessage = thread.messages[0];
      for (const file of chosen) {
        try {
          await uploadTicketAttachment(ctx, thread.ticket.id, file, firstMessage?.id);
        } catch (err) {
          failed.push(`${file.name}${err instanceof ApiError ? ` (${err.detail || err.code})` : ""}`);
        }
      }

      if (failed.length > 0) {
        ctx.toast(`Ticket ${thread.ticket.number || "created"}, but ${failed.length} file${failed.length === 1 ? "" : "s"} did not upload`, {
          tone: "danger",
          duration: 0,
          detail: `${failed.join("; ")}. You can attach them from the ticket.`,
        });
      } else {
        ctx.toast(`Ticket ${thread.ticket.number || "opened"}`, {
          tone: "success",
          detail: "The team has it. Replies arrive here and by email.",
        });
      }
      ctx.api.invalidate("/api/v1/portal/tickets");
      ctx.navigate(ticketHref(thread.ticket.id));
    });
  });

  return h(
    "div",
    { class: "stack" },
    card({
      title: "New ticket",
      sub: "The subject and the first message are the two things support reads first.",
      body: form,
    }),
    card({
      body: h(
        "p",
        { class: "field-hint" },
        icon("info", 12),
        " A ticket is visible to you and to the support team. Nothing here is posted publicly.",
      ),
    }),
  );
}

// ------------------------------------------------------------ attachments --

interface FilePicker {
  el: HTMLElement;
  files: () => File[];
}

/** A native file input with a list of what is selected and a way to remove one.
 *
 *  The list is a real list rather than a count so a customer can see *which* file is the 11 MB
 *  one before submitting. `accept` is deliberately absent: the server normalises the stored
 *  content type and always serves it as an attachment, so the honest rule is a size cap, not an
 *  extension allowlist that would reject a `.log`. */
function filePicker(maxBytes: number): FilePicker {
  let chosen: File[] = [];

  const list = h("div", { class: "ticket-files" });
  const picker = h("input", { type: "file", id: "ticket-file-input", class: "sr-only", multiple: true });
  const label = h(
    "label",
    { class: "btn btn--sm ticket-file-btn", for: "ticket-file-input" },
    icon("plus", 13),
    "Choose files",
  );

  const render = () => {
    if (chosen.length === 0) {
      swap(list, h("span", { class: "faint" }, "No files selected."));
      return;
    }
    const rows = chosen.map((file, index) => {
        const over = file.size > maxBytes;
        return h(
          "div",
          { class: `ticket-file${over ? " ticket-file--over" : ""}` },
          icon(over ? "alertTriangle" : "download", 12),
          h("span", { class: "ticket-file-name", title: file.name }, file.name),
          h("span", { class: "ticket-file-size" }, bytesLabel(file.size)),
          over ? h("span", { class: "ticket-file-note" }, `over ${bytesLabel(maxBytes)}`) : null,
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
        );
      });
    swap(list, h("div", { class: "stack-sm" }, ...rows));
  };

  picker.addEventListener("change", () => {
    const picked = Array.from(picker.files ?? []);
    // Appending rather than replacing: choosing a second file should not discard the first, and
    // a file already in the list is not added twice.
    for (const file of picked) {
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

/** One sentence per documented creation failure. The code is the contract; the detail is a
 *  hint, and a detail that is missing must not become an empty banner. */
function createFailure(err: unknown): HTMLElement {
  if (!(err instanceof ApiError)) {
    return banner("danger", "The ticket could not be opened", "The server could not be reached. Nothing was created.");
  }
  switch (err.code) {
    case "tickets_disabled":
      return banner("warning", "Support tickets are switched off", "This installation is not taking new tickets.");
    case "invalid_input":
      return banner("danger", "Something in the form was refused", err.detail || "Check the subject, department, priority and message.");
    case "not_found":
      return banner(
        "danger",
        "That service is not on your account",
        "The service picker may be out of date. Reload the page and choose again, or leave the service empty.",
      );
    case "duplicate_message":
      return banner("warning", "This message was already received", "A ticket for it exists. Check your ticket list.");
    case "rate_limited":
      return banner("warning", "Too many attempts", "Wait a minute before opening another ticket.");
    default:
      return banner("danger", "The ticket could not be opened", err.detail || err.code);
  }
}

/** Re-exported for the thread screen, which reports the same failures from its reply box. */
export { createFailure };
export { errorState };
