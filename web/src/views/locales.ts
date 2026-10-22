// Locales and translations.
//
// This screen is two panes with one idea behind them: a locale is not a setting, it is a body of
// work. An operator adds `de`, and the honest question is immediately "how much of `de` is
// actually translated" — so coverage is on every row rather than behind a click, and the editor
// lists what is still missing for the locale being worked on.
//
// Four things the server decides and this screen must not second-guess:
//
//  - **Exactly one locale is the default.** The database enforces it with a partial unique index.
//    So "make default" is an action on a row, not a checkbox in a form: a checkbox would let an
//    operator express a state that cannot exist.
//  - **The default cannot be deleted or disabled.** The server refuses with `default_locale`, and
//    the screen renders that refusal as a sentence about what to do next ("make another locale the
//    default first") rather than as a rule restated.
//  - **A missing translation falls back, it never renders blank.** The editor therefore shows the
//    default locale's text beside an empty field, because that is what the customer is actually
//    seeing right now. An empty box with nothing beside it looks like a bug; an empty box beside
//    "Invoice paid" looks like work to do.
//  - **Deleting a translation is not deleting a key.** Removing a locale's row makes it fall back;
//    it does not remove the key from the installation. The screen says so where it matters.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime } from "../lib/format";
import { icon } from "../lib/icons";
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
  resource,
  skeletonPage,
  withBusy,
} from "../ui/kit";
import { openDialog } from "../ui/dialog";

export const meta = { title: "Locales", crumb: "Locales" };

// ---------------------------------------------------------------- payloads --

interface LocaleRow {
  code: string;
  name: string;
  native_name: string;
  enabled: boolean;
  is_default: boolean;
  sort_order: number;
  translated: number;
  total_keys: number;
  coverage: number;
  created_at: string;
  updated_at: string;
}

interface LocalesResponse {
  data: LocaleRow[];
  total_keys: number;
  default: string;
}

/** One key across every locale, from `i18n.Key`. */
interface CatalogKey {
  key: string;
  default: string;
  translations: Record<string, string> | null;
  missing: string[] | null;
}

const ENDPOINT = "/api/v1/staff/locales";

export default async function localesPage(ctx: Ctx): Promise<Node> {
  const host = h("div", { class: "resource" }, skeletonPage());
  const controller = new AbortController();
  ctx.onDispose(() => controller.abort());

  const load = async (): Promise<void> => {
    try {
      const res = await ctx.api.get<LocalesResponse>(ENDPOINT, { cache: false, signal: controller.signal });
      if (controller.signal.aborted) return;
      swap(host, render(ctx, res, load));
    } catch (err) {
      if (controller.signal.aborted) return;
      swap(host, errorState(err, () => {
        swap(host, skeletonPage());
        void load();
      }));
    }
  };
  void load();
  return host;
}

function render(ctx: Ctx, res: LocalesResponse, reload: () => Promise<void>): Node {
  const rows = res.data ?? [];
  // The default is the fallback text every other locale leans on, so the editor starts there
  // rather than on whichever row happened to be first.
  const editing = h("div");

  const addButton = btn("Add a locale", {
    icon: "plus",
    variant: "primary",
    onclick: () => openAddLocale(ctx, reload),
  });

  return h(
    "div",
    { class: "page stack" },
    pageHead({
      title: "Locales",
      sub: "Which languages the shop offers, and how much of each is translated.",
      actions: addButton,
    }),

    banner(
      "info",
      "A missing translation falls back rather than rendering blank",
      h(
        "p",
        null,
        "When a key has no text in a locale, the customer sees the default locale's text — never an empty label. Coverage below is how much of each locale is its own words.",
      ),
    ),

    rows.length === 0
      ? card({
          body: emptyState({
            title: "No locales",
            body: "The installation should always have at least the default locale. If this is empty, run the migrations.",
            icon: "globe",
          }),
        })
      : card({
          title: "Languages",
          sub: `${res.total_keys} key${res.total_keys === 1 ? "" : "s"} in the default catalogue.`,
          body: h(
            "table",
            { class: "table" },
            h(
              "thead",
              null,
              h("tr", null,
                h("th", null, "Language"),
                h("th", null, "Code"),
                h("th", null, "Coverage"),
                h("th", null, "State"),
                h("th", null, ""),
              ),
            ),
            h("tbody", null, ...rows.map((row) => localeRow(ctx, row, reload, editing))),
          ),
        }),

    editing,
  );
}

function localeRow(ctx: Ctx, row: LocaleRow, reload: () => Promise<void>, editing: HTMLElement): Node {
  const state: Node[] = [];
  if (row.is_default) state.push(badge("default", "success"));
  state.push(row.enabled ? badge("enabled", "neutral") : badge("disabled", "warning"));

  const actions: Node[] = [
    btn("Translations", {
      icon: "book",
      variant: "ghost",
      size: "sm",
      onclick: () => {
        swap(editing, editorFor(ctx, row));
        editing.scrollIntoView({ block: "nearest", behavior: "smooth" });
      },
    }),
  ];

  if (!row.is_default) {
    actions.push(
      btn(row.enabled ? "Disable" : "Enable", {
        variant: "ghost",
        size: "sm",
        onclick: (e) => {
          const button = e.currentTarget as HTMLButtonElement;
          void withBusy(ctx, button, async () => {
            try {
              await ctx.api.patch(`${ENDPOINT}/${encodeURIComponent(row.code)}`, { enabled: !row.enabled });
              ctx.toast(row.enabled ? `${row.name} disabled` : `${row.name} enabled`, { tone: "success" });
              await reload();
            } catch (err) {
              ctx.toast("That change was not saved", { tone: "danger", detail: describe(err) });
            }
          });
        },
      }),
      btn("Make default", {
        variant: "ghost",
        size: "sm",
        onclick: (e) => {
          const button = e.currentTarget as HTMLButtonElement;
          void withBusy(ctx, button, async () => {
            try {
              await ctx.api.post(`${ENDPOINT}/${encodeURIComponent(row.code)}/default`, {});
              ctx.toast(`${row.name} is now the default`, { tone: "success" });
              await reload();
            } catch (err) {
              ctx.toast("That change was not saved", { tone: "danger", detail: describe(err) });
            }
          });
        },
      }),
      btn("Remove", {
        variant: "ghost",
        size: "sm",
        onclick: () => {
          void ctx
            .confirm({
              title: `Remove ${row.name}?`,
              body: "Every translation in this locale is deleted and the language stops being offered. The keys themselves are not touched — they stay, and this locale simply no longer has its own words for them. This cannot be undone.",
              confirmLabel: "Remove",
              danger: true,
              phrase: row.code,
            })
            .then(async (ok) => {
              if (!ok) return;
              try {
                await ctx.api.del(`${ENDPOINT}/${encodeURIComponent(row.code)}`);
                ctx.toast(`${row.name} removed`, { tone: "success" });
                await reload();
              } catch (err) {
                ctx.toast("That locale was not removed", { tone: "danger", detail: describe(err) });
              }
            });
        },
      }),
    );
  }

  return h(
    "tr",
    null,
    h(
      "td",
      null,
      h("div", { class: "stack-xs" }, h("strong", null, row.native_name || row.name), row.native_name && row.native_name !== row.name ? h("span", { class: "field-hint" }, row.name) : null),
    ),
    h("td", null, h("code", null, row.code)),
    h(
      "td",
      null,
      h(
        "div",
        { class: "stack-xs" },
        // A bar, because "37 of 240" is arithmetic an operator should not have to do. The number
        // stays beside it for anyone who wants the exact figure.
        h(
          "div",
          { class: "meter", title: `${row.translated} of ${row.total_keys} keys` },
          h("div", { class: "meter-fill", style: `width:${Math.min(100, Math.max(0, row.coverage))}%` }),
        ),
        h("span", { class: "field-hint" }, `${row.coverage}% — ${row.translated} of ${row.total_keys}`),
      ),
    ),
    h("td", null, h("span", { class: "chip-row" }, ...state)),
    h("td", null, h("div", { class: "inline-row" }, ...actions)),
  );
}

// ----------------------------------------------------------------- editor --

function editorFor(ctx: Ctx, locale: LocaleRow): Node {
  const results = h("div", { class: "stack-sm" });
  const searchInput = input({
    type: "search",
    id: "i18n-search",
    placeholder: "Search keys, e.g. invoice or hero",
    autocomplete: "off",
  }) as HTMLInputElement;

  const search = async (query: string, onlyMissing: boolean): Promise<void> => {
    swap(results, h("p", { class: "field-hint" }, "Searching…"));
    try {
      const params = new URLSearchParams();
      if (query.trim()) params.set("q", query.trim());
      params.set("limit", "100");
      const res = await ctx.api.get<{ data: CatalogKey[] }>(`${ENDPOINT}/keys?${params.toString()}`, {
        cache: false,
      });
      const keys = (res.data ?? []).filter((k) => !onlyMissing || (k.missing ?? []).includes(locale.code));
      swap(results, renderKeys(ctx, locale, keys, onlyMissing, () => search(query, onlyMissing)));
    } catch (err) {
      swap(results, banner("danger", "The catalogue could not be searched", h("p", null, describe(err))));
    }
  };

  let missingOnly = false;
  const missingToggle = btn("Show only untranslated", {
    variant: "ghost",
    size: "sm",
    icon: "filter",
    onclick: (e) => {
      missingOnly = !missingOnly;
      (e.currentTarget as HTMLButtonElement).classList.toggle("is-active", missingOnly);
      void search(searchInput.value, missingOnly);
    },
  });

  searchInput.addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      event.preventDefault();
      void search(searchInput.value, missingOnly);
    }
  });

  void search("", false);

  return card({
    title: `Translations — ${locale.native_name || locale.name}`,
    sub: locale.is_default
      ? "This is the default locale: its text is what every other locale falls back to, and it cannot be left blank."
      : `Keys with no ${locale.name} text fall back to the default locale. The grey text beside each field is what the customer sees today.`,
    body: h(
      "div",
      { class: "stack" },
      h(
        "div",
        { class: "inline-row" },
        field({ label: "Search", control: searchInput, id: "i18n-search" }),
        missingToggle,
      ),
      results,
    ),
  });
}

function renderKeys(
  ctx: Ctx,
  locale: LocaleRow,
  keys: CatalogKey[],
  missingOnly: boolean,
  refresh: () => Promise<void>,
): Node {
  if (keys.length === 0) {
    return emptyState({
      title: missingOnly ? "Nothing left to translate" : "No keys matched",
      body: missingOnly
        ? `Every key the search found already has ${locale.name} text.`
        : "Try a shorter search, or clear it to list the catalogue.",
      icon: "checkCircle",
    });
  }

  return h(
    "div",
    { class: "stack-sm" },
    ...keys.map((entry) => {
      const current = entry.translations?.[locale.code] ?? "";
      const field0 = input({
        type: "text",
        id: `t-${entry.key}`,
        value: current,
        spellcheck: "false",
        placeholder: locale.is_default ? "Not set" : entry.default,
      }) as HTMLInputElement;

      const save = btn("Save", {
        variant: "primary",
        size: "sm",
        onclick: (e) => {
          const button = e.currentTarget as HTMLButtonElement;
          void withBusy(ctx, button, async () => {
            try {
              await ctx.api.put(`${ENDPOINT}/${encodeURIComponent(locale.code)}/keys/${encodeURIComponent(entry.key)}`, {
                text: field0.value,
              });
              ctx.toast("Translation saved", { tone: "success" });
              await refresh();
            } catch (err) {
              ctx.toast("That translation was not saved", { tone: "danger", detail: describe(err) });
            }
          });
        },
      });

      const actions: Node[] = [save];
      // Only a non-default locale can have a translation removed: the default's own text is what
      // everything falls back to, and the server refuses with `default_locale`.
      if (!locale.is_default && current !== "") {
        actions.push(
          btn("Clear", {
            variant: "ghost",
            size: "sm",
            onclick: (e) => {
              const button = e.currentTarget as HTMLButtonElement;
              void withBusy(ctx, button, async () => {
                try {
                  await ctx.api.del(
                    `${ENDPOINT}/${encodeURIComponent(locale.code)}/keys/${encodeURIComponent(entry.key)}`,
                  );
                  ctx.toast("Cleared — this key now falls back", { tone: "success" });
                  await refresh();
                } catch (err) {
                  ctx.toast("That translation was not cleared", { tone: "danger", detail: describe(err) });
                }
              });
            },
          }),
        );
      }

      return h(
        "div",
        { class: "stack-xs" },
        h(
          "div",
          { class: "inline-row" },
          h("code", null, entry.key),
          (entry.missing ?? []).includes(locale.code) && !locale.is_default
            ? badge("untranslated", "warning")
            : null,
        ),
        // The fallback text, shown beside the field rather than behind a tooltip: it is what the
        // customer sees right now, and an operator translating needs to read it while writing.
        !locale.is_default && entry.default
          ? h("p", { class: "field-hint" }, "Falls back to: ", h("em", null, entry.default))
          : null,
        h("div", { class: "inline-row" }, field0, ...actions),
      );
    }),
  );
}

// ------------------------------------------------------------------- add --

function openAddLocale(ctx: Ctx, reload: () => Promise<void>): void {
  const code = input({ type: "text", id: "new-locale-code", placeholder: "de, pt-BR, zh-Hans", spellcheck: "false" }) as HTMLInputElement;
  const name = input({ type: "text", id: "new-locale-name", placeholder: "German" }) as HTMLInputElement;
  const native = input({ type: "text", id: "new-locale-native", placeholder: "Deutsch" }) as HTMLInputElement;
  const enabled = h("input", { type: "checkbox", id: "new-locale-enabled", checked: true }) as HTMLInputElement;

  const body = h(
    "div",
    { class: "stack-sm" },
    field({
      label: "Language code",
      control: code,
      id: "new-locale-code",
      hint: "A BCP-47 tag: the language, then an optional region. Lower-case language, upper-case region.",
    }),
    field({ label: "English name", control: name, id: "new-locale-name" }),
    field({
      label: "Native name",
      control: native,
      id: "new-locale-native",
      hint: "What the language switcher shows, in the language itself.",
    }),
    h("label", { class: "switch-row" }, enabled, h("span", null, "Offer this language immediately")),
    h(
      "p",
      { class: "field-hint" },
      "A new locale starts with no translations, so every key falls back to the default until it is translated. Enabling it before it is translated is allowed — the shop will simply read in the default language.",
    ),
  );

  const cancel = btn("Cancel", { variant: "ghost" });
  const submit = btn("Add locale", { variant: "primary" });

  const handle = openDialog({
    title: "Add a locale",
    body,
    footer: h("div", { class: "row spacer" }, cancel, submit),
    locked: () => submit instanceof HTMLButtonElement && submit.disabled,
  });
  cancel.addEventListener("click", () => handle.close());

  const add = async (): Promise<void> => {
    await withBusy(ctx, submit, async () => {
      try {
        await ctx.api.post(ENDPOINT, {
          code: code.value.trim(),
          name: name.value.trim(),
          native_name: native.value.trim(),
          enabled: enabled.checked,
        });
        ctx.toast("Locale added", { tone: "success" });
        handle.close();
        await reload();
      } catch (err) {
        // The dialog stays open on a refusal: the operator's fix is to correct a field, and
        // closing would make them start again.
        ctx.toast("That locale was not added", { tone: "danger", detail: describe(err) });
      }
    });
  };
  submit.addEventListener("click", () => void add());
  code.addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      event.preventDefault();
      void add();
    }
  });
}

// ---------------------------------------------------------------- failure --

function describe(err: unknown): string {
  if (err instanceof ApiError) {
    switch (err.code) {
      case "default_locale":
        return err.message;
      case "duplicate_locale":
        return "That language code already exists.";
      case "invalid_input":
        return err.message;
      case "not_found":
        return "That locale or key no longer exists. Reload the page.";
      default:
        return err.message || "The operation could not be completed.";
    }
  }
  return err instanceof Error ? err.message : "The operation could not be completed.";
}
