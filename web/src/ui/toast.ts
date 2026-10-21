// Toasts.
//
// One stack, bottom right, aria-live polite. A toast is a receipt for something that already
// happened — it never asks a question and it never carries the only copy of an important
// value. A key that is shown once is shown in a dialog, not in a toast that fades.

import { h } from "../lib/dom";
import { icon } from "../lib/icons";
import type { ToastOptions } from "../lib/types";

export interface Toaster {
  push(title: string, options?: ToastOptions): void;
  el: HTMLElement;
}

export function createToaster(): Toaster {
  const el = h("div", { class: "toasts", role: "status", "aria-live": "polite", "aria-atomic": "false" });
  document.body.appendChild(el);

  const push = (title: string, options: ToastOptions = {}) => {
    const tone = options.tone ?? "info";
    const iconName = tone === "success" ? "checkCircle" : tone === "danger" ? "alertCircle" : "info";
    const toast = h(
      "div",
      { class: `toast toast--${tone}`, role: tone === "danger" ? "alert" : "status" },
      h("span", { class: "toast-icon" }, icon(iconName, 15)),
      h(
        "div",
        { class: "toast-body" },
        h("div", { class: "toast-title" }, title),
        options.detail ? h("div", { class: "toast-detail" }, options.detail) : null,
      ),
      h(
        "button",
        {
          class: "toast-close",
          type: "button",
          "aria-label": "Dismiss",
          onclick: () => dismiss(toast),
        },
        icon("x", 11),
      ),
    );
    el.appendChild(toast);

    // Never more than four: a burst of failures should not cover the screen.
    while (el.children.length > 4) el.firstElementChild?.remove();

    const duration = options.duration ?? (tone === "danger" ? 8000 : 4500);
    if (duration > 0) window.setTimeout(() => dismiss(toast), duration);
  };

  const dismiss = (toast: HTMLElement) => {
    if (!toast.isConnected) return;
    toast.classList.add("is-leaving");
    window.setTimeout(() => toast.remove(), 200);
  };

  return { push, el };
}
