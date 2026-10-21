// DOM construction.
//
// There is no virtual DOM and no template compiler. `h` builds a real element and returns
// it; a page composes elements and the app mounts them. That is enough for a panel whose
// largest screen is a table, and it keeps the bundle at a size where the first paint is
// bounded by the network rather than by parsing a framework.
//
// The security-relevant rule: a string child becomes a text node, never markup. Customer
// names, invoice line descriptions and provider error strings all reach this function, and
// an operator panel is an authenticated origin — the CSP would stop an injected script, but
// rendering attacker-controlled text as HTML is still how a panel leaks data. `raw` exists
// for exactly one caller, `icons.ts`, whose input is a closed union of literals.

export type Child = Node | string | number | null | undefined | false;

type EventHandler = (event: Event) => void;

export interface Props {
  class?: string;
  id?: string;
  /** Trusted markup. Only `icons.ts` may use this; it is typed to a closed union. */
  html?: string;
  dataset?: Record<string, string>;
  /** Property, not attribute: an input's value must be the live value. */
  value?: string | number;
  checked?: boolean;
  disabled?: boolean;
  title?: string;
  role?: string;
  tabindex?: number;
  [key: string]: unknown;
}

/** A string that is already known-safe markup. */
const TRUSTED = Symbol("trusted-html");

export interface TrustedHtml {
  readonly [TRUSTED]: true;
  readonly html: string;
}

export function raw(html: string): TrustedHtml {
  return { [TRUSTED]: true, html };
}

function append(parent: Node, child: Child): void {
  if (child === null || child === undefined || child === false) return;
  if (typeof child === "string" || typeof child === "number") {
    parent.appendChild(document.createTextNode(String(child)));
    return;
  }
  parent.appendChild(child);
}

export function h<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  props?: Props | null,
  ...children: Child[]
): HTMLElementTagNameMap[K] {
  const el = document.createElement(tag);
  applyProps(el, props);
  for (const child of children) append(el, child);
  return el;
}

function applyProps(el: Element, props?: Props | null): void {
  if (!props) return;
  for (const [key, value] of Object.entries(props)) {
    if (value === undefined || value === null || value === false) continue;

    if (key === "class") {
      el.setAttribute("class", String(value));
      continue;
    }
    if (key === "dataset") {
      for (const [k, v] of Object.entries(value as Record<string, string>)) {
        (el as HTMLElement).dataset[k] = v;
      }
      continue;
    }
    if (key === "html") {
      const trusted = value as TrustedHtml;
      if (!trusted || trusted[TRUSTED] !== true) {
        throw new Error("dom: html prop requires a TrustedHtml value from raw()");
      }
      el.innerHTML = trusted.html;
      continue;
    }
    if (key === "value") {
      (el as HTMLInputElement).value = String(value);
      continue;
    }
    if (key === "checked") {
      (el as HTMLInputElement).checked = Boolean(value);
      continue;
    }
    if (key.startsWith("on") && typeof value === "function") {
      el.addEventListener(key.slice(2).toLowerCase(), value as EventHandler);
      continue;
    }
    if (value === true) {
      el.setAttribute(key, "");
      continue;
    }
    el.setAttribute(key, String(value));
  }
}

/** Appends children to an existing element. */
export function fill(parent: Element, ...children: Child[]): Element {
  parent.replaceChildren();
  for (const child of children) append(parent, child);
  return parent;
}

/** Replaces an element's content with a single node. */
export function swap(parent: Element, node: Node): void {
  parent.replaceChildren(node);
}

export function frag(...children: Child[]): DocumentFragment {
  const f = document.createDocumentFragment();
  for (const child of children) append(f, child);
  return f;
}

export function text(value: string | number): Text {
  return document.createTextNode(String(value));
}

/** Adds a listener and returns its removal, so a page can clean up on navigation. */
export function on<K extends keyof HTMLElementEventMap>(
  target: EventTarget,
  type: K | string,
  handler: (event: HTMLElementEventMap[K]) => void,
  options?: AddEventListenerOptions,
): () => void {
  target.addEventListener(type, handler as EventListener, options);
  return () => target.removeEventListener(type, handler as EventListener, options);
}

export function qs<T extends Element = HTMLElement>(sel: string, root: ParentNode = document): T | null {
  return root.querySelector<T>(sel);
}

export function qsa<T extends Element = HTMLElement>(sel: string, root: ParentNode = document): T[] {
  return Array.from(root.querySelectorAll<T>(sel));
}

/** Debounces a function, keeping the last arguments. */
export function debounce<A extends unknown[]>(fn: (...args: A) => void, ms: number): (...args: A) => void {
  let timer: number | undefined;
  return (...args: A) => {
    if (timer !== undefined) window.clearTimeout(timer);
    timer = window.setTimeout(() => fn(...args), ms);
  };
}

/** Runs `fn` once the element leaves the document (used to abort fetches). */
export function onDetach(el: Node, fn: () => void): void {
  const observer = new MutationObserver(() => {
    if (!el.isConnected) {
      observer.disconnect();
      fn();
    }
  });
  observer.observe(document.body, { childList: true, subtree: true });
}

export function prefersReducedMotion(): boolean {
  return window.matchMedia("(prefers-reduced-motion: reduce)").matches;
}
