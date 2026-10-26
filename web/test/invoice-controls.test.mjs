// ADM-31 — the two creation-time controls on the New-invoice screen.
//
// The screen itself builds DOM, so its rules live in lib/invoice-controls.ts where they can be
// run under `node --test`. These tests pin the three things that would go wrong silently:
//
//   * a draft must not carry `send_email` (the field only means something to an issuing
//     request), and an issuing request must carry the operator's answer explicitly — an
//     omitted field means "installation default", which would silently re-enable a mail the
//     operator turned off;
//   * the PDF affordance must follow the status the server reported, because the document
//     route answers 409 `not_issued` for a draft;
//   * the request never grows a total: the server prices the invoice, the client sends its
//     inputs.
//
// A final pair of assertions reads the view's source: the helpers are worthless if the screen
// stops calling them, and a view file cannot be imported in Node.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import {
  creationSummary,
  hasInvoicePdf,
  invoiceCreateBody,
  invoicePdfPath,
  sendEmailSelectable,
} from "../.test-build/lib/invoice-controls.js";

const baseLine = {
  description: "Managed hosting — March",
  quantity: 2,
  unitAmountMinor: 12000,
  currency: "USD",
};

test("a draft's body carries no send_email at all", () => {
  const body = invoiceCreateBody({
    customerId: "cus_1",
    issueNow: false,
    sendEmail: true,
    dueInDays: 30,
    memo: "",
    lines: [baseLine],
  });
  assert.equal(body.issue_now, false);
  assert.equal("send_email" in body, false, "a draft has nothing to email; the field must be omitted");
});

test("an issuing body carries the operator's answer explicitly, on and off", () => {
  const on = invoiceCreateBody({
    customerId: "cus_1",
    issueNow: true,
    sendEmail: true,
    dueInDays: 30,
    memo: "",
    lines: [baseLine],
  });
  assert.equal(on.send_email, true);

  const off = invoiceCreateBody({
    customerId: "cus_1",
    issueNow: true,
    sendEmail: false,
    dueInDays: 30,
    memo: "",
    lines: [baseLine],
  });
  assert.equal(off.send_email, false, "off must be sent as false, not omitted, or the installation default wins");
  assert.equal("send_email" in off, true);
});

test("the body never computes a total: minor units and quantity pass through", () => {
  const body = invoiceCreateBody({
    customerId: "cus_1",
    issueNow: false,
    sendEmail: false,
    dueInDays: 7,
    memo: "Backdated",
    lines: [{ ...baseLine, quantity: 3, unitAmountMinor: 4599 }],
  });
  assert.equal(body.lines[0].quantity, 3);
  assert.equal(body.lines[0].unit_amount_minor, 4599);
  for (const key of ["total", "subtotal", "tax", "amount"]) {
    assert.equal(key in body.lines[0], false, `the client must not send a ${key}`);
  }
});

test("product and price version are omitted for an ordinary line, present when chosen", () => {
  const ordinary = invoiceCreateBody({
    customerId: "cus_1",
    issueNow: false,
    sendEmail: false,
    dueInDays: 30,
    memo: "",
    lines: [baseLine],
  });
  assert.equal("product_id" in ordinary.lines[0], false);
  assert.equal("price_version_id" in ordinary.lines[0], false);

  const sold = invoiceCreateBody({
    customerId: "cus_1",
    issueNow: false,
    sendEmail: false,
    dueInDays: 30,
    memo: "",
    lines: [{ ...baseLine, productId: "prod_1", priceVersionId: "price_2" }],
  });
  assert.equal(sold.lines[0].product_id, "prod_1");
  assert.equal(sold.lines[0].price_version_id, "price_2");
});

test("the send-email toggle is only live while the invoice is being issued", () => {
  assert.equal(sendEmailSelectable(false), false);
  assert.equal(sendEmailSelectable(true), true);
});

test("the PDF path encodes the id into one segment", () => {
  assert.equal(invoicePdfPath("inv_123"), "/api/v1/staff/invoices/inv_123/pdf");
  assert.equal(invoicePdfPath("a/b?c#d"), "/api/v1/staff/invoices/a%2Fb%3Fc%23d/pdf");
});

test("hasInvoicePdf mirrors the route: a draft has no document, anything else does", () => {
  for (const status of ["open", "paid", "overdue", "void", "uncollectible"]) {
    assert.equal(hasInvoicePdf(status), true, `${status} should have a document`);
  }
  for (const status of ["draft", "DRAFT", "", "  "]) {
    assert.equal(hasInvoicePdf(status), false, `${JSON.stringify(status)} should have none`);
  }
});

test("the creation summary ties the PDF to the reported status, not to the request", () => {
  const issued = creationSummary(
    { id: "inv_9", number: "INV-9", status: "open" },
    { sendEmail: true, autoDownload: true },
  );
  assert.equal(issued.issued, true);
  assert.equal(issued.pdfPath, "/api/v1/staff/invoices/inv_9/pdf");
  assert.equal(issued.offerDownload, true);

  // Issued, but the operator did not ask for the file: it exists, just is not offered.
  const quiet = creationSummary(
    { id: "inv_9", number: "INV-9", status: "paid" },
    { sendEmail: false, autoDownload: false },
  );
  assert.equal(quiet.offerDownload, false);
  assert.equal(quiet.pdfPath, "/api/v1/staff/invoices/inv_9/pdf");
});

test("a draft's summary offers no PDF even when the toggle was on", () => {
  const summary = creationSummary(
    { id: "inv_9", number: "", status: "draft" },
    { sendEmail: true, autoDownload: true },
  );
  assert.equal(summary.issued, false);
  assert.equal(summary.pdfPath, null);
  assert.equal(summary.offerDownload, false);
  assert.match(summary.emailNote, /draft/i, "the operator is told why nothing was sent");
});

test("the mail note distinguishes queued from suppressed from draft", () => {
  const queued = creationSummary({ id: "i", number: "INV-1", status: "open" }, { sendEmail: true, autoDownload: false });
  const suppressed = creationSummary({ id: "i", number: "INV-1", status: "open" }, { sendEmail: false, autoDownload: false });
  assert.match(queued.emailNote, /queued/i);
  assert.match(suppressed.emailNote, /no notification was queued/i);
});

test("a session without invoice:read:all is not offered a PDF the route would refuse", () => {
  const summary = creationSummary(
    { id: "inv_9", number: "INV-9", status: "open" },
    { sendEmail: true, autoDownload: true, canDownloadPdf: false },
  );
  assert.equal(summary.issued, true, "the invoice is still issued");
  assert.equal(summary.pdfPath, null);
  assert.equal(summary.offerDownload, false);
  assert.match(summary.emailNote, /invoice:read:all/, "the operator is told which permission is missing");
});

// --- the wiring guard: the helpers must actually be used by the screen ---------

const viewSource = readFileSync(new URL("../src/views/invoice-new.ts", import.meta.url), "utf8");

test("the view builds its body through invoiceCreateBody", () => {
  assert.match(viewSource, /invoiceCreateBody\(/, "the create body must come from the tested builder");
});

test("the view no longer posts send_email by hand", () => {
  assert.equal(
    /send_email\s*:/.test(viewSource),
    false,
    "send_email belongs to invoiceCreateBody; a second literal is how the draft rule drifts",
  );
});

test("the view renders the PDF as a download anchor after creation", () => {
  assert.match(viewSource, /creationSummary\(/, "the created document's status must decide the affordance");
  assert.match(viewSource, /download: true/, "the PDF must be an anchor with download, not a rendered page");
  assert.match(viewSource, /summary\.pdfPath/, "the download must point at the issued document");
});
