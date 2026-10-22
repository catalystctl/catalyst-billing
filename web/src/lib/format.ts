// Formatting.
//
// Money never becomes a float. The API sends minor units plus a scale, and this module
// inserts the decimal point by string manipulation — the same rule the server uses. A
// panel that computed a total in a double would disagree with the ledger by a cent at
// exactly the moment somebody is checking it.

export interface Amount {
  minor: number;
  currency: string;
  scale: number;
  display?: string;
}

export function formatMinor(minor: number, scale: number): string {
  const negative = minor < 0;
  let digits = String(Math.abs(Math.trunc(minor)));
  if (scale > 0) {
    while (digits.length <= scale) digits = "0" + digits;
    const cut = digits.length - scale;
    digits = `${digits.slice(0, cut)}.${digits.slice(cut)}`;
  }
  const [whole, fraction] = digits.split(".");
  const grouped = (whole ?? "0").replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  return `${negative ? "-" : ""}${grouped}${fraction ? "." + fraction : ""}`;
}

export function money(a: Amount | null | undefined): string {
  if (!a) return "—";
  return formatMinor(a.minor, a.scale ?? 2);
}

/** Presentation rules for one currency, as `GET /api/v1/currencies` returns them.
 *
 *  The server owns this: a currency's symbol, position and grouping are configuration, and a
 *  page that hard-coded "$" would be wrong the moment an installation sells in EUR. The
 *  arithmetic never comes from here — only the presentation does. */
export interface CurrencyFormat {
  code: string;
  symbol: string;
  symbol_position: "prefix" | "suffix";
  group_separator: string;
  decimal_mark: string;
  group_size: number;
  scale: number;
}

/** A format for a code with no server-supplied row, so a page renders a correct number even
 *  before the currency list has loaded. */
export function plainCurrency(code: string, scale = 2): CurrencyFormat {
  return { code, symbol: "", symbol_position: "prefix", group_separator: ",", decimal_mark: ".", group_size: 3, scale };
}

/** Groups the integer part of a digit string, honouring the currency's separators. */
function groupWith(digits: string, format: CurrencyFormat): string {
  const negative = digits.startsWith("-");
  const body = negative ? digits.slice(1) : digits;
  const dot = body.indexOf(".");
  const whole = dot >= 0 ? body.slice(0, dot) : body;
  const rest = dot >= 0 ? body.slice(dot) : "";
  let out = "";
  if (format.group_size <= 0 || !format.group_separator) {
    out = whole;
  } else {
    for (let i = 0; i < whole.length; i += 1) {
      if (i > 0 && (whole.length - i) % format.group_size === 0) out += format.group_separator;
      out += whole[i];
    }
  }
  const tail = format.decimal_mark === "." ? rest : rest.replace(".", format.decimal_mark);
  return `${negative ? "-" : ""}${out}${tail}`;
}

/** Formats an amount with a currency's symbol, position and grouping.
 *
 *  `withCode` appends the ISO code, which is what a multi-currency invoice needs: a bare "$"
 *  on an invoice that could be USD or CAD is ambiguous. */
export function formatCurrency(amount: Amount | null | undefined, format?: CurrencyFormat, withCode = false): string {
  if (!amount || typeof amount.minor !== "number") return "—";
  const fmt = format ?? plainCurrency(amount.currency, amount.scale ?? 2);
  const digits = groupWith(formatMinor(amount.minor, amount.scale ?? 2), fmt);
  const negative = digits.startsWith("-");
  const magnitude = negative ? digits.slice(1) : digits;
  const signed = negative ? `-${magnitude}` : magnitude;
  const withSymbol = fmt.symbol
    ? fmt.symbol_position === "prefix"
      ? `${fmt.symbol}${signed}`
      : `${signed} ${fmt.symbol}`
    : signed;
  return withCode ? `${withSymbol} ${amount.currency}` : withSymbol;
}

/** Sums amounts that share a currency, refusing to mix. Returns null when they differ.
 *
 *  A total across two currencies is meaningless without an exchange rate, and this system
 *  deliberately has none: a price is explicit per currency. Returning null forces the caller
 *  to render one line per currency instead of a number that adds apples to oranges. */
export function sumAmounts(amounts: Amount[]): Amount | null {
  if (amounts.length === 0) return null;
  const first = amounts[0]!;
  let total = 0;
  for (const amount of amounts) {
    if (amount.currency !== first.currency) return null;
    total += amount.minor;
  }
  return { minor: total, currency: first.currency, scale: first.scale };
}

/** The currency code, for the small label before an amount. */
export function currencyOf(a: Amount | null | undefined): string {
  return a?.currency ?? "";
}

/** A full one-line amount, e.g. "USD 1,240.00". */
export function moneyWithCode(a: Amount | null | undefined): string {
  if (!a) return "—";
  return `${a.currency} ${money(a)}`;
}

/** Parses a human amount ("1,240.00") into minor units, or null when it is not a number. */
export function parseMoneyToMinor(input: string, scale = 2): number | null {
  const cleaned = input.trim().replace(/[,\s]/g, "");
  if (!/^-?\d*(\.\d{0,})?$/.test(cleaned) || cleaned === "" || cleaned === "-" || cleaned === ".") {
    return null;
  }
  const negative = cleaned.startsWith("-");
  const body = negative ? cleaned.slice(1) : cleaned;
  const [whole = "0", fraction = ""] = body.split(".");
  const padded = (fraction + "0".repeat(scale)).slice(0, scale);
  const value = Number(whole) * 10 ** scale + Number(padded || 0);
  if (!Number.isFinite(value)) return null;
  return Math.round(negative ? -value : value);
}

// ---------------------------------------------------------------- numbers --

export function number(n: number | null | undefined): string {
  if (n === null || n === undefined) return "—";
  return n.toLocaleString("en-US");
}

export function compact(n: number): string {
  if (Math.abs(n) < 1000) return String(n);
  if (Math.abs(n) < 1_000_000) return `${(n / 1000).toFixed(n % 1000 === 0 ? 0 : 1)}k`;
  return `${(n / 1_000_000).toFixed(1)}m`;
}

export function percent(part: number, whole: number): number {
  if (whole <= 0) return 0;
  return Math.max(0, Math.min(100, Math.round((part / whole) * 100)));
}

export function plural(n: number, one: string, many = `${one}s`): string {
  return `${number(n)} ${n === 1 ? one : many}`;
}

// ------------------------------------------------------------------ dates --

const rtf = new Intl.RelativeTimeFormat("en", { numeric: "auto" });
const DATE_TIME: Intl.DateTimeFormatOptions = {
  year: "numeric",
  month: "short",
  day: "numeric",
  hour: "2-digit",
  minute: "2-digit",
};
const DATE_ONLY: Intl.DateTimeFormatOptions = { year: "numeric", month: "short", day: "numeric" };

function toDate(value: string | Date | null | undefined): Date | null {
  if (!value) return null;
  const d = value instanceof Date ? value : new Date(value);
  return Number.isNaN(d.getTime()) ? null : d;
}

export function dateTime(value: string | Date | null | undefined): string {
  const d = toDate(value);
  return d ? d.toLocaleString("en-US", DATE_TIME) : "—";
}

export function dateOnly(value: string | Date | null | undefined): string {
  const d = toDate(value);
  return d ? d.toLocaleDateString("en-US", DATE_ONLY) : "—";
}

export function isoDate(value: string | Date | null | undefined): string {
  const d = toDate(value);
  if (!d) return "";
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
}

/** "in 3 days", "2 hours ago". The panel's default for anything recent. */
export function relative(value: string | Date | null | undefined): string {
  const d = toDate(value);
  if (!d) return "—";
  const seconds = Math.round((d.getTime() - Date.now()) / 1000);
  const abs = Math.abs(seconds);
  if (abs < 45) return "just now";
  if (abs < 90) return seconds < 0 ? "a minute ago" : "in a minute";
  if (abs < 3600) return rtf.format(Math.round(seconds / 60), "minute");
  if (abs < 86_400) return rtf.format(Math.round(seconds / 3600), "hour");
  if (abs < 2_592_000) return rtf.format(Math.round(seconds / 86_400), "day");
  if (abs < 31_536_000) return rtf.format(Math.round(seconds / 2_592_000), "month");
  return rtf.format(Math.round(seconds / 31_536_000), "year");
}

/** A short age for a dense table cell: "3d", "5h", "12m". */
export function age(value: string | Date | null | undefined): string {
  const d = toDate(value);
  if (!d) return "—";
  const ms = Date.now() - d.getTime();
  if (ms < 0) return "—";
  const mins = Math.floor(ms / 60_000);
  if (mins < 1) return "now";
  if (mins < 60) return `${mins}m`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h`;
  const days = Math.floor(hours / 24);
  if (days < 60) return `${days}d`;
  return `${Math.floor(days / 30)}mo`;
}

export function daysUntil(value: string | Date | null | undefined): number | null {
  const d = toDate(value);
  if (!d) return null;
  return Math.ceil((d.getTime() - Date.now()) / 86_400_000);
}

// ---------------------------------------------------------------- strings --

export function initials(name: string | null | undefined, fallback = "?"): string {
  const source = (name ?? "").trim();
  if (!source) return fallback;
  const parts = source.split(/\s+/).filter(Boolean);
  if (parts.length === 1) return (parts[0] ?? "").slice(0, 2);
  return `${(parts[0] ?? "").charAt(0)}${(parts[parts.length - 1] ?? "").charAt(0)}`;
}

export function titleCase(value: string | null | undefined): string {
  if (!value) return "";
  return value
    .replace(/[_-]+/g, " ")
    .replace(/\b\w/g, (c) => c.toUpperCase());
}

export function humanStatus(value: string | null | undefined): string {
  if (!value) return "—";
  const map: Record<string, string> = {
    pending_payment: "Awaiting payment",
    needs_review: "Needs review",
    retry_wait: "Retry scheduled",
    uncollectible: "Uncollectible",
    scheduled_cancel: "Cancels at period end",
    not_payable: "Not payable",
    unknown: "Outcome unknown",
    queued: "Queued",
    running: "Running",
    succeeded: "Succeeded",
    failed: "Failed",
    open: "Open",
    paid: "Paid",
    void: "Void",
    draft: "Draft",
    issued: "Issued",
    applied: "Applied",
    refunded: "Refunded",
    disputed: "Disputed",
    pending: "Pending",
    approved: "Approved",
    rejected: "Rejected",
    expired: "Expired",
    created: "Created",
    succeeded_refund: "Refunded",
    active: "Active",
    suspended: "Suspended",
    terminated: "Terminated",
    cancelled: "Cancelled",
    fulfilling: "Fulfilling",
    completed: "Completed",
    processing: "Processing",
    suppressed: "Suppressed",
    sent: "Sent",
    sending: "Sending",
    unverified: "Unverified",
    verified: "Verified",
  };
  return map[value] ?? titleCase(value);
}

export type Tone = "neutral" | "success" | "warning" | "danger" | "info" | "accent";

/** The colour a status is allowed to have. One map, so "paid" is green everywhere. */
export function statusTone(value: string | null | undefined): Tone {
  switch (value) {
    case "paid":
    case "succeeded":
    case "completed":
    case "active":
    case "approved":
    case "sent":
    case "verified":
    case "applied":
      return "success";
    case "open":
    case "issued":
    case "queued":
    case "pending":
    case "pending_payment":
    case "fulfilling":
    case "processing":
    case "sending":
    case "running":
    case "scheduled_cancel":
      return "info";
    case "overdue":
    case "retry_wait":
    case "warning":
    case "needs_review":
    case "unknown":
    case "suspended":
    case "unverified":
    case "expired":
      return "warning";
    case "failed":
    case "void":
    case "uncollectible":
    case "disputed":
    case "terminated":
    case "rejected":
    case "danger":
      return "danger";
    case "refunded":
    case "cancelled":
    case "suppressed":
    case "draft":
      return "neutral";
    default:
      return "neutral";
  }
}

export function shortId(id: string | null | undefined, length = 8): string {
  if (!id) return "—";
  return id.length <= length ? id : id.slice(0, length);
}

export function truncate(value: string, max: number): string {
  if (value.length <= max) return value;
  return `${value.slice(0, max - 1)}…`;
}

/** A stable, non-cryptographic key for a mutation. The server only needs uniqueness. */
export function idempotencyKey(): string {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) {
    return crypto.randomUUID();
  }
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}

export function safeExternalURL(url: string | null | undefined): string | null {
  if (!url) return null;
  // The origin is only used to resolve a relative URL, and only when there is a document to
  // resolve it against. Without one (a test, a worker) a relative URL is not a link a browser
  // could follow anyway, so it is refused rather than guessed at.
  const origin = typeof window !== "undefined" ? window.location.origin : undefined;
  try {
    const parsed = new URL(url, origin);
    if (parsed.protocol === "https:" || parsed.protocol === "http:") return parsed.toString();
    return null;
  } catch {
    return null;
  }
}
