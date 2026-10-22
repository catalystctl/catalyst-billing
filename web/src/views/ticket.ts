// One ticket, as a conversation.
//
// The screen is the same for both audiences, for the same reason the list is: `/tickets/:id` is
// one route and the server decides which thread it will answer. What differs is the controls —
// staff get assignment and the status/priority/department fields, a customer gets a reply box and
// a close button — and both are gated by `ctx.can()` rather than by a guess about the role. The
// server refuses what a permission does not allow, so the UI hiding an action is a courtesy and
// the button never appears where the request would fail.
//
// Three details are deliberate:
//
//   - **An attachment is a link to the API path**, never a blob URL. The handler serves the bytes
//     with `Content-Disposition: attachment`, `nosniff` and a sandboxing CSP; fetching the file
//     and handing the browser an object URL would discard all three and render an uploaded HTML
//     page on our own origin. `download` also tells the router to leave the navigation alone.
//   - **The thread is rendered from `messages`, not from `last_message`.** The row's summary is
//     for the list; the thread is the record.
//   - **A closed ticket is not a dead end.** The reply box stays, because a reply reopens the
//     ticket — that is what `touchTicket` does — and the screen says so instead of hiding the
//     one control the customer needs.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime, humanStatus, relative, titleCase } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  avatar,
  banner,
  btn,
  card,
  emptyState,
  errorState,
  field,
  input,
  kv,
  pageHead,
  select,
  skeletonPage,
  textarea,
  withBusy,
} from "../ui/kit";
import { openDialog } from "../ui/dialog";
import {
  assignTicket,
  attachmentLink,
  bytesLabel,
  clientGaps,
  gapExplanation,
  humanTicketStatus,
  isStaffTickets,
  normaliseThread,
  patchTicket,
  ticketPath,
  ticketPriorityBadge,
  ticketStatusBadge,
  unavailableReason,
  uploadTicketAttachment,
  type TicketMessage,
  type TicketMeta,
  type TicketRow,
  type TicketThread,
} from "./tickets";

export const meta = { title: "Ticket", crumb: "Ticket" };

const DEFAULT_MAX_ATTACHMENT_BYTES = 10 * 1024 * 1024;

export default async function ticketPage(ctx: Ctx): Promise<Node> {
  const id = ctx.route.params.id ?? "";
  const staff = isStaffTickets(ctx);

  if (!id) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Ticket" }),
      card({
        flush: true,
        body: emptyState({
          title: "There is no ticket in this address",
          body: "The link has no ticket identifier in it. The list is the way back.",
          icon: "message",
          action: btn("All tickets", { href: "/tickets", size: "sm" }),
        }),
      }),
    );
  }

  const host = h("div", { class: "page" }, skeletonPage());
  const controller = new AbortController();
  ctx.onDispose(() => controller.abort());
  let loading = false;

  const load = async (quiet = false) => {
    if (loading) return;
    loading = true;
    if (!quiet) swap(host, skeletonPage());
    try {
      const [threadPayload, meta] = await Promise.all([
        ctx.api.get<unknown>(ticketPath(ctx, id), { cache: false, signal: controller.signal }),
        // The meta request is only needed for the staff edit controls and the attachment cap. A
        // failure here must not take the thread down, so it degrades to the defaults below.
        loadMeta(ctx, staff, controller.signal),
      ]);
      if (controller.signal.aborted) return;
      const thread = normaliseThread(threadPayload);
      swap(host, renderThread(ctx, thread, meta, () => void load(true)));
    } catch (err) {
      if (controller.signal.aborted || (err instanceof DOMException && err.name === "AbortError")) return;
      const unavailable = unavailableReason(err);
      if (unavailable) {
        swap(
          host,
          h(
            "div",
            { class: "stack" },
            pageHead({ title: "Ticket" }),
            card({
              flush: true,
              body: emptyState({
                title: unavailable.title,
                body: unavailable.body,
                icon: "lock",
                action: btn("All tickets", { href: "/tickets", size: "sm" }),
              }),
            }),
          ),
        );
        return;
      }
      swap(
        host,
        errorState(err, () => {
          void load();
        }),
      );
    } finally {
      loading = false;
    }
  };

  // A reply, a close or an assignment moves the thread. The event carries the ticket id, so a
  // thread that is not this one is ignored.
  ctx.onLive((event) => {
    const kind = typeof event.kind === "string" ? event.kind : "";
    if (!kind.startsWith("ticket.")) return;
    const ticketID = typeof event.ticket_id === "string" ? event.ticket_id : "";
    if (ticketID && ticketID !== id) return;
    void load(true);
  });

  void load();
  return host;
}

async function loadMeta(ctx: Ctx, staff: boolean, signal: AbortSignal): Promise<TicketMeta | null> {
  if (!staff) {
    // A customer needs the cap only, and the default matches the server's constant. Skipping the
    // request keeps a customer's thread to one round trip.
    return null;
  }
  try {
    return await ctx.api.get<TicketMeta>(`/api/v1/staff/tickets/meta`, { cache: false, signal });
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") throw err;
    return null;
  }
}

// ----------------------------------------------------------------- render --

function renderThread(ctx: Ctx, thread: TicketThread, meta: TicketMeta | null, reload: () => void): Node {
  const ticket = thread.ticket;
  const messages = thread.messages;
  const staff = isStaffTickets(ctx);
  const canWrite = ctx.can(staff ? "ticket:write:all" : "ticket:write:self");
  const canClose = canWrite && !(meta?.client_closing_disabled && !staff);

  ctx.setTitle(ticket.subject ? `${ticket.number || "Ticket"} · ${ticket.subject}` : "Ticket");
  ctx.setCrumbs([{ label: "Tickets", href: "/tickets" }, { label: ticket.number || "Ticket" }]);

  const head = pageHead({
    title: ticket.subject || "(no subject)",
    badge: h("span", { class: "row-wrap" }, ticketStatusBadge(ticket.status), ticketPriorityBadge(ticket.priority)),
    sub: h(
      "span",
      { class: "row-wrap ticket-head-sub" },
      h("span", { class: "mono" }, ticket.number || "—"),
      h("span", { class: "dot-sep" }, "·"),
      h("span", null, titleCase(ticket.department)),
      h("span", { class: "dot-sep" }, "·"),
      h("span", null, `opened ${relative(ticket.created_at)}`),
      ticket.customer
        ? h(
            "span",
            { class: "row-wrap" },
            h("span", { class: "dot-sep" }, "·"),
            h(
              "a",
              { class: "linkish", href: `/customers/${encodeURIComponent(ticket.customer.id)}` },
              ticket.customer.name || ticket.customer.email,
            ),
          )
        : null,
    ),
    actions: h(
      "div",
      { class: "row" },
      btn("All tickets", { href: "/tickets", size: "sm", icon: "arrowLeft" }),
      canClose && ticket.status !== "closed"
        ? closeButton(ctx, ticket, reload)
        : null,
    ),
  });

  const threadCard = card({
    title: "Conversation",
    sub: `${messages.length} message${messages.length === 1 ? "" : "s"}`,
    flush: true,
    body:
      messages.length === 0
        ? emptyState({
            title: "No messages on this ticket",
            body: "The ticket exists but carries no message. Reply below to start the conversation.",
            icon: "inbox",
          })
        : h("div", { class: "ticket-thread" }, ...messages.map((message) => messageBlock(ctx, message, staff))),
  });

  const replyCard = canWrite
    ? card({
        title: ticket.status === "closed" ? "Reply and reopen" : "Reply",
        sub:
          ticket.status === "closed"
            ? "A reply reopens this ticket, so it returns to the queue."
            : "Your reply is added to the thread and the team is notified.",
        body: replyForm(ctx, ticket, reload, meta),
      })
    : card({
        title: "Reply",
        body: h(
          "p",
          { class: "page-sub" },
          "Your account can read this ticket but not add to it. Ask an administrator for the ticket:write permission if that is wrong.",
        ),
      });

  const side = h(
    "div",
    { class: "stack" },
    detailsCard(ticket, staff),
    staff ? staffControls(ctx, ticket, meta, reload) : null,
  );

  return h("div", { class: "stack" }, head, h("div", { class: "split" }, h("div", { class: "stack" }, threadCard, replyCard), side));
}

function detailsCard(ticket: TicketRow, staff: boolean): HTMLElement {
  return card({
    title: "Details",
    body: h(
      "div",
      null,
      kv("Number", h("span", { class: "mono" }, ticket.number || "—")),
      kv("Status", ticketStatusBadge(ticket.status)),
      kv("Priority", ticketPriorityBadge(ticket.priority)),
      kv("Department", h("span", null, titleCase(ticket.department))),
      staff
        ? kv(
            "Assignee",
            ticket.assigned_to
              ? h("span", null, ticket.assignee_name || "Assigned")
              : h("span", { class: "faint" }, "Unassigned"),
          )
        : null,
      kv("Messages", h("span", { class: "mono" }, String(ticket.message_count))),
      kv("Opened", h("span", { title: dateTime(ticket.created_at) }, dateTime(ticket.created_at))),
      ticket.last_reply_at ? kv("Last reply", h("span", { title: dateTime(ticket.last_reply_at) }, dateTime(ticket.last_reply_at))) : null,
      ticket.closed_at ? kv("Closed", h("span", { title: dateTime(ticket.closed_at) }, dateTime(ticket.closed_at))) : null,
      ticket.service_id
        ? kv("Service", h("a", { class: "linkish mono", href: `/portal/services/${encodeURIComponent(ticket.service_id)}` }, ticket.service_id))
        : null,
    ),
  });
}

function messageBlock(ctx: Ctx, message: TicketMessage, staff: boolean): HTMLElement {
  const authorName = message.author?.name?.trim() || (message.from_staff ? "Support" : "You");
  // "You" is only true for the reader; on a staff screen a customer's message is theirs, so the
  // label is the author's own name and the side is the signal.
  const mine = !staff && !message.from_staff;
  const attachments = Array.isArray(message.attachments) ? message.attachments : [];

  return h(
    "article",
    { class: `ticket-msg${message.from_staff ? " ticket-msg--staff" : ""}${mine ? " ticket-msg--mine" : ""}` },
    h(
      "header",
      { class: "ticket-msg-head" },
      avatar(authorName, { size: "sm", accent: message.from_staff }),
      h("span", { class: "ticket-msg-author" }, authorName),
      message.from_staff ? h("span", { class: "ticket-msg-role" }, "Support team") : null,
      h("span", { class: "spacer" }),
      h("span", { class: "ticket-msg-time", title: dateTime(message.created_at) }, dateTime(message.created_at)),
    ),
    // The body is a text node, never markup: a message is whatever the sender typed, and the
    // panel must not interpret it. `white-space: pre-wrap` in the stylesheet is what keeps the
    // newlines the sender wrote.
    h("div", { class: "ticket-msg-body" }, message.body),
    attachments.length > 0
      ? h("div", { class: "ticket-msg-files" }, ...attachments.map((attachment) => attachmentLink(attachment)))
      : null,
    message.message_id ? h("div", { class: "ticket-msg-meta" }, `via email · ${message.message_id}`) : null,
  );
}

// ------------------------------------------------------------------ reply --

function replyForm(ctx: Ctx, ticket: TicketRow, reload: () => void, meta: TicketMeta | null): HTMLElement {
  const maxBytes =
    typeof meta?.max_attachment_bytes === "number" && meta.max_attachment_bytes > 0
      ? meta.max_attachment_bytes
      : DEFAULT_MAX_ATTACHMENT_BYTES;

  const body = textarea({
    id: "ticket-reply",
    placeholder: ticket.status === "closed" ? "Reply to reopen this ticket…" : "Add to the conversation…",
    maxlength: 32_000,
  });
  body.rows = 5;

  // The attach control is only drawn when the client can actually send a raw body. A picker that
  // silently drops the file is worse than no picker: the customer believes they sent it.
  const gaps = clientGaps(ctx);
  const file = h("input", { type: "file", id: "ticket-reply-file", class: "sr-only" });
  const fileLabel = h(
    "label",
    { class: "btn btn--sm ticket-file-btn", for: "ticket-reply-file" },
    icon("plus", 13),
    "Attach a file",
  );
  const fileNote = h("span", { class: "faint ticket-file-note" }, `Up to ${bytesLabel(maxBytes)}.`);
  let chosen: File | null = null;
  file.addEventListener("change", () => {
    chosen = (file.files ?? [])[0] ?? null;
    swap(
      fileNote,
      h(
        "span",
        { class: chosen && chosen.size > maxBytes ? "ticket-file-over" : undefined },
        chosen ? `${chosen.name} · ${bytesLabel(chosen.size)}` : `Up to ${bytesLabel(maxBytes)}.`,
      ),
    );
  });

  const errorHost = h("div");
  const submit = btn("Send reply", { variant: "primary", type: "submit", icon: "send" }) as HTMLButtonElement;

  const form = h(
    "form",
    { class: "stack", novalidate: true },
    errorHost,
    field({ label: "Reply", control: body, id: "ticket-reply" }),
    gaps.canUpload
      ? h("div", { class: "row-wrap" }, fileLabel, fileNote, h("span", { class: "spacer" }), submit)
      : h(
          "div",
          { class: "stack-sm" },
          banner("info", "File uploads are not available in this build", gapExplanation("upload")),
          h("div", { class: "row-wrap" }, h("span", { class: "spacer" }), submit),
        ),
  );

  form.addEventListener("submit", (event) => {
    event.preventDefault();
    void withBusy(ctx, submit, async () => {
      errorHost.replaceChildren();
      const text = body.value.trim();
      if (!text) {
        errorHost.replaceChildren(banner("danger", "The reply is empty", "Write something before sending."));
        body.focus();
        return;
      }
      if (chosen && chosen.size > maxBytes) {
        errorHost.replaceChildren(
          banner("danger", `That file is over the ${bytesLabel(maxBytes)} limit`, `${chosen.name} is ${bytesLabel(chosen.size)}. Remove it and send the reply without it.`),
        );
        return;
      }

      const staff = isStaffTickets(ctx);
      const path = staff
        ? `/api/v1/staff/tickets/${encodeURIComponent(ticket.id)}/replies`
        : `/api/v1/portal/tickets/${encodeURIComponent(ticket.id)}/replies`;

      let messageID: string | undefined;
      try {
        const message = await ctx.api.post<{ id?: string }>(path, { body: text });
        messageID = typeof message?.id === "string" ? message.id : undefined;
      } catch (err) {
        errorHost.replaceChildren(replyFailure(err));
        return;
      }

      body.value = "";
      if (chosen) {
        const pending = chosen;
        chosen = null;
        file.value = "";
        swap(fileNote, h("span", null, `Up to ${bytesLabel(maxBytes)}.`));
        try {
          await uploadTicketAttachment(ctx, ticket.id, pending, messageID);
        } catch (err) {
          ctx.toast("The reply was sent, but the file did not upload", {
            tone: "danger",
            duration: 0,
            detail: err instanceof ApiError ? err.detail || err.code : "The server could not be reached.",
          });
        }
      }

      ctx.toast("Reply sent", { tone: "success" });
      ctx.api.invalidate("/api/v1/portal/tickets");
      ctx.api.invalidate("/api/v1/staff/tickets");
      reload();
    });
  });

  return form;
}

function replyFailure(err: unknown): HTMLElement {
  if (!(err instanceof ApiError)) {
    return banner("danger", "The reply was not sent", "The server could not be reached. Nothing was added.");
  }
  switch (err.code) {
    case "not_found":
      return banner("danger", "This ticket is no longer available", "It may have been deleted, or it belongs to another account.");
    case "invalid_input":
      return banner("danger", "The reply was refused", err.detail || "The message is empty or too long.");
    case "duplicate_message":
      return banner("info", "This message was already received", "The thread already carries it.");
    case "rate_limited":
      return banner("warning", "Too many replies", "Wait a moment before sending another.");
    default:
      return banner("danger", "The reply was not sent", err.detail || err.code);
  }
}

// ------------------------------------------------------------------ close --

function closeButton(ctx: Ctx, ticket: TicketRow, reload: () => void): HTMLButtonElement {
  const button = btn("Close ticket", { variant: "ghost", size: "sm", icon: "checkCircle" }) as HTMLButtonElement;
  button.addEventListener("click", () => {
    void withBusy(ctx, button, async () => {
      const confirmed = await ctx.confirm({
        title: `Close ${ticket.number || "this ticket"}?`,
        body: "Closing ends the conversation and takes the ticket out of the queue. A reply reopens it, so nothing is lost — but if the problem is solved, closing it is what stops it being chased.",
        confirmLabel: "Close ticket",
      });
      if (!confirmed) return;
      const staff = isStaffTickets(ctx);
      const path = staff
        ? `/api/v1/staff/tickets/${encodeURIComponent(ticket.id)}/close`
        : `/api/v1/portal/tickets/${encodeURIComponent(ticket.id)}/close`;
      try {
        await ctx.api.post(path, {});
      } catch (err) {
        if (err instanceof ApiError && err.code === "client_closing_disabled") {
          ctx.toast("Only the support team can close this ticket", {
            tone: "info",
            detail: "Reply instead, and ask them to close it when you are done.",
          });
          return;
        }
        if (err instanceof ApiError) {
          ctx.toast(err.detail || "The ticket could not be closed", { tone: "danger", detail: err.code });
          return;
        }
        ctx.toast("The ticket could not be closed", { tone: "danger" });
        return;
      }
      ctx.toast("Ticket closed", { tone: "success", detail: "A reply reopens it." });
      ctx.api.invalidate("/api/v1/portal/tickets");
      ctx.api.invalidate("/api/v1/staff/tickets");
      reload();
    });
  });
  return button;
}

// ------------------------------------------------------------------ staff --

function staffControls(ctx: Ctx, ticket: TicketRow, meta: TicketMeta | null, reload: () => void): HTMLElement {
  const canWrite = ctx.can("ticket:write:all");
  if (!canWrite) {
    return card({
      title: "Queue controls",
      body: h("p", { class: "page-sub" }, "Changing a ticket needs ticket:write:all. You can read this thread but not move it."),
    });
  }

  const gaps = clientGaps(ctx);
  const statuses = Array.isArray(meta?.statuses) && meta.statuses.length > 0 ? meta.statuses : ["open", "replied", "closed"];
  const priorities = Array.isArray(meta?.priorities) && meta.priorities.length > 0 ? meta.priorities : ["low", "normal", "high"];
  const departments = Array.isArray(meta?.departments) ? meta.departments.filter(Boolean) : [];

  const statusSelect = select(
    statuses.map((value) => ({ value, label: humanTicketStatus(value) })),
    { id: "ticket-edit-status", value: ticket.status },
  );
  const prioritySelect = select(
    priorities.map((value) => ({ value, label: titleCase(value) })),
    { id: "ticket-edit-priority", value: ticket.priority },
  );
  const departmentSelect = select(
    departments.length > 0
      ? departments.map((value) => ({ value, label: titleCase(value) }))
      : [{ value: ticket.department, label: titleCase(ticket.department) }],
    { id: "ticket-edit-department", value: ticket.department },
  );

  const noticeHost = h("div");
  const save = btn("Save changes", { variant: "primary", size: "sm", type: "submit" }) as HTMLButtonElement;

  // The edit is a PATCH, which this build's client cannot send. Rather than a form that always
  // fails, the three fields are shown read-only with the reason, and the actions that *do* work
  // (assign, close, reply) stay available — a support agent is not left with nothing.
  const form = gaps.canPatch
    ? h(
        "form",
        { class: "stack-sm", novalidate: true },
        field({ label: "Status", control: statusSelect, id: "ticket-edit-status" }),
        field({ label: "Priority", control: prioritySelect, id: "ticket-edit-priority" }),
        field({
          label: "Department",
          control: departmentSelect,
          id: "ticket-edit-department",
          hint: departments.length === 0 ? "No departments are configured; the current one is kept." : undefined,
        }),
        noticeHost,
        h("div", { class: "row-wrap" }, save),
      )
    : h(
        "div",
        { class: "stack-sm" },
        banner("info", "Status, priority and department cannot be changed here yet", gapExplanation("patch")),
        h(
          "div",
          { class: "stack-sm" },
          kv("Status", humanTicketStatus(ticket.status)),
          kv("Priority", titleCase(ticket.priority)),
          kv("Department", titleCase(ticket.department)),
        ),
      );

  form.addEventListener("submit", (event) => {
    event.preventDefault();
    void withBusy(ctx, save, async () => {
      noticeHost.replaceChildren();
      // Only what moved is sent: the handler treats an omitted field as "leave it alone", and
      // sending all three would write an audit row claiming three changes where one happened.
      const changes: { status?: string; priority?: string; department?: string } = {};
      if (statusSelect.value !== ticket.status) changes.status = statusSelect.value;
      if (prioritySelect.value !== ticket.priority) changes.priority = prioritySelect.value;
      if (departmentSelect.value !== ticket.department) changes.department = departmentSelect.value;
      if (Object.keys(changes).length === 0) {
        noticeHost.replaceChildren(banner("info", "Nothing has changed", "Move a field first."));
        return;
      }
      try {
        await patchTicket(ctx, ticket.id, changes);
      } catch (err) {
        noticeHost.replaceChildren(
          err instanceof ApiError
            ? banner("danger", "The change was refused", err.detail || err.code)
            : banner("danger", "The change was not saved", "The server could not be reached."),
        );
        return;
      }
      ctx.toast("Ticket updated", { tone: "success", detail: Object.keys(changes).join(", ") });
      ctx.api.invalidate("/api/v1/staff/tickets");
      reload();
    });
  });

  const assign = btn("Assign…", { variant: "ghost", size: "sm", icon: "user" }) as HTMLButtonElement;
  assign.addEventListener("click", () => openAssign(ctx, ticket, reload));

  return card({
    title: "Queue controls",
    sub: "Every change is audited under your name.",
    body: h("div", { class: "stack" }, form, h("div", { class: "row-wrap" }, assign)),
  });
}

/** The assign dialog.
 *
 *  There is no endpoint that lists staff, so this asks for a user id rather than inventing a
 *  picker over a list that does not exist. "Assign to me" is the one case the server can resolve
 *  without an id — the queue's `assignee=me` — and it is offered first because it is what an
 *  agent does most. */
function openAssign(ctx: Ctx, ticket: TicketRow, reload: () => void): void {
  const userID = input({ id: "ticket-assignee", placeholder: "User id (uuid)", autocomplete: "off" });
  const errorHost = h("div");

  const me = btn("Assign to me", { variant: "primary", icon: "user" }) as HTMLButtonElement;
  const submit = btn("Assign", { variant: "default", type: "submit" }) as HTMLButtonElement;
  const unassign = btn("Unassign", { variant: "ghost" }) as HTMLButtonElement;
  const cancel = btn("Cancel", { variant: "ghost" });

  const form = h(
    "form",
    { class: "stack", novalidate: true },
    banner(
      "info",
      "Assignment is a user id",
      "The API takes the id of the staff account that will own the ticket, and there is no endpoint that lists staff accounts. Your own id is the one this screen can supply without asking.",
    ),
    field({
      label: "Staff user id",
      control: userID,
      id: "ticket-assignee",
      hint: "Leave empty to unassign. The id is the account's uuid, not its email address.",
    }),
    errorHost,
  );

  const handle = openDialog({
    title: `Assign ${ticket.number || "ticket"}`,
    subtitle: "Who is answering this?",
    body: form,
    footer: h("div", { class: "row spacer" }, cancel, unassign, submit, me),
    locked: () => submit.disabled || me.disabled || unassign.disabled,
  });

  cancel.addEventListener("click", () => handle.close());

  const run = async (assignee: string, label: string) => {
    errorHost.replaceChildren();
    submit.disabled = true;
    me.disabled = true;
    unassign.disabled = true;
    try {
      await assignTicket(ctx, ticket.id, assignee);
    } catch (err) {
      errorHost.replaceChildren(
        err instanceof ApiError
          ? banner("danger", "The ticket was not reassigned", err.detail || err.code)
          : banner("danger", "The ticket was not reassigned", "The server could not be reached."),
      );
      return;
    } finally {
      submit.disabled = false;
      me.disabled = false;
      unassign.disabled = false;
    }
    handle.close();
    ctx.toast(label, { tone: "success" });
    ctx.api.invalidate("/api/v1/staff/tickets");
    reload();
  };

  me.addEventListener("click", () => void run(ctx.session.user.id, "Assigned to you"));
  unassign.addEventListener("click", () => void run("", "Ticket unassigned"));
  form.addEventListener("submit", (event) => {
    event.preventDefault();
    const value = userID.value.trim();
    if (!value) {
      errorHost.replaceChildren(banner("danger", "A user id is required", "Use Unassign to clear the assignment instead."));
      return;
    }
    void run(value, "Ticket assigned");
  });
}

// Re-exported so the stylesheet's vocabulary and the screen's use of `humanStatus` stay honest:
// a status this build does not know renders as its own words rather than as a blank cell.
export { humanStatus };
