// The two creation-time decisions a manual invoice offers (ADM-31): whether the customer is
// emailed, and whether the operator leaves with the PDF.
//
// This lives outside the view because both decisions are rules about the API contract, not
// about layout, and a rule that only exists inside a render function is a rule nobody can
// test. The view asks `invoiceCreateBody` what to send and `creationSummary` what to show.
//
// What the server actually supports, read from internal/httpapi/staff_write.go:
//
//  - `POST /api/v1/staff/invoices` takes `send_email` as `*bool`. Nil means "the installation's
//    default" (`invoices.email_on_issue`, on unless turned off). It is only consulted when
//    `issue_now` is true: the mail is queued inside the issuing transaction. A draft therefore
//    has nothing to send and the field is meaningless for one, so this module omits it rather
//    than sending a value the server will ignore.
//  - `GET /api/v1/staff/invoices/{id}/pdf` answers 409 `not_issued` for a draft and
//    `application/pdf` for anything else. So the PDF affordance is conditional on the status
//    the create response reports, not on a client-side guess about what "issued" means.
//
// The field is sent explicitly (true or false), never omitted, when the document is being
// issued: an operator who turned the mail off must not have it turned back on by the
// installation default. That is the one behaviour a toggle exists to guarantee.

/** One line as the POST body takes it. Amounts are minor units; totals stay the server's job. */
export interface InvoiceLineInput {
  description: string;
  quantity: number;
  unitAmountMinor: number;
  currency: string;
  /** Empty means "this line provisions nothing" and the key is left out entirely. */
  productId?: string;
  priceVersionId?: string;
}

export interface InvoiceCreateInput {
  customerId: string;
  issueNow: boolean;
  /** The operator's answer to the toggle. Ignored for a draft. */
  sendEmail: boolean;
  dueInDays: number;
  memo: string;
  lines: InvoiceLineInput[];
}

/** The body `POST /api/v1/staff/invoices` receives. */
export interface InvoiceCreateBody {
  customer_id: string;
  issue_now: boolean;
  due_in_days: number;
  send_email?: boolean;
  memo: string;
  lines: Array<{
    description: string;
    quantity: number;
    unit_amount_minor: number;
    currency: string;
    product_id?: string;
    price_version_id?: string;
  }>;
}

/**
 * The create body, including only the fields the server reads for this document.
 *
 * `send_email` is present exactly when the document is issued in this request: issued, it is
 * the operator's answer; drafted, it is absent because a draft queues no mail and sending
 * `false` there would record a choice about a notification nobody asked for.
 */
export function invoiceCreateBody(input: InvoiceCreateInput): InvoiceCreateBody {
  return {
    customer_id: input.customerId,
    issue_now: input.issueNow,
    due_in_days: input.dueInDays,
    ...(input.issueNow ? { send_email: input.sendEmail } : {}),
    memo: input.memo,
    lines: input.lines.map((line) => ({
      description: line.description,
      quantity: line.quantity,
      unit_amount_minor: line.unitAmountMinor,
      currency: line.currency,
      // Omitted entirely for an ordinary line, so the server stores NULL and fulfilment knows
      // there is nothing to provision. An empty string would be a lookup for a product that
      // does not exist.
      ...(line.productId ? { product_id: line.productId } : {}),
      ...(line.priceVersionId ? { price_version_id: line.priceVersionId } : {}),
    })),
  };
}

/**
 * Whether the send-email checkbox is live.
 *
 * False while the invoice stays a draft, mirroring the server: the flag has no meaning until
 * the document is issued. Disabled rather than hidden, so the operator sees that the choice
 * exists and why it is unavailable.
 */
export function sendEmailSelectable(issueNow: boolean): boolean {
  return issueNow;
}

/** The create response, as far as these controls care. */
export interface CreatedInvoice {
  id: string;
  number: string;
  status: string;
}

/** The staff PDF route for one invoice. Encoded: an id reaches a path segment. */
export function invoicePdfPath(id: string): string {
  return `/api/v1/staff/invoices/${encodeURIComponent(id)}/pdf`;
}

/**
 * Whether a PDF exists for a document in this status.
 *
 * The route refuses a draft with 409 `not_issued` and serves everything else, so "issued"
 * here is "not a draft". A paid or overdue invoice still has a document; it is the same
 * frozen snapshot with a different status printed on it.
 */
export function hasInvoicePdf(status: string): boolean {
  const s = status.trim().toLowerCase();
  return s !== "" && s !== "draft";
}

export interface CreationSummary {
  issued: boolean;
  /** Null when there is no document, or when this session may not download it. */
  pdfPath: string | null;
  /** True only when a download is allowed and the operator asked for it at creation. */
  offerDownload: boolean;
  /** What to tell the operator about the notification, in the terms the server guarantees. */
  emailNote: string;
}

/**
 * What the created invoice means for these two controls.
 *
 * The wording about mail is deliberately about the queue, not delivery: the server queues the
 * notification in the issuing transaction and the worker sends it later, so "emailed" would be a
 * claim the client cannot make.
 *
 * `canDownloadPdf` mirrors the route's own permission (`invoice:read:all`, see
 * internal/httpapi/invoice_pdf.go). Creating an invoice needs `invoice:write:all`, which is a
 * different grant, so a session that may create is not automatically one the PDF route will
 * serve; offering a link it would 403 on is a dead end a confirmation screen should not have.
 */
export function creationSummary(
  created: CreatedInvoice,
  opts: { sendEmail: boolean; autoDownload: boolean; canDownloadPdf?: boolean },
): CreationSummary {
  const issued = hasInvoicePdf(created.status);
  const downloadable = issued && opts.canDownloadPdf !== false;
  const pdfPath = downloadable ? invoicePdfPath(created.id) : null;
  let emailNote: string;
  if (!issued) {
    emailNote = "No email was queued: a draft has no number to announce. The customer is notified when it is issued.";
  } else if (!downloadable) {
    emailNote = "The invoice is issued. Downloading its PDF needs the invoice:read:all permission; ask an administrator if you should have it.";
  } else if (opts.sendEmail) {
    emailNote = "The invoice notification is queued in the same transaction that issued the number.";
  } else {
    emailNote = "No notification was queued, as you chose. The invoice is issued and the customer can still see it in the portal.";
  }
  return {
    issued,
    pdfPath,
    offerDownload: downloadable && opts.autoDownload,
    emailNote,
  };
}
