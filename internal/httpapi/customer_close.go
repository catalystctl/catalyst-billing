package httpapi

// Closing an account: the customer's own, and a customer record's.
//
// These are two operations that look alike and are not, and the difference is the whole point of
// this file:
//
//	GET  /api/v1/portal/account-closure   what closing would do, and what stops it
//	POST /api/v1/portal/account-closure   the customer closes their own account
//	GET  /api/v1/portal/mfa               the caller's own second-factor state
//	GET  /api/v1/staff/customers/{id}/account-state  closure + per-login security, for the overview
//	DELETE /api/v1/staff/customers/{id}   an operator closes a customer record
//
// **The customer's operation is a request about their relationship; the operator's is a change to
// a record.** The customer's version therefore refuses while anything is still owed or still
// running — they cannot walk away from a live service we are provisioning or an invoice we are
// collecting — and it says exactly which service and which invoice. The operator's version has
// the same guard for the same reason: closing a record is not a way to make a debt disappear, and
// a delete that silently orphaned an active service would leave a server running that nobody is
// billing. Both refusals name the blocking thing, because "that did not work" is the answer that
// produces a support ticket rather than a resolution.
//
// **The delete is soft, and that is a decision rather than a shortcut.**
//
// A hard delete of a customer is not available even in principle. `invoices`, `orders`,
// `services`, `subscriptions`, `credit_ledger` and `tickets` all reference `customers (id)` with
// NO ACTION, and `audit_events` names the actor of every change that ever touched the record. So
// `DELETE FROM customers` would fail on a foreign key for any customer who has ever bought
// anything, and would succeed only for a record with no history at all — while still destroying
// the audit rows that explain how the account was created and closed. The two honest options are
// therefore "refuse to delete anything that has ever been used" (which is almost everything) or
// "mark it closed and keep the record". This file takes the second, with `customers.deleted_at`
// (migration 0050):
//
//   - the row, its invoices, its services and its audit trail survive, which is what an auditor
//     and a tax authority need for years after the customer has left;
//   - the *login* stops working, so the customer genuinely cannot use the account again — the
//     practical meaning of closing it;
//   - nothing is cascaded. A cascade here would be the one change that quietly destroys the
//     evidence this operation is supposed to preserve.
//
// What the customer is told is built from the same facts the handler enforces, not from prose in
// the screen: `effects` below is returned by both the preflight and the close, so a confirmation
// dialog cannot promise something the server does not do.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("64-account-closure", registerAccountClosure)
}

func registerAccountClosure(r *router) {
	// The customer's own account. `customer:read:self` to look, `customer:write:self` to act:
	// closing is a change to the account, so a read-only customer token cannot do it.
	r.get("/api/v1/portal/account-closure", authn.PermCustomerReadSelf, r.s.handlePortalClosureState)
	r.mutation(http.MethodPost, "/api/v1/portal/account-closure",
		authn.PermCustomerWriteSelf, r.s.handlePortalCloseAccount)
	// The second-factor state of the caller's own account. The identity endpoint does not report
	// it, so without this the security screen can only guess whether a factor is on.
	r.get("/api/v1/portal/mfa", authn.PermCustomerReadSelf, r.s.handlePortalMFAState)

	// The operator's view of the same facts, and the guarded delete.
	//
	// `customer:read:all` reads, `customer:write:all` closes: the write permission is the one that
	// already governs creating and editing a customer, and closing is the same kind of act.
	r.staffGet("/api/v1/staff/customers/{id}/account-state",
		authn.PermCustomerReadAll, r.s.handleStaffCustomerAccountState)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/customers/{id}",
		authn.PermCustomerWriteAll, r.s.handleStaffDeleteCustomer)
}

// Errors the closure paths answer with.
var (
	// errCustomerNotFound is an unknown or malformed customer id.
	errCustomerNotFound = errors.New("httpapi: no such customer")
	// errCustomerClosed is a second close of an account that is already closed.
	errCustomerClosed = errors.New("httpapi: the account is already closed")
)

// closeBlocker is one reason an account cannot be closed.
//
// The fields are what a screen needs to render a list the customer can act on: which service,
// what state it is in, which invoice, how much is left. A bare count would be enough for a
// refusal sentence and useless for the "so what do I do" that follows it.
type closeBlocker struct {
	// Kind is `live_service` or `unpaid_invoice`.
	Kind string
	ID   string
	// Label is the product name for a service and the invoice number for an invoice.
	Label string
	// Status is the service or invoice status.
	Status string
	// Detail is one sentence naming the thing, used verbatim in the refusal.
	Detail string
}

func (b closeBlocker) json() map[string]any {
	return map[string]any{
		"kind":   b.Kind,
		"id":     b.ID,
		"label":  b.Label,
		"status": b.Status,
		"detail": b.Detail,
	}
}

// closureBlockedError carries the blockers so the handler can name them.
type closureBlockedError struct {
	blockers []closeBlocker
}

func (e closureBlockedError) Error() string {
	return "httpapi: the account cannot be closed while " + describeBlockers(e.blockers)
}

// describeBlockers renders the blocking things as a sentence fragment.
//
// Grouped by kind and each one named, because the sentence is read by a person who has to go and
// do something about it: "1 live service (Game Server — active) and 1 unpaid invoice (INV-1042 —
// 12.00 USD unpaid)".
func describeBlockers(blockers []closeBlocker) string {
	var services, invoices []string
	for _, b := range blockers {
		switch b.Kind {
		case "live_service":
			services = append(services, b.Detail)
		case "unpaid_invoice":
			invoices = append(invoices, b.Detail)
		}
	}
	var parts []string
	if len(services) > 0 {
		parts = append(parts, fmt.Sprintf("%s (%s)", plural(len(services), "live service"), strings.Join(services, "; ")))
	}
	if len(invoices) > 0 {
		parts = append(parts, fmt.Sprintf("%s (%s)", plural(len(invoices), "unpaid invoice"), strings.Join(invoices, "; ")))
	}
	if len(parts) == 0 {
		return "it has records that must be settled first"
	}
	return strings.Join(parts, " and ")
}

// rowQuerier is the slice of pgx that the blocker queries need.
//
// It exists so the preflight and the enforcement run *the same SQL*: `*store.Pool` and `pgx.Tx`
// both satisfy it, so there is one implementation of "what blocks a close" rather than two that
// can drift apart — which is the classic way a preflight starts promising something the write
// then refuses.
type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// closeBlockers returns everything that stops this customer being closed.
//
// Two queries rather than one UNION because the two lists are different shapes and the screen
// renders them differently. Both are index lookups on `customer_id`.
//
// "Live" is every service state but `terminated`: `pending` and `provisioning` are a service we
// are about to create (and would create for a closed account), `active` and `suspended` are one
// that exists, `terminating` is one on its way out, and `error` is one somebody has to look at.
// Only `terminated` means "nothing of ours is running for this customer".
//
// "Unpaid" is an issued demand for money that has not been met: `open` or `disputed`, with more
// total than has been paid. A `draft` is not yet a demand, `void` is not one any more, and
// `uncollectible` is a decision an operator already made about the debt.
func (s *Server) closeBlockers(ctx context.Context, q rowQuerier, customerID string) ([]closeBlocker, error) {
	var out []closeBlocker

	rows, err := q.Query(ctx, `
		SELECT s.id::text, s.status, COALESCE(NULLIF(p.name, ''), 'a service')
		FROM services s
		LEFT JOIN products p ON p.id = s.product_id
		WHERE s.customer_id = $1 AND s.status <> 'terminated'
		ORDER BY s.created_at`, customerID)
	if err != nil {
		return nil, fmt.Errorf("httpapi: list live services: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, status, product string
		)
		if err := rows.Scan(&id, &status, &product); err != nil {
			return nil, fmt.Errorf("httpapi: scan live service: %w", err)
		}
		out = append(out, closeBlocker{
			Kind: "live_service", ID: id, Label: product, Status: status,
			Detail: fmt.Sprintf("%s is %s", product, status),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("httpapi: read live services: %w", err)
	}

	invoiceRows, err := q.Query(ctx, `
		SELECT i.id::text, COALESCE(i.number, ''), i.status, i.currency,
		       COALESCE(c.scale, 2), i.total_minor - i.amount_paid_minor
		FROM invoices i
		LEFT JOIN currencies c ON c.code = i.currency
		WHERE i.customer_id = $1
		  AND i.status IN ('open', 'disputed')
		  AND i.total_minor > i.amount_paid_minor
		ORDER BY i.due_at NULLS LAST, i.created_at`, customerID)
	if err != nil {
		return nil, fmt.Errorf("httpapi: list unpaid invoices: %w", err)
	}
	defer invoiceRows.Close()
	for invoiceRows.Next() {
		var (
			id, number, status, currency string
			scale                        int
			due                          int64
		)
		if err := invoiceRows.Scan(&id, &number, &status, &currency, &scale, &due); err != nil {
			return nil, fmt.Errorf("httpapi: scan unpaid invoice: %w", err)
		}
		label := number
		if label == "" {
			label = "a draft-numbered invoice"
		}
		out = append(out, closeBlocker{
			Kind: "unpaid_invoice", ID: id, Label: label, Status: status,
			Detail: fmt.Sprintf("%s is %s with %s %s unpaid",
				label, status, currency, formatMinor(due, scale)),
		})
	}
	if err := invoiceRows.Err(); err != nil {
		return nil, fmt.Errorf("httpapi: read unpaid invoices: %w", err)
	}

	return out, nil
}

// closureEffects is what closing the account actually does, as the confirmation dialog must state
// it. Both the preflight and the close return it, so the two can never disagree.
func closureEffects() map[string]any {
	return map[string]any{
		"portal_access": map[string]any{
			"revoked": true,
			"detail":  "You are signed out and the login stops working. You cannot sign in to this account again.",
		},
		"services": map[string]any{
			"terminated": false,
			"detail":     "Closing your account does not terminate a service. That is why it is refused while one is still running: cancel the service first, then close the account.",
		},
		"invoices": map[string]any{
			"written_off": false,
			"detail":      "Closing your account never writes off an invoice. That is why it is refused while one is unpaid: settle it first.",
		},
		"record": map[string]any{
			"retained": true,
			"detail":   "Your customer record, your invoices and the audit trail are kept for accounting and legal retention. Nothing is deleted from the ledger.",
		},
	}
}

// closureStatement is the one sentence the confirmation dialog leads with.
func closureStatement() string {
	return "Closing your account signs you out for good and stops the login working. It keeps your " +
		"records and invoices for accounting, and it does not terminate a service or cancel an " +
		"invoice — so it is only possible while nothing is running and nothing is unpaid."
}

// customerIDForUser resolves the billing record a login owns.
//
// It reads the link directly rather than going through the portal manager, because closing an
// account must work on an installation whose portal is not configured: the row is the fact, and
// the manager is a convenience on top of it.
//
// An open record wins over a closed one. One login may be the portal account of two customers —
// a bookkeeper who pays for two companies — and the one they are asking about is the one that is
// still open. When every record is closed the closed one is returned anyway, so the screen can
// say "this account is closed" instead of "no such record".
func (s *Server) customerIDForUser(ctx context.Context, userID string) (string, error) {
	var customerID string
	err := s.pool.QueryRow(ctx, `
		SELECT ca.customer_id::text FROM customer_accounts ca
		JOIN customers c ON c.id = ca.customer_id
		WHERE ca.user_id = $1
		ORDER BY (c.deleted_at IS NULL) DESC, ca.created_at LIMIT 1`, userID).Scan(&customerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errCustomerNotFound
	}
	if err != nil {
		return "", fmt.Errorf("httpapi: resolve customer for user: %w", err)
	}
	return customerID, nil
}

// ------------------------------------------------------------ the customer --

// handlePortalClosureState serves GET /api/v1/portal/account-closure.
//
// The preflight exists so the confirmation dialog can say what will happen *and* what currently
// stops it, before the customer types anything. A destructive action that only explains itself
// after it fails is a screen that teaches by error.
func (s *Server) handlePortalClosureState(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "the database is not configured")
		return
	}
	user, _ := userFrom(r.Context())

	customerID, err := s.customerIDForUser(r.Context(), user.ID)
	if err != nil {
		s.writeClosureError(w, r, err)
		return
	}

	var (
		closedAt    *string
		closedKind  *string
		closedEmail *string
	)
	if err := s.pool.QueryRow(r.Context(), `
		SELECT c.deleted_at::text, c.deletion_kind, u.email::text
		FROM customers c
		LEFT JOIN users u ON u.id = c.deleted_by
		WHERE c.id = $1`, customerID).Scan(&closedAt, &closedKind, &closedEmail); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.writeClosureError(w, r, errCustomerNotFound)
			return
		}
		s.writeClosureError(w, r, err)
		return
	}

	blockers, err := s.closeBlockers(r.Context(), s.pool, customerID)
	if err != nil {
		s.writeClosureError(w, r, err)
		return
	}

	out := make([]map[string]any, 0, len(blockers))
	for _, b := range blockers {
		out = append(out, b.json())
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"customer_id": customerID,
		"closed":      closedAt != nil,
		"closed_at":   closedAt,
		"closed_kind": closedKind,
		"closed_by":   closedEmail,
		"can_close":   closedAt == nil && len(blockers) == 0,
		"blockers":    out,
		"effects":     closureEffects(),
		"statement":   closureStatement(),
		// The phrase the dialog asks the customer to type. It is served rather than hard-coded in
		// the screen so the confirmation and the server's check cannot drift apart.
		"confirm_phrase": closeConfirmPhrase,
	})
}

// closeConfirmPhrase is what a customer types to close their own account.
//
// A typed phrase rather than a checkbox: closing an account is irreversible from the customer's
// side, and a check box is dismissed by a mis-click. The server checks it too, so a client that
// forgets the dialog cannot close an account by accident.
const closeConfirmPhrase = "close my account"

// handlePortalCloseAccount serves POST /api/v1/portal/account-closure.
func (s *Server) handlePortalCloseAccount(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "the database is not configured")
		return
	}
	user, _ := userFrom(r.Context())

	var req struct {
		Reason  string `json:"reason"`
		Confirm string `json:"confirm"`
	}
	if !s.decodeJSON(w, r, &req, 8<<10) {
		return
	}
	if strings.TrimSpace(strings.ToLower(req.Confirm)) != closeConfirmPhrase {
		writeProblem(w, http.StatusUnprocessableEntity, "confirmation_mismatch",
			"type \""+closeConfirmPhrase+"\" to confirm closing the account")
		return
	}

	customerID, err := s.customerIDForUser(r.Context(), user.ID)
	if err != nil {
		s.writeClosureError(w, r, err)
		return
	}

	reason := strings.TrimSpace(req.Reason)
	if len(reason) > 500 {
		writeProblem(w, http.StatusBadRequest, "invalid_reason", "the reason is too long")
		return
	}

	err = s.closeCustomer(r.Context(), customerID, "self", user.ID, reason,
		s.auditorFor(r, "the customer closed their own account"))
	if err != nil {
		s.writeClosureError(w, r, err)
		return
	}

	// Every session of the account has just been revoked by the close, including the caller's.
	// Clearing the cookies is what makes the next request a signed-out one instead of a 401 the
	// screen has to explain.
	s.clearSessionCookies(w)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "closed",
		"customer_id": customerID,
		"effects":     closureEffects(),
		"statement":   closureStatement(),
	})
}

// handlePortalMFAState serves GET /api/v1/portal/mfa.
//
// It is the read the customer's security screen was missing: whether a factor is confirmed, how
// many recovery codes are left, and whether an operator's reset has left an enrolment demand
// outstanding. Without it the screen had to guess, and a "two-factor is on" badge that is a guess
// is worse than no badge.
func (s *Server) handlePortalMFAState(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	user, _ := userFrom(r.Context())

	status, err := s.auth.Service.MFAStatusFor(r.Context(), user.ID)
	if err != nil {
		if errors.Is(err, authn.ErrUserNotFound) {
			writeProblem(w, http.StatusNotFound, "not_found", "no such account")
			return
		}
		s.cfg.Logger.ErrorContext(r.Context(), "mfa status read failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error", "the request could not be completed")
		return
	}

	var resetBy any
	if status.ResetBy != "" {
		resetBy = status.ResetBy
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"has_mfa":                  status.HasMFA,
		"enrolled_at":              timeOrNil(status.EnrolledAt),
		"recovery_codes_remaining": status.RecoveryCodesRemaining,
		// The demand an operator's reset leaves behind. The screen refuses to let the account past
		// this: enrolling is not an optional improvement while it is true.
		"enrolment_required": status.EnrolmentRequired,
		"reset_at":           timeOrNil(status.ResetAt),
		"reset_by":           resetBy,
	})
}

// -------------------------------------------------------------- the staff --

// handleStaffCustomerAccountState serves GET /api/v1/staff/customers/{id}/account-state.
//
// One request answers the three questions the customer overview's security panel asks: is the
// record closed, what would stop it being closed, and what is the login security of each account
// attached to it. The alternative was three round trips from a screen that already makes one for
// the customer itself.
func (s *Server) handleStaffCustomerAccountState(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "the database is not configured")
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such customer")
		return
	}

	var (
		closedAt     *time.Time
		closedKind   *string
		closedReason string
		closedBy     *string
	)
	err := s.pool.QueryRow(r.Context(), `
		SELECT c.deleted_at, c.deletion_kind, c.deletion_reason, u.email::text
		FROM customers c
		LEFT JOIN users u ON u.id = c.deleted_by
		WHERE c.id = $1`, id).Scan(&closedAt, &closedKind, &closedReason, &closedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such customer")
		return
	}
	if err != nil {
		s.staffError(w, r, err)
		return
	}

	blockers, err := s.closeBlockers(r.Context(), s.pool, id)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(blockers))
	for _, b := range blockers {
		out = append(out, b.json())
	}

	// The MFA facts are read in one query for every login on the account rather than one call per
	// login to the auth service: this is a read-only view of three columns, and a screen that made
	// N round trips to render a list is a screen that gets slower with every account a customer
	// adds.
	logins := make([]map[string]any, 0, 4)
	rows, err := s.pool.Query(r.Context(), `
		SELECT ca.user_id::text, u.email, u.status,
		       m.confirmed_at,
		       (SELECT count(*) FROM mfa_recovery_codes rc
		         WHERE rc.user_id = u.id AND rc.used_at IS NULL),
		       u.mfa_enrolment_required, u.mfa_reset_at, rb.email::text
		FROM customer_accounts ca
		JOIN users u ON u.id = ca.user_id
		LEFT JOIN mfa_credentials m ON m.user_id = u.id AND m.kind = 'totp'
		LEFT JOIN users rb ON rb.id = u.mfa_reset_by
		WHERE ca.customer_id = $1
		ORDER BY ca.created_at`, id)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var (
			userID, email, status string
			enrolledAt            *time.Time
			remaining             int
			required              bool
			resetAt               *time.Time
			resetBy               *string
		)
		if err := rows.Scan(&userID, &email, &status, &enrolledAt, &remaining,
			&required, &resetAt, &resetBy); err != nil {
			s.staffError(w, r, err)
			return
		}
		logins = append(logins, map[string]any{
			"user_id":                  userID,
			"email":                    email,
			"status":                   status,
			"has_mfa":                  enrolledAt != nil,
			"enrolled_at":              timeOrNil(enrolledAt),
			"recovery_codes_remaining": remaining,
			"enrolment_required":       required,
			"reset_at":                 timeOrNil(resetAt),
			"reset_by":                 resetBy,
		})
	}
	if err := rows.Err(); err != nil {
		s.staffError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"customer_id":    id,
		"closed":         closedAt != nil,
		"closed_at":      timeOrNil(closedAt),
		"closed_kind":    closedKind,
		"closed_by":      closedBy,
		"closed_reason":  closedReason,
		"can_close":      closedAt == nil && len(blockers) == 0,
		"blockers":       out,
		"logins":         logins,
		"confirm_phrase": closeConfirmPhrase,
		"delete_statement": "Closing a customer record revokes their portal access and keeps every " +
			"invoice, service and audit row. It is refused while a service is live or an invoice is unpaid.",
	})
}

// handleStaffDeleteCustomer serves DELETE /api/v1/staff/customers/{id}.
//
// It is a soft close with the same guard as the customer's own, and a mandatory reason: the audit
// row for "an operator closed this record" is only useful if it says why.
func (s *Server) handleStaffDeleteCustomer(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "the database is not configured")
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such customer")
		return
	}
	operator, _ := userFrom(r.Context())

	// DELETE bodies are optional on the wire (a client may send none), so an absent body is
	// distinguished from a malformed one: "a reason is required" is the useful answer, not "the
	// request body is not valid JSON".
	var req struct {
		Reason string `json:"reason"`
	}
	if !s.decodeOptionalJSON(w, r, &req, 8<<10) {
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		writeProblem(w, http.StatusBadRequest, "reason_required",
			"closing a customer record needs a reason; it is recorded in the audit trail")
		return
	}
	if len(reason) > 500 {
		writeProblem(w, http.StatusBadRequest, "invalid_reason", "the reason is too long")
		return
	}

	if err := s.closeCustomer(r.Context(), id, "staff", operator.ID, reason, s.auditorFor(r, reason)); err != nil {
		s.writeClosureError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "closed",
		"customer_id":     id,
		"closed_kind":     "staff",
		"closed_by":       operator.ID,
		"closed_reason":   reason,
		"record_retained": true,
		"statement":       "The record is closed, not deleted: invoices, services and the audit trail are kept.",
	})
}

// ------------------------------------------------------------- the write --

// closeCustomer marks a customer closed and revokes the portal access behind it.
//
// Everything happens in one transaction: the blockers are read under the customer's row lock, so a
// service cannot be created between the check and the close, and the audit row commits or rolls
// back with the change it describes.
func (s *Server) closeCustomer(ctx context.Context, customerID, kind, actorID, reason string,
	auditor audit.Auditor) error {
	return s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			email    string
			closedAt *string
		)
		err := tx.QueryRow(ctx, `
			SELECT email, deleted_at::text FROM customers WHERE id = $1 FOR UPDATE`,
			customerID).Scan(&email, &closedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return errCustomerNotFound
		}
		if err != nil {
			return fmt.Errorf("httpapi: load customer: %w", err)
		}
		if closedAt != nil {
			return errCustomerClosed
		}

		// The same blockers the preflight reported, re-read inside the lock. The preflight is a
		// courtesy; this is the enforcement.
		blockers, err := s.closeBlockers(ctx, tx, customerID)
		if err != nil {
			return err
		}
		if len(blockers) > 0 {
			return closureBlockedError{blockers: blockers}
		}

		// The soft delete. `deletion_kind` says who asked, and `deleted_by` is the operator when
		// there was one — NULL for a customer closing their own account, which is not an omission
		// but the truth: nobody acted on their behalf. The customer's own id is still the *actor*
		// of the audit row below, because "who did this" and "on whose instruction" are different
		// questions and the column answers the second one.
		var deletedBy any
		if kind == "staff" {
			deletedBy = actorID
		}
		if _, err := tx.Exec(ctx, `
			UPDATE customers
			SET deleted_at = now(), deleted_by = $2, deletion_kind = $3, deletion_reason = $4
			WHERE id = $1`,
			customerID, deletedBy, kind, reason); err != nil {
			return fmt.Errorf("httpapi: close customer: %w", err)
		}

		// The logins stop working. A login that is also the portal account of another, still-open
		// customer is left alone: disabling it would close an account nobody asked to close, and
		// that customer would lose access to their own invoices.
		disabled, err := s.disableCustomerLogins(ctx, tx, customerID)
		if err != nil {
			return err
		}

		// The audit trail. `customer.close` for the customer's own act, `customer.delete` for an
		// operator's, because an auditor looking for "who deleted this record" should not have to
		// know that the two share an implementation.
		action := "customer.close"
		if kind == "staff" {
			action = "customer.delete"
		}
		if auditor == nil {
			return nil
		}
		return auditor.RecordAction(ctx, tx, action, "customer", customerID, map[string]any{
			"customer_email":  email,
			"kind":            kind,
			"reason":          reason,
			"actor_id":        actorID,
			"logins_disabled": disabled,
			"soft_delete":     true,
		})
	})
}

// disableCustomerLogins disables the users attached to a customer and ends their sessions.
//
// It returns how many logins it disabled. Two exclusions carry the safety of the whole operation,
// and both are `NOT EXISTS` clauses for the same reason — the *absence* of a row is the fact that
// matters, so it is checked in the same statement that writes:
//
//   - **A login that holds a staff role is never disabled.** A user can be both an operator and
//     the portal account of a customer record (a staff member who is also a customer, and, once
//     every login is given a billing record at creation, every operator). Closing that billing
//     relationship ends the customer's access to the portal; it must not end the person's access
//     to the panel. Without this clause an operator closing an account record would disable their
//     own login and revoke their own sessions — locking themselves out of the product by doing
//     their job. Their sessions are left alone for the same reason.
//
//   - **A login that still owns another open customer record is left alone.** A billing contact
//     can pay for two companies; closing one of them must not lock them out of the other.
func (s *Server) disableCustomerLogins(ctx context.Context, tx pgx.Tx, customerID string) (int, error) {
	rows, err := tx.Query(ctx, `
		UPDATE users u
		SET status = 'disabled', updated_at = now()
		WHERE u.id IN (SELECT ca.user_id FROM customer_accounts ca WHERE ca.customer_id = $1)
		  AND u.status <> 'disabled'
		  AND NOT EXISTS (SELECT 1 FROM staff_roles sr WHERE sr.user_id = u.id)
		  AND NOT EXISTS (
		      SELECT 1
		      FROM customer_accounts other
		      JOIN customers c2 ON c2.id = other.customer_id
		      WHERE other.user_id = u.id AND c2.id <> $1 AND c2.deleted_at IS NULL
		  )
		RETURNING u.id::text`, customerID)
	if err != nil {
		return 0, fmt.Errorf("httpapi: disable customer logins: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("httpapi: scan disabled login: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("httpapi: disable customer logins: %w", err)
	}

	for _, id := range ids {
		// Sessions are revoked explicitly rather than left to the credential version: a disabled
		// account already fails authentication, but a live session row on a closed account makes
		// the session list of a record nobody may use look like one somebody is using.
		if _, err := tx.Exec(ctx, `
			UPDATE sessions SET revoked_at = now()
			WHERE user_id = $1 AND revoked_at IS NULL`, id); err != nil {
			return 0, fmt.Errorf("httpapi: revoke sessions: %w", err)
		}
		// The account's own authentication trail gets the row, so the customer's "why can I not
		// sign in" question is answered from the trail they can see rather than from the
		// operator's audit log.
		if _, err := tx.Exec(ctx, `
			INSERT INTO auth_events (user_id, event, detail)
			VALUES ($1, 'account_closed', 'the customer account was closed')`, id); err != nil {
			return 0, fmt.Errorf("httpapi: record account closure: %w", err)
		}
	}
	return len(ids), nil
}

// ---------------------------------------------------------------- errors --

// writeClosureError maps a closure error to a response.
//
// The refusal is the interesting case: 409 `account_close_blocked` with a detail that names the
// service and the invoice. A generic "that did not work" would leave the customer with a support
// ticket instead of a next step.
func (s *Server) writeClosureError(w http.ResponseWriter, r *http.Request, err error) {
	var blocked closureBlockedError
	switch {
	case errors.As(err, &blocked):
		writeProblem(w, http.StatusConflict, "account_close_blocked",
			"this account cannot be closed while it has "+describeBlockers(blocked.blockers)+
				"; settle or cancel those first, then close the account")
	case errors.Is(err, errCustomerNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such customer")
	case errors.Is(err, errCustomerClosed):
		writeProblem(w, http.StatusConflict, "already_closed", "this account is already closed")
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "account closure failed",
			"path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
	}
}

// decodeOptionalJSON decodes a body when one was sent.
//
// `decodeJSON` answers "the request body is not valid JSON" for an empty body, which is the right
// answer for a POST and the wrong one for a DELETE whose body carries only an optional reason.
// This distinguishes the two so the caller hears "a reason is required" instead.
func (s *Server) decodeOptionalJSON(w http.ResponseWriter, r *http.Request, dst any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_body", "the request body could not be read")
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_body", "the request body is not valid JSON")
		return false
	}
	return true
}
