// Rich text: the client half of the description sanitiser.
//
// The server sanitises a product description on write, so what comes back from the API should
// already be safe. This file exists anyway, and deliberately, because "should already be safe"
// is not a security property:
//
//   - A stored value can predate the allowlist (an operator's database restored from an older
//     backup, a row written by a script that never went through the API).
//   - The server and the browser are two different parsers. A string that one reads as text and
//     the other reads as markup is the classic mutation-XSS shape, and re-parsing here through a
//     *rebuilding* sanitiser (not a regex, not a blocklist) removes that class entirely.
//   - The panel's CSP (`script-src 'self'`, no `unsafe-inline`) would stop an injected script
//     running, but it would not stop an injected `<img src="/api/v1/…">` from making a visitor's
//     browser request a URL, and it would not stop the page from rendering nonsense.
//
// The allowlist is the same closed set as internal/catalog/sanitize.go. Keeping them separate is
// the point: two independent implementations that agree are a defence, one implementation used
// twice is a single point of failure.

import { h, raw, type TrustedHtml } from "./dom";

const ALLOWED = new Set([
  "p", "br", "hr", "div", "span",
  "strong", "b", "em", "i", "u", "s",
  "ul", "ol", "li",
  "h2", "h3", "h4",
  "blockquote", "code", "pre",
  "a", "img",
]);

const VOID = new Set(["br", "hr", "img"]);

// Elements whose *content* goes too: a `<script>` body is not text a visitor should read.
const DROP_WITH_CONTENT = new Set([
  "script", "style", "title", "textarea", "iframe", "object", "noscript", "template", "svg", "math",
]);

/** Sanitises markup into the allowlist, rebuilding it rather than filtering it. */
export function sanitizeRichText(markup: string): string {
  // DOMParser builds an inert document: no scripts run and no resources load while it parses.
  const parsed = new DOMParser().parseFromString(markup, "text/html");
  const out = document.createElement("div");
  for (const node of Array.from(parsed.body.childNodes)) copyNode(node, out);
  return out.innerHTML;
}

function copyNode(node: Node, parent: Element): void {
  if (node.nodeType === Node.TEXT_NODE) {
    parent.appendChild(document.createTextNode(node.nodeValue ?? ""));
    return;
  }
  if (node.nodeType !== Node.ELEMENT_NODE) {
    // Comments, processing instructions and doctypes carry nothing a description needs, and a
    // conditional comment is the classic way to smuggle markup past a filter.
    return;
  }
  const el = node as Element;
  const name = el.tagName.toLowerCase();
  if (DROP_WITH_CONTENT.has(name)) return;
  if (!ALLOWED.has(name)) {
    // An element we do not recognise is unwrapped, not deleted: `<section><p>x</p></section>`
    // loses the section and keeps the paragraph. The exception above is the elements whose
    // content is dangerous rather than merely unknown.
    for (const child of Array.from(el.childNodes)) copyNode(child, parent);
    return;
  }

  const clone = document.createElement(name);
  if (name === "a") {
    const href = safeURL(el.getAttribute("href"), false);
    if (href) {
      clone.setAttribute("href", href);
      clone.setAttribute("rel", "noopener noreferrer nofollow");
    }
    const title = el.getAttribute("title");
    if (title) clone.setAttribute("title", title);
  } else if (name === "img") {
    const src = safeURL(el.getAttribute("src"), true);
    if (!src) return; // no safe source means no image at all
    clone.setAttribute("src", src);
    clone.setAttribute("alt", el.getAttribute("alt") ?? "");
    clone.setAttribute("loading", "lazy");
  }
  // Every other attribute — style, class, id, event handlers — is dropped by omission. An
  // allowlist that enumerates what to remove is a list that is always one attribute behind.
  for (const child of Array.from(el.childNodes)) copyNode(child, clone);
  if (!VOID.has(name) || clone.hasChildNodes()) parent.appendChild(clone);
}

/** Returns the URL if a browser would treat it as a plain navigation (or a same-origin image). */
function safeURL(value: string | null, image: boolean): string | null {
  if (!value) return null;
  // Control characters and whitespace are ignored in the scheme position, so `java\tscript:` is
  // the same string to a browser as `javascript:` and has to be the same string to us.
  const cleaned = value.replace(/[\u0000-\u0020\u007f]/g, "").trim();
  if (!cleaned) return null;
  const lower = cleaned.toLowerCase();
  if (image) {
    if (!cleaned.startsWith("/") || cleaned.startsWith("//") || cleaned.includes("\\") || cleaned.includes(":")) {
      return null;
    }
    return cleaned;
  }
  if (lower.startsWith("https://") || lower.startsWith("http://") || lower.startsWith("mailto:")) return cleaned;
  if (cleaned.startsWith("//")) return null;
  if (cleaned.startsWith("/") || cleaned.startsWith("#")) return cleaned;
  return null;
}

/** Markup that is safe to place in the DOM, for a description stored with format `html`. */
export function richText(markup: string): TrustedHtml {
  return raw(sanitizeRichText(markup));
}

/**
 * A description as it should be shown: parsed when the stored format says it is markup, and
 * rendered as text when it is plain. Saying which is the whole reason the format is stored.
 */
export function descriptionBody(text: string, format: string): Node {
  if (!text) return h("span", { class: "faint" }, "No description.");
  if (format === "html") {
    // `innerHTML` is set from a TrustedHtml value this file sanitised one line earlier. The `html`
    // prop of `h` would be the tidier spelling, but it is typed for the icon module's closed union
    // of literals; going through `raw()` here keeps the trust boundary explicit.
    const body = h("div", { class: "store-prose" });
    body.innerHTML = richText(text).html;
    return body;
  }
  return h("div", { class: "store-prose" }, ...text.split(/\n{2,}/).map((para) => h("p", null, para)));
}

export interface RichTextEditor {
  /** The element to place in the page. */
  el: HTMLElement;
  /** The current value, sanitised. */
  value(): string;
}

/**
 * A small contenteditable editor with the formatting a product description needs.
 *
 * It is a plain editor rather than a toolbar of exotic features on purpose: every command it
 * offers produces markup the server's allowlist keeps, so what an operator sees while typing is
 * what a visitor gets. The value is sanitised on read as well as on write, so a paste from
 * another page cannot introduce anything the allowlist refuses.
 */
export function richTextEditor(opts: { value: string; onChange?: () => void; id?: string }): RichTextEditor {
  const area = h("div", {
    class: "input store-prose",
    id: opts.id,
    contenteditable: "true",
    role: "textbox",
    "aria-multiline": "true",
  }) as HTMLElement;
  // The stored value is sanitised before it is ever parsed into the live document.
  area.innerHTML = sanitizeRichText(opts.value);
  area.addEventListener("input", () => opts.onChange?.());

  const command = (name: string, argument?: string) => () => {
    area.focus();
    document.execCommand(name, false, argument);
    opts.onChange?.();
  };

  const tool = (label: string, title: string, name: string, argument?: string): HTMLElement =>
    h(
      "button",
      {
        class: "btn btn--ghost btn--sm",
        type: "button",
        title,
        "aria-label": title,
        onmousedown: (event: Event) => event.preventDefault(), // keep the selection
        onclick: command(name, argument),
      },
      label,
    );

  const toolbar = h(
    "div",
    { class: "row", role: "toolbar", "aria-label": "Formatting" },
    tool("B", "Bold", "bold"),
    tool("I", "Italic", "italic"),
    tool("H2", "Heading", "formatBlock", "h2"),
    tool("H3", "Subheading", "formatBlock", "h3"),
    tool("¶", "Paragraph", "formatBlock", "p"),
    tool("• List", "Bulleted list", "insertUnorderedList"),
    tool("1. List", "Numbered list", "insertOrderedList"),
    tool("Quote", "Quote", "formatBlock", "blockquote"),
    tool("Link", "Insert a link", "createLink", "https://"),
    tool("Clear", "Remove formatting", "removeFormat"),
  );

  return {
    el: h("div", { class: "stack-sm" }, toolbar, area),
    value: () => sanitizeRichText(area.innerHTML),
  };
}
