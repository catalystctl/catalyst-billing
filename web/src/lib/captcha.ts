// The human-challenge widget (IDN-18).
//
// A registration form and a password-reset form are the two places an anonymous visitor can
// make the panel spend money — an account and a verification mail, or a reset mail to any
// address — so both ask for a challenge when the operator has configured one.
//
// # Nothing here runs unless a provider is configured
//
// Importing this module does nothing: no script tag, no request, no global. The first call to
// `mountCaptcha` is what reads `/api/v1/auth/captcha`, and only the screens that need a widget
// call it. When the answer is `provider: "none"` — the default — `mountCaptcha` resolves to
// `null` and the page submits without a token, which is exactly what it did before this file
// existed.
//
// # The widget is loaded from the provider's origin, on demand
//
// All three providers are a script from their own origin that renders a cross-origin iframe.
// The panel's CSP is `script-src 'self'` and `default-src 'self'`, so those origins have to be
// allowed before any of this can work. The exact set is in the CSP report for this feature;
// the short version is that `script-src` needs the provider's script host and `frame-src` (or
// `child-src`) needs the host of the iframe it renders. Nothing else about the panel changes:
// there is still no inline script, and the token is posted to our own API, not to the provider.
//
// # The token is opaque and short-lived
//
// A token is single use and expires in a few minutes. `reset()` throws it away and asks for a
// fresh one, which is what a page does after the server refuses a submission with
// `captcha_failed` — otherwise the visitor retries with a token the provider has already
// consumed and fails again for a reason that is not their fault.

import type { Api } from "./api";

/** The providers the server can be configured with. `none` means no widget. */
export type CaptchaProvider = "none" | "turnstile" | "recaptcha" | "hcaptcha";

/** The public half of the server's captcha configuration. It never contains the secret key. */
export interface CaptchaConfig {
  provider: CaptchaProvider;
  /** The provider's public key. Empty when unconfigured. */
  site_key: string;
  /** Whether a widget can be rendered at all: a provider *and* a site key. */
  enabled: boolean;
}

/** A rendered challenge. */
export interface CaptchaWidget {
  readonly provider: Exclude<CaptchaProvider, "none">;
  /**
   * The token for the challenge as it stands, or null when the visitor has not solved it
   * yet (or it has expired). It is deliberately synchronous: a form that awaited a challenge
   * nobody is going to solve would hang on submit.
   */
  token(): string | null;
  /** Discards the current token and asks for a new challenge. */
  reset(): void;
  /** Removes the widget. */
  destroy(): void;
}

// ------------------------------------------------------------------- provider APIs --

// The globals each provider's script defines. They are declared as the narrow surface this
// file uses rather than as the providers' full SDKs: a type that describes more than we call
// is a type that can lie about what we need.

interface TurnstileApi {
  render(
    container: HTMLElement,
    options: {
      sitekey: string;
      theme?: "auto" | "light" | "dark";
      callback: (token: string) => void;
      "expired-callback"?: () => void;
      "error-callback"?: () => void;
    },
  ): string;
  reset(widgetId?: string): void;
  remove(widgetId: string): void;
}

interface GrecaptchaApi {
  render(
    container: HTMLElement,
    options: {
      sitekey: string;
      theme?: "light" | "dark";
      callback: (token: string) => void;
      "expired-callback"?: () => void;
      "error-callback"?: () => void;
    },
  ): number;
  reset(widgetId?: number): void;
  getResponse(widgetId?: number): string;
}

interface HCaptchaApi {
  render(
    container: HTMLElement,
    options: {
      sitekey: string;
      theme?: "light" | "dark";
      callback: (token: string) => void;
      "expired-callback"?: () => void;
      "error-callback"?: () => void;
    },
  ): string;
  reset(widgetId?: string): void;
  remove(widgetId: string): void;
  getResponse(widgetId?: string): string;
}

declare global {
  interface Window {
    turnstile?: TurnstileApi;
    grecaptcha?: GrecaptchaApi;
    hcaptcha?: HCaptchaApi;
  }
}

// The script URLs. `render=explicit` is what stops each script from rendering into every
// element it finds on load: this module decides where the widget goes.
const SCRIPTS: Record<Exclude<CaptchaProvider, "none">, string> = {
  turnstile: "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit",
  recaptcha: "https://www.google.com/recaptcha/api.js?render=explicit",
  hcaptcha: "https://js.hcaptcha.com/1/api.js?render=explicit",
};

// ------------------------------------------------------------------------- config --

// The configuration is fetched once per page load and shared by every screen that asks. A
// failed fetch is treated as "no provider": the page then submits without a token, and the
// server — which reads the same setting from the database — refuses with captcha_unavailable
// if a challenge really was required. Failing closed in the browser would show a widget that
// cannot work.
let configRequest: Promise<CaptchaConfig> | null = null;

/** Reads the challenge configuration. Cached for the life of the page. */
export function loadCaptchaConfig(api: Api): Promise<CaptchaConfig> {
  if (!configRequest) {
    configRequest = api
      .get<CaptchaConfig>("/api/v1/auth/captcha")
      .then((config) => normalise(config))
      .catch(() => ({ provider: "none", site_key: "", enabled: false }) as CaptchaConfig);
  }
  return configRequest;
}

/** Clears the cached configuration, so the next screen re-reads it. */
export function resetCaptchaConfig(): void {
  configRequest = null;
}

function isProvider(value: unknown): value is CaptchaProvider {
  return value === "none" || value === "turnstile" || value === "recaptcha" || value === "hcaptcha";
}

// normalise refuses to trust the response shape: a provider this file does not implement is
// treated as no provider, not rendered as an empty widget.
function normalise(config: CaptchaConfig): CaptchaConfig {
  const provider = isProvider(config.provider) ? config.provider : "none";
  const siteKey = typeof config.site_key === "string" ? config.site_key : "";
  return {
    provider,
    site_key: siteKey,
    enabled: config.enabled === true && provider !== "none" && siteKey !== "",
  };
}

// ------------------------------------------------------------------ script loading --

// One promise per script URL: a second screen on the same page load reuses the script instead
// of injecting it twice.
const scripts = new Map<string, Promise<void>>();

function loadScript(src: string): Promise<void> {
  const existing = scripts.get(src);
  if (existing) return existing;
  const request = new Promise<void>((resolve, reject) => {
    const script = document.createElement("script");
    script.src = src;
    script.async = true;
    script.defer = true;
    script.addEventListener("load", () => resolve(), { once: true });
    script.addEventListener("error", () => reject(new Error(`could not load ${src}`)), {
      once: true,
    });
    document.head.appendChild(script);
  });
  scripts.set(src, request);
  return request;
}

// waitFor polls for the global a script defines. The load event fires when the script has
// executed, but the providers' scripts fetch a second bundle before they define their API, so
// the global is polled rather than assumed.
async function waitFor<T>(read: () => T | undefined, what: string, timeoutMs = 5000): Promise<T> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const value = read();
    if (value) return value;
    if (Date.now() >= deadline) throw new Error(`${what} did not load`);
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
}

// -------------------------------------------------------------------------- widgets --

type Mounter = (host: HTMLElement, siteKey: string) => Promise<CaptchaWidget>;

const mounters: Record<Exclude<CaptchaProvider, "none">, Mounter> = {
  // Turnstile and hCaptcha are the same shape: render returns a widget id, the token arrives
  // through the callback, and reset/remove take the id.
  turnstile: async (host, siteKey) => {
    await loadScript(SCRIPTS.turnstile);
    const api = await waitFor(() => window.turnstile, "Turnstile");
    let token: string | null = null;
    const id = api.render(host, {
      sitekey: siteKey,
      theme: "auto",
      callback: (value) => {
        token = value;
      },
      "expired-callback": () => {
        token = null;
      },
      "error-callback": () => {
        token = null;
      },
    });
    return {
      provider: "turnstile",
      token: () => token,
      reset: () => {
        token = null;
        api.reset(id);
      },
      destroy: () => {
        token = null;
        api.remove(id);
      },
    };
  },

  hcaptcha: async (host, siteKey) => {
    await loadScript(SCRIPTS.hcaptcha);
    const api = await waitFor(() => window.hcaptcha, "hCaptcha");
    let token: string | null = null;
    const id = api.render(host, {
      sitekey: siteKey,
      theme: "light",
      callback: (value) => {
        token = value;
      },
      "expired-callback": () => {
        token = null;
      },
      "error-callback": () => {
        token = null;
      },
    });
    return {
      provider: "hcaptcha",
      token: () => token ?? (api.getResponse(id) || null),
      reset: () => {
        token = null;
        api.reset(id);
      },
      destroy: () => {
        token = null;
        api.remove(id);
      },
    };
  },

  // reCAPTCHA keeps the token itself: `getResponse` is the authoritative read, and the
  // callback only exists so the page can react when the visitor solves it.
  recaptcha: async (host, siteKey) => {
    await loadScript(SCRIPTS.recaptcha);
    const api = await waitFor(() => window.grecaptcha, "reCAPTCHA");
    const id = api.render(host, {
      sitekey: siteKey,
      theme: "light",
      callback: () => {},
      "expired-callback": () => {},
      "error-callback": () => {},
    });
    return {
      provider: "recaptcha",
      token: () => api.getResponse(id) || null,
      reset: () => api.reset(id),
      destroy: () => api.reset(id),
    };
  },
};

/**
 * Renders the configured challenge into `host`.
 *
 * Resolves to null when no provider is configured — the page then submits without a token and
 * the server accepts, because there is nothing to check. It rejects when a provider *is*
 * configured but its script could not be loaded: that is a broken deployment (or a CSP that
 * has not been updated for the provider's origin), and a page that silently submitted anyway
 * would produce a `captcha_required` refusal the visitor cannot understand.
 */
export async function mountCaptcha(api: Api, host: HTMLElement): Promise<CaptchaWidget | null> {
  const config = await loadCaptchaConfig(api);
  if (!config.enabled || config.provider === "none") return null;
  const mount = mounters[config.provider];
  if (!mount) return null;
  return mount(host, config.site_key);
}
