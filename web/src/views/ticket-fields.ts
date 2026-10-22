// Ticket fields.
//
// What a ticket asks for, and what must be answered before it can be closed. This screen is the
// definition side of that: the fields themselves, not a ticket's answers (those live on the ticket).
//
// Three decisions shape it:
//
//   - **A field is text, select or checkbox**, and the type is fixed once a ticket has answered it.
//     The server refuses a type change or an option removal that would strand an answer, and this
//     screen shows the type as a badge rather than a control for a field with answers: an operator
//     who wants a different type archives this field and adds another, which is the only change
//     that keeps last year's tickets readable.
//   - **The two requirements are separate columns, not one switch.** "Required to open" is triage —
//     it refuses a ticket that does not answer the question. "Required to close" is the record — it
//     refuses the close and names the field. A field can be either, both, or neither, and an
//     operator needs to see the difference at a glance.
//   - **Remove is archive.** A definition that tickets point at is never deleted, because its
//     answers are part of a closed ticket's history. Archiving stops it being asked and stops it
//     being required; the answers stay. The screen says so, because "remove" that keeps the data
//     must be honest about keeping it.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { titleCase } from "../lib/format";
import type { Ctx } from "../lib/types";
import {
  badge,
  banner,
  btn,
  card,
  emptyState,
  errorState,
  field,
  input,
  pageHead,
  select,
  skeletonPage,
  textarea,
  withBusy,
} from "../ui/kit";
import { bindForm, openDialog } from "../ui/dialog";
import type { Column } from "../ui/table";
import type { TicketField } from "./tickets";

export const meta = { title: "Ticket fields", crumb: "Ticket fields" };

/** The closed set of types, with the sentence that explains each one. The server sends the set too
 *  (`types`), and this is the label for a value in it; a type this build does not know renders as
 *  its own words rather than as a blank cell. */
const TYPE_LABELS: Record<string, string> = {
  text: "Text",
  select: "Choose one",
  checkbox: "Checkbox",
};

const TYPE_HINTS: Record<string, string> = {
  text: "A single line of text, e.g. a server name or a callback number.",
  select: "One of a fixed list of options. Use this when the answers must be reportable.",
  checkbox: "A yes/no box. A field required at close must be ticked, not merely shown.",
};

interface FieldList {
  fields: TicketField[];
  types: string[];
}

export default async function ticketFieldsPage(ctx: Ctx): Promise<Node> {
  // The navigation hides this from anyone without ticket:read:all and the router bounces them;
  // this is the sentence instead of a screen that fails on load.
  if (!ctx.can("ticket:read:all")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Ticket fields" }),
      card({
        flush: true,
        body: emptyState({
          title: "This screen is for the support team",
          body: "Reading what the desk asks for needs ticket:read:all. Your own tickets are in your account, under Tickets.",
          icon: "lock",
          action: btn("Your tickets", { href: "/tickets", size: "sm", icon: "message" }),
        }),
      }),
    );
  }

  const canWrite = ctx.can("ticket:write:all");
  const host = h("div", { class: "stack" }, skeletonPage());

  let fields: TicketField[] = [];
  let types: string[] = ["text", "select", "checkbox"];
  let loading = false;

  const load = async () => {
    if (loading) return;
    loading = true;
    try {
      const body = await ctx.api.get<{ fields?: unknown; types?: unknown }>(
        "/api/v1/staff/ticket-fields?include_archived=true",
        { cache: false },
      );
      fields = Array.isArray(body.fields) ? (body.fields as TicketField[]) : [];
      types = Array.isArray(body.types) && body.types.length > 0 ? (body.types as string[]) : types;
      swap(host, render());
    } catch (err) {
      swap(host, errorState(err, () => void load()));
    } finally {
      loading = false;
    }
  };

  const reload = () => {
    void load();
  };

  // ---------------------------------------------------------------- the list --

  const columns: Column<TicketField>[] = [
    {
      key: "label",
      label: "Field",
      primary: true,
      render: (row) =>
        h(
          "div",
          { class: "cell-main" },
          h(
            "div",
            { class: "primary" },
            h("span", { class: "row-title" }, row.label),
            row.archived ? badge("Archived", "neutral", { dot: true }) : null,
          ),
          h("div", { class: "secondary" }, h("span", { class: "mono" }, row.key)),
        ),
    },
    {
      key: "type",
      label: "Type",
      hideSmall: true,
      render: (row) => badge(TYPE_LABELS[row.type] ?? titleCase(row.type), row.type === "select" ? "info" : "neutral"),
    },
    {
      key: "options",
      label: "Options",
      hideSmall: true,
      render: (row) =>
        row.type === "select" && Array.isArray(row.options) && row.options.length > 0
          ? h("span", { class: "dim" }, row.options.join(", "))
          : h("span", { class: "faint" }, "—"),
    },
    {
      key: "create",
      label: "To open",
      align: "right",
      render: (row) =>
        row.required_on_create
          ? badge("Required", "warning")
          : h("span", { class: "faint" }, "optional"),
    },
    {
      key: "close",
      label: "To close",
      align: "right",
      render: (row) =>
        row.required_to_close ? badge("Required", "warning") : h("span", { class: "faint" }, "optional"),
    },
    {
      key: "answers",
      label: "Answers",
      align: "right",
      hideSmall: true,
      render: (row) =>
        row.answer_count > 0
          ? h("span", { class: "mono", title: "Tickets that have answered this field" }, String(row.answer_count))
          : h("span", { class: "faint" }, "0"),
    },
    {
      key: "actions",
      label: "",
      align: "right",
      width: "1%",
      render: (row) =>
        canWrite
          ? h(
              "div",
              { class: "row" },
              btn("Edit", { size: "sm", variant: "ghost", icon: "edit", onclick: () => openFieldDialog(row) }),
              row.archived
                ? btn("Restore", {
                    size: "sm",
                    variant: "ghost",
                    icon: "refresh",
                    title: "Ask for this field again",
                    onclick: () => void setArchived(row, false),
                  })
                : btn("Archive", {
                    size: "sm",
                    variant: "ghost",
                    icon: "layers",
                    title: "Stop asking for this field. Answers already given are kept.",
                    onclick: () => void archive(row),
                  }),
            )
          : null,
    },
  ];

  // ---------------------------------------------------------------- render --

  const render = (): Node => {
    const table = h(
      "table",
      { class: "table" },
      h(
        "thead",
        null,
        h(
          "tr",
          null,
          ...columns.map((column) =>
            h(
              "th",
              {
                scope: "col",
                class: [column.align === "right" ? "num" : "", column.hideSmall ? "hide-sm" : ""]
                  .filter(Boolean)
                  .join(" "),
                width: column.width,
              },
              column.label,
            ),
          ),
        ),
      ),
      h(
        "tbody",
        null,
        ...fields.map((row) =>
          h(
            "tr",
            { class: row.archived ? "is-muted" : "" },
            ...columns.map((column) => {
              const content = column.render ? column.render(row) : (row as unknown as Record<string, unknown>)[column.key];
              return h(
                "td",
                {
                  class: [column.align === "right" ? "num" : "", column.hideSmall ? "hide-sm" : ""]
                    .filter(Boolean)
                    .join(" "),
                  "data-label": column.label,
                },
                content as Node,
              );
            }),
          ),
        ),
      ),
    );

    const empty = fields.length === 0;
    const active = fields.filter((row) => !row.archived).length;

    return h(
      "div",
      { class: "stack" },
      pageHead({
        title: "Ticket fields",
        sub: "What a ticket asks for, and what must be answered before it can be closed. Fields are asked on every ticket, including ones the team opens on a customer's behalf.",
        actions: canWrite
          ? btn("Add a field", { variant: "primary", size: "sm", icon: "plus", onclick: () => openFieldDialog(null) })
          : null,
      }),
      empty
        ? card({
            flush: true,
            body: emptyState({
              title: "No ticket fields",
              body: "A ticket asks for a subject, a department and a message. Anything else — the server name, the region, a confirmation that the customer tried a restart — is a field you define here.",
              icon: "sliders",
              action: canWrite
                ? btn("Add the first field", { variant: "primary", icon: "plus", onclick: () => openFieldDialog(null) })
                : null,
            }),
          })
        : card({
            flush: true,
            body: h("div", { class: "table-wrap table-wrap--cards" }, table),
            foot: h(
              "span",
              { class: "faint" },
              `${active} field${active === 1 ? "" : "s"} in use${fields.length > active ? `, ${fields.length - active} archived` : ""}. Archived fields are no longer asked for, and the answers already given stay on the tickets that carry them.`,
            ),
          }),
      h(
        "div",
        { class: "grid-2" },
        card({
          title: "Required to open",
          body: h(
            "p",
            { class: "page-sub" },
            "A ticket cannot be created while this field is blank — by the customer, or by an agent opening one on their behalf. Use it for what the first reply would otherwise have to ask for.",
          ),
        }),
        card({
          title: "Required to close",
          body: h(
            "p",
            { class: "page-sub" },
            "Closing is refused until the field is answered, and the refusal names the field. Use it for the record a closed ticket should carry: the root cause, the resolution, the customer's confirmation.",
          ),
        }),
      ),
    );
  };

  // ------------------------------------------------------------- mutations --

  const setArchived = async (row: TicketField, archived: boolean) => {
    try {
      await ctx.api.patch<TicketField>(`/api/v1/staff/ticket-fields/${encodeURIComponent(row.id)}`, { archived });
      ctx.toast(archived ? "Field archived" : "Field restored", {
        tone: "success",
        detail: archived ? "Answers already given are kept." : `${row.label} is asked for again.`,
      });
      reload();
    } catch (err) {
      report(err, archived ? "The field could not be archived" : "The field could not be restored");
    }
  };

  const archive = async (row: TicketField) => {
    const confirmed = await ctx.confirm({
      title: `Archive “${row.label}”?`,
      body:
        row.answer_count > 0
          ? `It stops being asked and stops being required. The ${row.answer_count} ticket${row.answer_count === 1 ? "" : "s"} that answered it keep the answer — a retired field is not a lost record.`
          : "It stops being asked and stops being required. Nothing has answered it yet, so nothing changes on any ticket.",
      confirmLabel: "Archive field",
    });
    if (!confirmed) return;
    await setArchived(row, true);
  };

  const report = (err: unknown, fallback: string) => {
    if (err instanceof ApiError) {
      ctx.toast(err.detail || fallback, { tone: "danger", detail: err.code });
      return;
    }
    ctx.toast(fallback, { tone: "danger", detail: "The server could not be reached." });
  };

  // ---------------------------------------------------------------- dialog --

  const openFieldDialog = (existing: TicketField | null) => {
    const isEdit = existing !== null;
    const lockedType = isEdit && existing.answer_count > 0;

    const label = input({
      id: "field-label",
      value: existing?.label ?? "",
      placeholder: "Server name",
      maxlength: 120,
      autocomplete: "off",
    });
    const key = input({
      id: "field-key",
      value: existing?.key ?? "",
      placeholder: "server_name",
      class: "input--mono",
      maxlength: 64,
      autocomplete: "off",
    });
    const type = select(
      types.map((value) => ({ value, label: TYPE_LABELS[value] ?? titleCase(value) })),
      { id: "field-type", value: existing?.type ?? "text" },
    );
    if (lockedType) type.disabled = true;

    const options = textarea({
      id: "field-options",
      placeholder: "eu-west\nus-east\nap-south",
      value: Array.isArray(existing?.options) ? existing?.options.join("\n") : "",
      maxlength: 4000,
    });
    options.rows = 5;

    const requiredOnCreate = h("input", { type: "checkbox", checked: existing?.required_on_create ?? false });
    const requiredToClose = h("input", { type: "checkbox", checked: existing?.required_to_close ?? false });

    const optionsField = field({
      label: "Options",
      control: options,
      id: "field-options",
      hint: "One per line. The answer must be one of them, and removing an option a ticket already chose is refused.",
    });

    // The option list is only meaningful for a select, so it is hidden for the other types rather
    // than shown and ignored.
    const syncOptions = () => {
      optionsField.hidden = type.value !== "select";
    };
    type.addEventListener("change", syncOptions);
    syncOptions();

    const errorHost = h("div");
    const submit = btn(isEdit ? "Save field" : "Add field", { variant: "primary", type: "submit" });
    const cancel = btn("Cancel", { variant: "ghost" });

    const body = h(
      "div",
      { class: "stack" },
      field({
        label: "Label",
        control: label,
        id: "field-label",
        required: true,
        hint: "What the form calls it, and what a refused close names. “Server name”, not “server_name”.",
      }),
      field({
        label: "Key",
        control: key,
        id: "field-key",
        required: true,
        hint: "The stable slug the API stores the answer under: lower-case letters, digits and underscores. Immutable in practice — clients send answers under it.",
      }),
      field({
        label: "Type",
        control: type,
        id: "field-type",
        hint: lockedType
          ? "Tickets already answer this field, so its type cannot change. Archive it and add another if you need a different shape."
          : TYPE_HINTS[type.value] ?? "",
      }),
      optionsField,
      h(
        "div",
        { class: "stack-sm" },
        h("label", { class: "switch" }, requiredOnCreate, h("span", null, "Required to open a ticket")),
        h("label", { class: "switch" }, requiredToClose, h("span", null, "Required to close a ticket")),
      ),
    );

    const form = h("form", { class: "stack", novalidate: true }, errorHost, body);
    bindForm(form, submit);

    const handle = openDialog({
      title: isEdit ? `Edit “${existing.label}”` : "New ticket field",
      subtitle: isEdit
        ? "The label, the requirements and (for a select nobody has answered yet) the options can change."
        : "A field is asked on every ticket. Required to open refuses a ticket; required to close refuses the close.",
      body: h("div", { class: "stack" }, form),
      footer: h("div", { class: "row spacer" }, cancel, submit),
      wide: true,
      locked: () => submit instanceof HTMLButtonElement && submit.disabled,
    });
    cancel.addEventListener("click", () => handle.close());

    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void withBusy(ctx, submit, async () => {
        errorHost.replaceChildren();

        const labelValue = label.value.trim();
        const keyValue = key.value.trim();
        if (!labelValue) {
          swap(errorHost, banner("danger", "A label is required", "It is what the form shows and what a refused close names."));
          label.focus();
          return;
        }
        if (!isEdit && !/^[a-z][a-z0-9_]{1,63}$/.test(keyValue)) {
          swap(
            errorHost,
            banner("danger", "That key is not valid", "Lower-case letters, digits and underscores, starting with a letter, 2 to 64 characters."),
          );
          key.focus();
          return;
        }
        const typeValue = type.value;
        const optionValues =
          typeValue === "select"
            ? options.value
                .split("\n")
                .map((line) => line.trim())
                .filter(Boolean)
            : [];
        if (typeValue === "select" && optionValues.length === 0) {
          swap(errorHost, banner("danger", "A choose-one field needs options", "Add at least one option, one per line."));
          options.focus();
          return;
        }
        if (new Set(optionValues).size !== optionValues.length) {
          swap(errorHost, banner("danger", "Two options are the same", "Each option must be distinct."));
          options.focus();
          return;
        }

        const payload: Record<string, unknown> = {
          label: labelValue,
          required_on_create: requiredOnCreate.checked,
          required_to_close: requiredToClose.checked,
        };
        if (!isEdit) {
          payload.key = keyValue;
          payload.type = typeValue;
        }
        // Only send the type back when it is unchanged and unlocked: an omitted type is "leave it
        // alone", which is exactly what an edit of a field with answers must do.
        if (isEdit && !lockedType && typeValue !== existing.type) payload.type = typeValue;
        if (typeValue === "select" && !lockedType) payload.options = optionValues;

        try {
          if (isEdit) {
            await ctx.api.patch<TicketField>(`/api/v1/staff/ticket-fields/${encodeURIComponent(existing.id)}`, payload);
          } else {
            await ctx.api.post<TicketField>("/api/v1/staff/ticket-fields", payload);
          }
          ctx.toast(isEdit ? "Field saved" : "Field added", { tone: "success", detail: labelValue });
          handle.close();
          reload();
        } catch (err) {
          swap(errorHost, banner("danger", "The field was not saved", fieldError(err)));
        }
      });
    });
  };

  await load();
  return h("div", { class: "page" }, host);
}

/** One sentence per documented failure. The code is the contract; the detail is the hint. */
function fieldError(err: unknown): string {
  if (!(err instanceof ApiError)) return "The server could not be reached. Nothing was saved.";
  switch (err.code) {
    case "invalid_input":
      return err.detail || "Check the key, the label and the options.";
    case "not_found":
      return "That field no longer exists. Reload the screen.";
    case "rate_limited":
      return "Too many changes. Wait a moment and try again.";
    default:
      return err.detail || err.code;
  }
}

export type { TicketField };
