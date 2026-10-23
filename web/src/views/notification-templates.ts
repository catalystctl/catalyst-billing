// Notification templates.
//
// Every message the panel sends comes from a row in `notification_templates`, and this is the
// screen that edits those rows. Three facts about the data shape it:
//
//  - **Two kinds of event.** A *transactional* event (a password reset, an email confirmation) is
//    the only way a customer can finish something they started, so a customer cannot opt out of
//    it: an edit to its wording reaches everybody who triggers it. Every other event is optional
//    — a customer may have turned it off — so an edit there reaches whoever still wants it. The
//    distinction decides whether an edit is safe, so it is a column, not a footnote.
//  - **The wording is a template with a vocabulary.** An event supplies a fixed set of
//    placeholders ({{invoice_number}}, {{total}}, …). A placeholder an event does not supply
//    would be rendered to a customer as literal text, so the server refuses it at save time and
//    again in the preview. The vocabulary is fetched from the server rather than written down
//    here: a copy in this file would offer a word the server rejects and hide one it accepts.
//  - **The preview is the renderer.** What this screen shows is the server's own rendering with
//    the event's sample data, on both channels — the mail body with absolute links, and the inbox
//    row with panel-relative ones. An operator sees the sentence before a customer does.
//
// The editor is a master–detail page rather than a dialog: the body of a mail is a paragraph of
// prose, and a paragraph in a modal is a paragraph nobody reads twice.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { relative } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  badge,
  banner,
  btn,
  card,
  emptyState,
  field,
  input,
  pageHead,
  resource,
  spinner,
  textarea,
  withBusy,
} from "../ui/kit";

export const meta = { title: "Notification templates", crumb: "Notification templates" };

// ---------------------------------------------------------------- payloads --

/** One template, from `templateJSON` in internal/httpapi/notifications.go. When the event has no
 *  row yet the list carries only `event_key`, `name`, `force` and `seeded: false`. */
interface TemplateRow {
  event_key: string;
  name: string;
  subject?: string;
  body?: string;
  in_app_title?: string;
  in_app_body?: string;
  in_app_url?: string;
  enabled?: boolean;
  mail_enabled?: boolean;
  in_app_enabled?: boolean;
  cc_addresses?: string;
  bcc_addresses?: string;
  force: boolean;
  seeded: boolean;
  updated_at?: string;
}

interface TemplateList {
  data: TemplateRow[];
}

/** One placeholder an event supplies, from `handleTemplateVariables`. */
interface TemplateVariable {
  name: string;
  description: string;
  sample: string;
}

interface VariableList {
  data: Array<{ event_key: string; name: string; force: boolean; variables: TemplateVariable[] }>;
}

/** The preview response, from `handlePreviewTemplate`. */
interface PreviewResponse {
  event_key: string;
  force: boolean;
  mail: { subject: string; body: string; cc_addresses: string[] | null; bcc_addresses: string[] | null };
  in_app: { title: string; body: string; url: string };
}

/** The editable fields, sent to both the preview and the save route. */
interface TemplateDraft {
  name: string;
  subject: string;
  body: string;
  in_app_title: string;
  in_app_body: string;
  in_app_url: string;
  enabled: boolean;
  mail_enabled: boolean;
  in_app_enabled: boolean;
  cc_addresses: string;
  bcc_addresses: string;
}

export default async function notificationTemplatesPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("notification:write")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Notification templates" }),
      banner(
        "danger",
        "You cannot edit the notification templates",
        "This screen needs the notification:write permission. Support answers “why did I get this”; it does not decide what the mail says.",
        btn("Back to the dashboard", { href: "/", size: "sm", variant: "ghost" }),
      ),
    );
  }

  const load = async (signal: AbortSignal) => {
    const [list, vocabulary] = await Promise.all([
      ctx.api.get<TemplateList>("/api/v1/staff/notification-templates", { cache: false, signal }),
      ctx.api.get<VariableList>("/api/v1/staff/notification-template-variables", { cache: false, signal }),
    ]);
    return { templates: list.data ?? [], vocabulary: vocabulary.data ?? [] };
  };

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Notification templates",
      sub: "What each event's message says. The wording here is what a customer reads — in their inbox and in their mail.",
    }),
    resource(ctx, load, (data) => {
      if (data.templates.length === 0) {
        return card({
          body: emptyState({
            title: "The notification catalogue is empty",
            body: "This build ships no events, so there is nothing to word. A template row is only ever created for an event that exists.",
            icon: "bell",
          }),
        });
      }
      return editor(ctx, data.templates, data.vocabulary);
    }),
  );
}

// ------------------------------------------------------------------- page --

/** editor renders the master–detail page: the events on the left, the selected one on the right.
 *
 *  It keeps the selection in a closure and repaints the whole layout on save, which is the
 *  simplest thing that cannot show one event's wording under another event's name. */
function editor(
  ctx: Ctx,
  templates: TemplateRow[],
  vocabulary: VariableList["data"],
): HTMLElement {
  const variables = new Map<string, TemplateVariable[]>();
  for (const entry of vocabulary) variables.set(entry.event_key, entry.variables);

  let selected = ctx.route.query.get("event") ?? templates[0]?.event_key ?? "";
  if (!templates.some((t) => t.event_key === selected)) selected = templates[0]?.event_key ?? "";

  const host = h("div");

  const paint = () => {
    const nav = h("nav", { class: "settings-nav", "aria-label": "Notification events" });
    for (const row of templates) {
      nav.appendChild(
        h(
          "a",
          {
            class: `settings-link${row.event_key === selected ? " is-active" : ""}`,
            href: `#${row.event_key}`,
            onclick: (event: MouseEvent) => {
              event.preventDefault();
              selected = row.event_key;
              ctx.replaceQuery({ event: selected });
              paint();
            },
          },
          icon(row.force ? "shield" : "bell", 14),
          h("span", { class: "row spacer" }, row.name),
          row.force
            ? h("span", { class: "badge badge--accent", title: "Transactional: a customer cannot opt out" }, "T")
            : null,
        ),
      );
    }

    const current = templates.find((t) => t.event_key === selected) ?? templates[0];
    const column = h(
      "div",
      { class: "stack" },
      current
        ? templateEditor(ctx, current, variables.get(current.event_key) ?? [], {
            onSaved: (saved) => {
              // The list is the source of truth for the nav's labels and the updated stamp, so it
              // is patched from the save's response rather than reloaded: a save can change the
              // name too. `seeded` is set here because the save response carries the row, and the
              // row now exists by definition.
              const index = templates.findIndex((t) => t.event_key === saved.event_key);
              if (index >= 0) templates[index] = { ...templates[index], ...saved, seeded: true } as TemplateRow;
              paint();
            },
          })
        : emptyState({ title: "No events", body: "This build ships no notification events.", icon: "bell" }),
    );

    swap(host, h("div", { class: "settings-layout" }, nav, column));
  };

  paint();
  return host;
}

// ---------------------------------------------------------------- editor --

function templateEditor(
  ctx: Ctx,
  row: TemplateRow,
  variables: TemplateVariable[],
  hooks: { onSaved: (saved: TemplateRow) => void },
): HTMLElement {
  const draft: TemplateDraft = {
    name: row.name ?? "",
    subject: row.subject ?? "",
    body: row.body ?? "",
    in_app_title: row.in_app_title ?? "",
    in_app_body: row.in_app_body ?? "",
    in_app_url: row.in_app_url ?? "",
    enabled: row.enabled ?? true,
    mail_enabled: row.mail_enabled ?? true,
    in_app_enabled: row.in_app_enabled ?? true,
    cc_addresses: row.cc_addresses ?? "",
    bcc_addresses: row.bcc_addresses ?? "",
  };

  const name = input({ id: `tpl-name-${row.event_key}`, value: draft.name, maxlength: 80 });
  const subject = input({ id: `tpl-subject-${row.event_key}`, value: draft.subject, maxlength: 200 });
  const body = textarea({
    id: `tpl-body-${row.event_key}`,
    value: draft.body,
    maxlength: 8000,
    placeholder: "The message, as plain text. Line breaks are kept; markup is not interpreted.",
  });
  body.rows = 12;
  const inAppTitle = input({ id: `tpl-title-${row.event_key}`, value: draft.in_app_title, maxlength: 200 });
  const inAppBody = textarea({ id: `tpl-ibody-${row.event_key}`, value: draft.in_app_body, maxlength: 500 });
  inAppBody.rows = 3;
  const inAppURL = input({
    id: `tpl-url-${row.event_key}`,
    value: draft.in_app_url,
    maxlength: 300,
    class: "input--mono",
    placeholder: "/portal/invoices/…",
  });
  const cc = input({ id: `tpl-cc-${row.event_key}`, value: draft.cc_addresses, class: "input--mono", placeholder: "billing@example.test" });
  const bcc = input({ id: `tpl-bcc-${row.event_key}`, value: draft.bcc_addresses, class: "input--mono", placeholder: "archive@example.test" });

  const enabled = h("input", { type: "checkbox", checked: draft.enabled });
  const mailEnabled = h("input", { type: "checkbox", checked: draft.mail_enabled });
  const inAppEnabled = h("input", { type: "checkbox", checked: draft.in_app_enabled });

  // The live preview. It is the server's renderer, called with the draft, so what is on screen is
  // what would be sent — including the refusal of a placeholder this event does not supply.
  const previewHost = h("div", { class: "stack-sm" }, h("span", { class: "field-hint" }, "Rendering…"));
  const errorHost = h("div");
  let timer: number | undefined;
  ctx.onDispose(() => window.clearTimeout(timer));

  const currentDraft = (): TemplateDraft => ({
    name: name.value.trim(),
    subject: subject.value,
    body: body.value,
    in_app_title: inAppTitle.value,
    in_app_body: inAppBody.value,
    in_app_url: inAppURL.value,
    enabled: enabled.checked,
    mail_enabled: mailEnabled.checked,
    in_app_enabled: inAppEnabled.checked,
    cc_addresses: cc.value,
    bcc_addresses: bcc.value,
  });

  const renderPreview = async (): Promise<void> => {
    swap(previewHost, h("div", { class: "row" }, spinner(), h("span", { class: "field-hint" }, "Rendering…")));
    try {
      const preview = await ctx.api.post<PreviewResponse>(
        `/api/v1/staff/notification-templates/${encodeURIComponent(row.event_key)}/preview`,
        currentDraft(),
      );
      swap(previewHost, previewView(preview));
      errorHost.replaceChildren();
    } catch (err) {
      if (err instanceof DOMException && err.name === "AbortError") return;
      if (err instanceof ApiError && err.code === "unknown_placeholder") {
        swap(
          previewHost,
          banner(
            "danger",
            "This event does not supply that placeholder",
            err.detail || "Fix the placeholder before saving: a customer would read the braces as text.",
          ),
        );
        return;
      }
      swap(
        previewHost,
        banner("warning", "The preview could not be rendered", err instanceof ApiError ? err.detail || err.code : "The server could not be reached."),
      );
    }
  };

  const schedule = () => {
    window.clearTimeout(timer);
    timer = window.setTimeout(() => void renderPreview(), 350);
  };
  for (const control of [subject, body, inAppTitle, inAppBody, inAppURL]) {
    control.addEventListener("input", schedule);
  }
  // The preview is the point of this page, and it is not an answer to a keystroke: the stored
  // wording deserves to be rendered the moment the event is selected. Without this the card
  // keeps the "Rendering…" placeholder it was built with until the operator happens to type,
  // which reads as a preview that never arrives.
  void renderPreview();

  /** Inserts a placeholder at the caret of the mail body, which is where an operator writes
   *  prose. The subject and the inbox fields take the same word, so it is offered once. */
  const insertPlaceholder = (word: string) => {
    const text = `{{${word}}}`;
    const start = body.selectionStart ?? body.value.length;
    const end = body.selectionEnd ?? start;
    body.value = body.value.slice(0, start) + text + body.value.slice(end);
    body.focus();
    body.selectionStart = body.selectionEnd = start + text.length;
    schedule();
  };

  const vocabulary = variables.length
    ? h(
        "div",
        { class: "row-wrap" },
        h("span", { class: "field-label" }, "This event supplies"),
        ...variables.map((v) =>
          h(
            "button",
            {
              class: "chip mono",
              type: "button",
              title: `${v.description} — sample: ${v.sample}`,
              onclick: () => insertPlaceholder(v.name),
            },
            `{{${v.name}}}`,
          ),
        ),
      )
    : h("p", { class: "field-hint" }, "This event supplies no placeholders.");

  const save = btn("Save template", { variant: "primary", icon: "check" }) as HTMLButtonElement;
  save.addEventListener("click", () => {
    void withBusy(ctx, save, async () => {
      errorHost.replaceChildren();
      try {
        const saved = await ctx.api.put<TemplateRow>(
          `/api/v1/staff/notification-templates/${encodeURIComponent(row.event_key)}`,
          currentDraft(),
        );
        ctx.toast("Template saved", {
          tone: "success",
          detail: row.force
            ? `${row.name} · transactional, so this wording now reaches every customer who triggers it`
            : `${row.name} · optional, so only customers who still have it on will receive it`,
        });
        hooks.onSaved(saved);
      } catch (err) {
        swap(errorHost, saveError(row, err));
      }
    });
  });

  const reset = btn("Reset to the shipped wording", { variant: "ghost", icon: "undo" }) as HTMLButtonElement;
  reset.addEventListener("click", () => {
    void withBusy(ctx, reset, async () => {
      const ok = await ctx.confirm({
        title: `Reset “${row.name}” to the wording this build ships?`,
        body: "The stored subject, body, inbox text and channel toggles are replaced. Anything you wrote is gone — copy it somewhere first if you may want it back.",
        confirmLabel: "Reset the wording",
        danger: true,
      });
      if (!ok) return;
      try {
        const saved = await ctx.api.post<TemplateRow>(
          `/api/v1/staff/notification-templates/${encodeURIComponent(row.event_key)}/reset`,
          {},
        );
        ctx.toast("Template reset", { tone: "success", detail: row.name });
        hooks.onSaved(saved);
      } catch (err) {
        swap(errorHost, saveError(row, err));
      }
    });
  });

  const header = h(
    "div",
    { class: "row-wrap" },
    badge(row.force ? "Transactional" : "Optional", row.force ? "accent" : "neutral", { dot: true }),
    row.seeded
      ? null
      : badge("No template row yet", "warning", { dot: true }),
    row.updated_at ? h("span", { class: "dim" }, `last changed ${relative(row.updated_at)}`) : null,
    h("span", { class: "mono faint" }, row.event_key),
  );

  const explain = row.force
    ? "A customer cannot turn this event off: it is the only way to finish something they started. An edit here reaches every customer who triggers it."
    : "A customer may have turned this event off in their notification preferences. An edit here reaches the customers who still have it on.";

  return h(
    "div",
    { class: "stack" },
    card({
      title: row.name,
      sub: row.event_key,
      actions: h("div", { class: "row" }, reset, save),
      body: h(
        "div",
        { class: "stack" },
        header,
        banner(row.force ? "info" : "neutral", row.force ? "Transactional" : "Optional", explain),
        row.seeded
          ? null
          : banner(
              "warning",
              "This event has no stored template",
              "The seed has not run in this installation, so the shipped wording is being used. Saving here creates the row.",
            ),
        errorHost,
        h(
          "div",
          { class: "form-grid" },
          h("div", { class: "span-2" }, field({ label: "Display name", control: name, hint: "What this screen calls the event. It is not sent to anyone." })),
          h("div", { class: "span-2" }, field({ label: "Subject", control: subject, hint: "The mail subject. A substituted value is stripped of newlines, so it cannot inject a header." })),
          h("div", { class: "span-2" }, field({ label: "Body", control: body, hint: "Plain text. Line breaks are kept, markup is not interpreted — the message is sent as text/plain." })),
          h("div", { class: "span-2" }, vocabulary),
          field({ label: "Inbox title", control: inAppTitle }),
          field({ label: "Inbox link", control: inAppURL, hint: "A panel path, e.g. /portal/invoices/…" }),
          h("div", { class: "span-2" }, field({ label: "Inbox body", control: inAppBody, hint: "One line in the customer's notification list." })),
          field({ label: "CC", control: cc, hint: "Comma-separated. Each address gets its own copy." }),
          field({ label: "BCC", control: bcc, hint: "Comma-separated. Each address gets its own copy." }),
          h("div", { class: "span-2" }, h("label", { class: "switch" }, enabled, "Send this event at all")),
          h("div", { class: "span-2" }, h("label", { class: "switch" }, mailEnabled, "Send the mail")),
          h("div", { class: "span-2" }, h("label", { class: "switch" }, inAppEnabled, "Write the inbox row")),
        ),
      ),
    }),
    card({
      title: "Preview",
      sub: "Rendered with this event's sample data",
      body: previewHost,
    }),
  );
}

// ---------------------------------------------------------------- preview --

/** previewView renders the two channels the server would produce. */
function previewView(preview: PreviewResponse): HTMLElement {
  const recipients = [...(preview.mail.cc_addresses ?? []), ...(preview.mail.bcc_addresses ?? [])];
  return h(
    "div",
    { class: "stack" },
    h(
      "div",
      { class: "stack-sm" },
      h("span", { class: "field-label" }, "Mail"),
      h("div", { class: "kv" }, h("span", { class: "kv-key" }, "Subject"), h("span", { class: "kv-value" }, preview.mail.subject || h("span", { class: "faint" }, "(empty)"))),
      h("div", { class: "doc-party" }, preview.mail.body || "(empty)"),
      recipients.length > 0
        ? h("p", { class: "field-hint" }, `Also copied to ${recipients.join(", ")}.`)
        : null,
    ),
    h(
      "div",
      { class: "stack-sm" },
      h("span", { class: "field-label" }, "Inbox row"),
      h(
        "div",
        { class: "kv" },
        h("span", { class: "kv-key" }, "Title"),
        h("span", { class: "kv-value" }, preview.in_app.title || h("span", { class: "faint" }, "(empty)")),
      ),
      h("p", { class: "dim" }, preview.in_app.body || "(empty)"),
      h("p", { class: "field-hint mono" }, preview.in_app.url || "(no link)"),
    ),
    h(
      "p",
      { class: "field-hint" },
      "The sample values are the event's own examples, not a customer's data. The mail body carries absolute links; the inbox row keeps the panel path.",
    ),
  );
}

// ----------------------------------------------------------------- errors --

/** saveError turns a refused save into a banner with the reason an operator can act on. */
function saveError(row: TemplateRow, err: unknown): HTMLElement {
  if (!(err instanceof ApiError)) {
    return banner("danger", "The template was not saved", "The server could not be reached. Nothing was changed.");
  }
  switch (err.code) {
    case "unknown_placeholder":
      return banner(
        "danger",
        "This event does not supply that placeholder",
        err.detail || "A customer would read the braces as text. Remove it or use one of the placeholders above.",
      );
    case "not_found":
      return banner("danger", "That event no longer exists", "Reload the page: this build's catalogue has changed.");
    case "forbidden":
      return banner("danger", "You do not hold notification:write", "The template was not saved.");
    case "template_missing":
      return banner(
        "warning",
        "This event has no template row",
        "Run the seed (the worker does it on boot) and try again.",
      );
    default:
      return banner("danger", "The template was not saved", err.detail || `The server refused it (${err.code}).`);
  }
}

