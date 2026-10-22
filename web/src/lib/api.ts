// The API client.
//
// Every request goes through one function, for four reasons that each cost money when they
// are missed:
//
//   - The CSRF token must travel with every mutation. A page that hand-rolled `fetch` for a
//     PUT would be a 403 in production and a mystery in review.
//   - Staff mutations require an `Idempotency-Key`. Sending one automatically means a
//     double-clicked refund is one refund, and a retried request after a dropped connection
//     replays the stored response instead of moving money twice.
//   - Errors are `application/problem+json` with a stable `code`. The client branches on the
//     code, never on the human `detail`, so a copy edit cannot break behaviour.
//   - A 401 means the session is gone, not that this one request failed. It is reported once,
//     through a callback, and the app returns to the sign-in screen.

export interface Problem {
  type?: string;
  title?: string;
  status?: number;
  detail?: string;
  code?: string;
  [key: string]: unknown;
}

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly detail: string;
  readonly problem: Problem;

  constructor(status: number, problem: Problem) {
    super(problem.detail || problem.title || `request failed (${status})`);
    this.name = "ApiError";
    this.status = status;
    this.code = problem.code || "unknown";
    this.detail = problem.detail || problem.title || "";
    this.problem = problem;
  }

  /** True when the caller should try again later rather than change the request. */
  get retryable(): boolean {
    return this.status === 429 || this.status === 502 || this.status === 503 || this.status === 504;
  }
}

export interface Page<T> {
  data: T[];
  next_cursor: string | null;
}

export type Query = Record<string, string | number | boolean | null | undefined>;

export interface RequestOptions {
  query?: Query;
  body?: unknown;
  signal?: AbortSignal;
  /** A caller-supplied key for a retry-safe mutation. Defaults to a fresh UUID. */
  idempotencyKey?: string;
  /** A mutation that must not send a key (login, password reset). */
  noIdempotency?: boolean;
  /** A mutation that must not send the CSRF header (public entry points). */
  noCSRF?: boolean;
  /** A body that is sent verbatim rather than JSON-encoded: a file upload. */
  raw?: BodyInit;
  /** The Content-Type of a raw body. */
  contentType?: string;
}

const CSRF_HEADER = "X-CSRF-Token";
const IDEMPOTENCY_HEADER = "Idempotency-Key";

function buildQuery(query?: Query): string {
  if (!query) return "";
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined || value === null || value === "") continue;
    params.set(key, String(value));
  }
  const s = params.toString();
  return s ? `?${s}` : "";
}

function readCookie(name: string): string | null {
  const prefix = `${name}=`;
  for (const part of document.cookie.split(";")) {
    const trimmed = part.trim();
    if (trimmed.startsWith(prefix)) return decodeURIComponent(trimmed.slice(prefix.length));
  }
  return null;
}

export class Api {
  private csrf: string | null = null;
  private onUnauthorized: (() => void) | null = null;
  /** A tiny response cache. It exists so a hover-prefetch and the click that follows it
   *  share one request; any mutation clears it. */
  private cache = new Map<string, { at: number; value: unknown }>();
  private readonly cacheTTL = 15_000;

  setCSRFToken(token: string | null): void {
    this.csrf = token;
  }

  /** Registers the single handler for an expired session. */
  setUnauthorizedHandler(fn: () => void): void {
    this.onUnauthorized = fn;
  }

  invalidate(prefix?: string): void {
    if (!prefix) {
      this.cache.clear();
      return;
    }
    for (const key of this.cache.keys()) {
      if (key.includes(prefix)) this.cache.delete(key);
    }
  }

  async get<T>(path: string, options: RequestOptions & { cache?: boolean } = {}): Promise<T> {
    const url = path + buildQuery(options.query);
    if (options.cache !== false) {
      const hit = this.cache.get(url);
      if (hit && Date.now() - hit.at < this.cacheTTL) return hit.value as T;
    }
    const value = await this.request<T>("GET", url, options);
    this.cache.set(url, { at: Date.now(), value });
    return value;
  }

  async post<T>(path: string, body?: unknown, options: RequestOptions = {}): Promise<T> {
    return this.request<T>("POST", path, { ...options, body });
  }

  async put<T>(path: string, body?: unknown, options: RequestOptions = {}): Promise<T> {
    return this.request<T>("PUT", path, { ...options, body });
  }

  /**
   * A partial update.
   *
   * It is separate from put() because the difference is not cosmetic on this API: a PUT replaces
   * a record's editable fields, so a caller that sends only the field it changed silently clears
   * the rest. Several staff routes are PATCH for exactly that reason — the locale editor is one,
   * where a rename and a switch must not be able to clobber each other.
   */
  async patch<T>(path: string, body?: unknown, options: RequestOptions = {}): Promise<T> {
    return this.request<T>("PATCH", path, { ...options, body });
  }

  async del<T>(path: string, body?: unknown, options: RequestOptions = {}): Promise<T> {
    return this.request<T>("DELETE", path, { ...options, body });
  }

  /**
   * A mutation whose body is *not* JSON.
   *
   * A file upload is the reason this exists: `post()` JSON-encodes its body, so a `File` would
   * arrive as `{}` — a 9 MB PDF stored as two bytes. The bytes are sent as they are, with the
   * caller's own Content-Type, and everything else (CSRF, the idempotency key, the 401 handler,
   * the problem+json envelope) is the same as any other mutation. That is the whole point of
   * adding a method here rather than calling `fetch` from the screen: a second HTTP client would
   * be the one that is silently wrong the next time one of those four changes.
   */
  async postRaw<T>(
    path: string,
    body: BodyInit,
    options: RequestOptions & { contentType?: string } = {},
  ): Promise<T> {
    return this.request<T>("POST", path, {
      ...options,
      raw: body,
      contentType: options.contentType ?? "application/octet-stream",
    });
  }

  private async request<T>(method: string, path: string, options: RequestOptions): Promise<T> {
    const headers: Record<string, string> = { Accept: "application/json" };
    if (options.raw !== undefined) {
      // A raw body carries its own type. The browser must not add one, and it must not add a
      // charset to a binary: the server's allowlist reads the type exactly as sent.
      headers["Content-Type"] = options.contentType ?? "application/octet-stream";
    } else if (options.body !== undefined) {
      headers["Content-Type"] = "application/json";
    }

    if (method !== "GET") {
      this.invalidate();
      if (!options.noCSRF) {
        const token = this.csrf ?? readCookie("billing_csrf");
        if (token) headers[CSRF_HEADER] = token;
      }
      if (!options.noIdempotency) {
        headers[IDEMPOTENCY_HEADER] = options.idempotencyKey ?? crypto.randomUUID();
      }
    }

    let response: Response;
    try {
      response = await fetch(path, {
        method,
        headers,
        credentials: "same-origin",
        body:
          options.raw !== undefined
            ? options.raw
            : options.body === undefined
              ? undefined
              : JSON.stringify(options.body),
        signal: options.signal ?? null,
        // A redirect from the API is never expected; following one would turn a JSON
        // request into an HTML fetch of the sign-in page.
        redirect: "error",
      });
    } catch (err) {
      if (err instanceof DOMException && err.name === "AbortError") throw err;
      throw new ApiError(0, {
        title: "Network error",
        detail: "The server could not be reached. Check your connection and try again.",
        code: "network_error",
      });
    }

    if (response.status === 204) return undefined as T;

    /**
 * True when a 401 means the session itself is gone, rather than a credential the user just typed.
 *
 * A 401 is not one thing. The credential endpoints legitimately answer 401 for a *wrong secret*
 * while the session is perfectly healthy: disabling two-factor with a mistyped code, confirming an
 * enrolment, changing a password with the wrong current one. Treating every 401 as "session
 * expired" signed the user out mid-dialog and discarded the enrolment secret they were half way
 * through typing — the flow looked broken when the server was answering correctly.
 *
 * This is an allowlist, not a denylist of the credential codes. A new credential endpoint added
 * later gets the safe behaviour by default; the alternative would silently sign people out every
 * time someone adds one.
 */
function isSessionDead(code: string | undefined): boolean {
  return (
    code === "unauthenticated" ||
    code === "authentication_required" ||
    code === "operator_session_ended" ||
    // No code at all means the response was not our problem+json (a proxy 401, say), which we
    // cannot interpret as a credential failure, so it keeps the old behaviour.
    code === undefined ||
    code === ""
  );
}

const contentType = response.headers.get("Content-Type") ?? "";
    const isJSON = contentType.includes("json");
    const payload: unknown = isJSON ? await response.json().catch(() => null) : await response.text().catch(() => "");

    if (!response.ok) {
      const problem: Problem =
        payload && typeof payload === "object"
          ? (payload as Problem)
          : { title: response.statusText, detail: typeof payload === "string" ? payload.slice(0, 300) : "" };
      if (response.status === 401 && isSessionDead(problem.code)) this.onUnauthorized?.();
      throw new ApiError(response.status, problem);
    }

    return payload as T;
  }
}

// ------------------------------------------------------------------ events --

/** Event kinds the server publishes. EventSource only delivers named events to a listener
 *  registered for that name, so the list is explicit rather than a wildcard. */
export const EVENT_KINDS = [
  "invoice.created",
  "invoice.issued",
  "invoice.paid",
  "invoice.voided",
  "credit_note.issued",
  "refund.succeeded",
  "service.resume_requested",
  "service.suspend_requested",
  "service.terminate_requested",
  "service.hold_added",
  "service.hold_cleared",
  "service.provisioned",
  "service.suspended",
  "service.resumed",
  "service.terminated",
] as const;

export type LiveEvent = { kind: string; event_id?: number; at?: string; [key: string]: unknown };

export interface EventStream {
  close(): void;
}

export function connectEvents(
  handlers: { onEvent?: (event: LiveEvent) => void; onStatus?: (state: "live" | "offline") => void },
): EventStream {
  let source: EventSource | null = null;
  let closed = false;
  let retry = 0;
  let reconnectTimer: number | undefined;

  const open = () => {
    if (closed) return;
    source = new EventSource("/api/v1/events", { withCredentials: true });

    source.addEventListener("open", () => {
      retry = 0;
      handlers.onStatus?.("live");
    });

    const deliver = (event: MessageEvent<string>) => {
      try {
        const data = JSON.parse(event.data) as LiveEvent;
        handlers.onEvent?.(data);
      } catch {
        // A malformed frame is not worth tearing the stream down for.
      }
    };
    for (const kind of EVENT_KINDS) source.addEventListener(kind, deliver as EventListener);
    // Also accept an unnamed event, in case the server publishes one.
    source.onmessage = deliver;

    source.onerror = () => {
      // EventSource retries on its own, but it also retries a 401 forever. Close and back
      // off so an expired session does not produce a request every three seconds.
      source?.close();
      source = null;
      handlers.onStatus?.("offline");
      if (closed) return;
      retry = Math.min(retry + 1, 6);
      const delay = Math.min(30_000, 1000 * 2 ** retry);
      reconnectTimer = window.setTimeout(open, delay);
    };
  };

  open();

  return {
    close() {
      closed = true;
      if (reconnectTimer !== undefined) window.clearTimeout(reconnectTimer);
      source?.close();
      source = null;
    },
  };
}
