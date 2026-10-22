// Dialogs.
//
// A modal is where a destructive action asks for confirmation and where a secret that is
// shown once is displayed. Both are cases where the user must be unable to miss the
// consequence, so the dialog owns the screen, traps focus, closes on Escape, and returns
// focus to the element that opened it.
//
// The confirm dialog can require a phrase. "Type the service's external id" is the only
// thing that stands between a mis-click and a terminated customer, so it is a first-class
// option rather than something a page re-implements.

import { h } from "../lib/dom";
import { icon } from "../lib/icons";
import type { ConfirmOptions, FormDialogOptions } from "../lib/types";
import { btn, input } from "./kit";

export interface DialogHandle {
  el: HTMLElement;
  close(): void;
}

interface OpenOptions {
  title: string;
  subtitle?: string;
  body: Node;
  footer?: Node;
  wide?: boolean;
  onClose?: () => void;
  /** Prevents Escape/backdrop dismissal while an action is in flight. */
  locked?: () => boolean;
}

let openCount = 0;

export function openDialog(opts: OpenOptions): DialogHandle {
  const previous = document.activeElement as HTMLElement | null;
  const titleId = `dialog-title-${Math.random().toString(36).slice(2, 8)}`;

  const closeBtn = h(
    "button",
    { class: "dialog-close", type: "button", "aria-label": "Close dialog" },
    icon("x", 14),
  );

  const dialog = h(
    "div",
    {
      class: `dialog${opts.wide ? " dialog--wide" : ""}`,
      role: "dialog",
      "aria-modal": "true",
      "aria-labelledby": titleId,
    },
    h(
      "header",
      { class: "dialog-head" },
      h(
        "div",
        { class: "dialog-head-text" },
        h("h2", { class: "dialog-title", id: titleId }, opts.title),
        opts.subtitle ? h("p", { class: "dialog-sub" }, opts.subtitle) : null,
      ),
      closeBtn,
    ),
    h("div", { class: "dialog-body" }, opts.body),
    opts.footer ? h("footer", { class: "dialog-foot" }, opts.footer) : null,
  );

  const overlay = h("div", { class: "overlay" }, dialog);

  const close = () => {
    overlay.remove();
    document.removeEventListener("keydown", onKey, true);
    document.body.style.overflow = "";
    openCount = Math.max(0, openCount - 1);
    previous?.focus?.();
    opts.onClose?.();
  };

  // `locked` guards the ways a *user* can dismiss the dialog while an action is in flight:
  // Escape and a backdrop click. It deliberately does not guard close() itself, because the
  // owner calls close() precisely when the action has finished — gating that on the same
  // predicate made every form dialog stay open after a successful submit, since the submit
  // button is still disabled at the moment close() is called.
  const dismissable = () => !opts.locked?.();

  const onKey = (event: KeyboardEvent) => {
    if (event.key === "Escape") {
      event.stopPropagation();
      if (dismissable()) close();
      return;
    }
    if (event.key !== "Tab") return;
    const focusables = Array.from(
      dialog.querySelectorAll<HTMLElement>(
        'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
      ),
    ).filter((el) => el.offsetParent !== null || el === document.activeElement);
    if (focusables.length === 0) return;
    const first = focusables[0];
    const last = focusables[focusables.length - 1];
    if (!first || !last) return;
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  };

  closeBtn.addEventListener("click", () => {
    if (dismissable()) close();
  });
  overlay.addEventListener("mousedown", (event) => {
    if (event.target === overlay && dismissable()) close();
  });
  document.addEventListener("keydown", onKey, true);
  document.body.style.overflow = "hidden";
  openCount += 1;
  document.body.appendChild(overlay);

  // Focus the first control, or the dialog itself when it has none.
  window.requestAnimationFrame(() => {
    const target = dialog.querySelector<HTMLElement>(
      "input:not([type=hidden]), select, textarea, button.btn--primary, button",
    );
    (target ?? dialog).focus();
  });

  return { el: overlay, close };
}

export function confirmDialog(opts: ConfirmOptions): Promise<boolean> {
  return new Promise((resolve) => {
    let settled = false;
    const finish = (value: boolean, close: () => void) => {
      if (settled) return;
      settled = true;
      resolve(value);
      close();
    };

    const phraseInput = opts.phrase
      ? input({ placeholder: opts.phrase, class: "input--mono", autocomplete: "off" })
      : null;
    const confirm = btn(opts.confirmLabel ?? "Confirm", {
      variant: opts.danger ? "danger-solid" : "primary",
      disabled: Boolean(opts.phrase),
    });
    const cancel = btn(opts.cancelLabel ?? "Cancel", { variant: "ghost" });

    let handle: DialogHandle;
    confirm.addEventListener("click", () => finish(true, () => handle.close()));
    cancel.addEventListener("click", () => finish(false, () => handle.close()));

    if (phraseInput) {
      phraseInput.addEventListener("input", () => {
        (confirm as HTMLButtonElement).disabled = phraseInput.value !== opts.phrase;
      });
    }

    const body = h(
      "div",
      { class: "stack-sm" },
      opts.body ? (typeof opts.body === "string" ? h("p", null, opts.body) : opts.body) : null,
      opts.phrase
        ? h(
            "div",
            { class: "stack-sm" },
            h("p", { class: "field-label" }, `Type “${opts.phrase}” to confirm`),
            phraseInput,
          )
        : null,
      opts.note ? h("div", { class: "banner banner--warning" }, icon("alertTriangle", 15), h("span", null, opts.note)) : null,
    );

    handle = openDialog({
      title: opts.title,
      body,
      wide: Boolean(opts.phrase),
      footer: h("div", { class: "row spacer" }, cancel, confirm),
      onClose: () => finish(false, () => {}),
    });
  });
}

/** Wires a dialog's footer submit button to a form that lives in its body.
 *
 *  A dialog's footer is a sibling of the body, not a child of the <form>, so a submit button
 *  placed there is associated with nothing and clicking it does nothing at all. The `form`
 *  attribute is the standard way to associate a control with a form elsewhere in the document.
 *  Every custom dialog in the panel needs this, and forgetting it produces a button that looks
 *  enabled, does not submit, and reports no error — which is why it is a helper rather than a
 *  line each page copies. */
export function bindForm(form: HTMLFormElement, submit: HTMLElement): void {
  if (!form.id) {
    form.id = `dialog-form-${Math.random().toString(36).slice(2, 9)}`;
  }
  submit.setAttribute("form", form.id);
}

export function formDialog(opts: FormDialogOptions): Promise<void> {
  return new Promise((resolve) => {
    const formId = `dialog-form-${Math.random().toString(36).slice(2, 9)}`;
    const submit = btn(opts.submitLabel ?? "Save", {
      variant: opts.danger ? "danger-solid" : "primary",
      type: "submit",
    });
    // The footer is a sibling of the body, not a child of the <form>, so a submit button
    // placed there is not associated with the form and clicking it does nothing at all.
    // The `form` attribute is the standard way to associate a control that lives outside
    // its form; without it every dialog's primary button was inert.
    submit.setAttribute("form", formId);
    const cancel = btn(opts.cancelLabel ?? "Cancel", { variant: "ghost" });
    const errorHost = h("div");

    const form = h("form", { class: "stack", novalidate: true, id: formId }, opts.body);
    const handle = openDialog({
      title: opts.title,
      subtitle: opts.subtitle,
      body: h("div", { class: "stack" }, form, errorHost),
      wide: true,
      footer: h("div", { class: "row spacer" }, cancel, submit),
      onClose: () => resolve(),
      locked: () => submit instanceof HTMLButtonElement && submit.disabled,
    });

    cancel.addEventListener("click", () => handle.close());

    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void (async () => {
        if (submit instanceof HTMLButtonElement) submit.disabled = true;
        errorHost.replaceChildren();
        try {
          const message = await opts.onSubmit();
          if (message) {
            errorHost.replaceChildren(
              h("div", { class: "banner banner--danger" }, icon("alertCircle", 15), h("span", null, message)),
            );
            return;
          }
          handle.close();
        } finally {
          if (submit instanceof HTMLButtonElement) submit.disabled = false;
        }
      })();
    });

    // A form dialog submits on Enter from a text field, which is what every operator
    // expects; the explicit button remains for discoverability.
    form.addEventListener("keydown", (event) => {
      if (event.key === "Enter" && (event.target as HTMLElement).tagName !== "TEXTAREA") {
        event.preventDefault();
        form.requestSubmit();
      }
    });
  });
}
