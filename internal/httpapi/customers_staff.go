package httpapi

// Creating and editing customers from the operator panel.
//
// The panel could list customers and act on them — credit them, impersonate them, edit their
// billing profile — but it could not create one, so a phone order or a manually onboarded account
// needed SQL. These are the two routes that close that, and the properties they are built to keep
// are worth stating before the code:
//
//   - **No response contains a credential.** The account is created with the hash of 32 random
//     bytes that are generated and discarded, so there is nothing to return and nothing to leak.
//     The customer claims the account through the ordinary reset flow, and the invitation mail
//     carries that link.
//   - **A duplicate address is refused**, with `email_taken` and a sentence that says which kind of
//     record already holds it. Self-registration must not enumerate addresses; an authenticated
//     operator who is about to create a record is a different caller and needs to be told.
//   - **The audit row is written inside the transaction** and names the operator, because an
//     account that can be billed but has no password is exactly the record whose provenance gets
//     asked about later.
//   - **Editing a customer never touches their invoices.** An issued document carries a frozen
//     snapshot; correcting an address changes the next document and nothing already sent.
//
// The domain work is in internal/customer, which owns the validation and the row writes. These
// handlers parse, map one error vocabulary onto one set of codes, and queue the invitation after
// the transaction commits.

import (
	"context"
	"errors"
	"net/http"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/customer"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/notify"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("46-customers-staff", registerStaffCustomers)
}

func registerStaffCustomers(r *router) {
	r.staffMutation(http.MethodPost, "/api/v1/staff/customers",
		authn.PermCustomerWriteAll, r.s.handleCreateCustomer)
	r.staffMutation(http.MethodPut, "/api/v1/staff/customers/{id}",
		authn.PermCustomerWriteAll, r.s.handleUpdateCustomer)
}

// customerStore constructs the customer write surface.
//
// It is constructed per request rather than cached on the Server: the value holds only the pool
// and a clock, so there is nothing to reuse, and a field would have meant editing the Server
// struct — a shared file — to add one.
func (s *Server) customerStore() *customer.Store { return customer.New(s.pool) }

// createCustomerRequest is the body of a staff-created customer.
//
// The address is the same shape as the billing-profile endpoints use (`profileBody`), so an
// operator who has learned what `country_code` means once has learned it for every screen.
type createCustomerRequest struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	// BillingAddress is optional. Omitted means no address is stored, which is honest: an empty
	// profile row would claim someone cleared fields they never filled in.
	BillingAddress *profileBody `json:"billing_address"`
	// TaxExempt marks the account exempt, with the reason it is exempt. The reason is required
	// when the flag is set: an exemption without one is a number on a tax return nobody can
	// defend.
	TaxExempt          bool   `json:"tax_exempt"`
	TaxExemptionReason string `json:"tax_exemption_reason"`
}

// updateCustomerRequest is a partial edit. An absent field is left alone, which is what stops a
// rename from clearing an address the operator did not touch.
type updateCustomerRequest struct {
	Name               *string      `json:"name"`
	Email              *string      `json:"email"`
	BillingAddress     *profileBody `json:"billing_address"`
	TaxExempt          *bool        `json:"tax_exempt"`
	TaxExemptionReason *string      `json:"tax_exemption_reason"`
}

// customerJSON renders the record the write endpoints just wrote.
func customerJSON(record customer.Record) map[string]any {
	return map[string]any{
		"id":                   record.CustomerID,
		"email":                record.Email,
		"name":                 record.Name,
		"created_at":           record.CreatedAt.UTC(),
		"has_login":            record.HasLogin,
		"tax_exempt":           record.TaxExempt,
		"tax_exemption_reason": record.TaxExemptionReason,
	}
}

// writeCustomerError maps the write surface's errors onto stable codes.
func (s *Server) writeCustomerError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, customer.ErrInvalidEmail):
		writeProblem(w, http.StatusBadRequest, "invalid_email", "a valid email address is required")
	case errors.Is(err, customer.ErrInvalidName):
		writeProblem(w, http.StatusBadRequest, "invalid_name",
			"a display name between 1 and 200 characters is required")
	case errors.Is(err, customer.ErrEmailTaken):
		writeProblem(w, http.StatusConflict, "email_taken",
			"this address already has a customer record or a login; open that record instead")
	case errors.Is(err, customer.ErrTaxExemptionReason):
		writeProblem(w, http.StatusBadRequest, "tax_exemption_reason_required",
			"a tax exemption needs a reason")
	case errors.Is(err, customer.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such customer")
	case errors.Is(err, billing.ErrInvalidProfile):
		// The message names the field the caller got wrong, which is the only way a form can put
		// the error next to the right input.
		writeProblem(w, http.StatusBadRequest, "invalid_profile", err.Error())
	default:
		s.staffError(w, r, err)
	}
}

// handleCreateCustomer creates a customer, their login and the link between them.
func (s *Server) handleCreateCustomer(w http.ResponseWriter, r *http.Request) {
	var req createCustomerRequest
	if !s.decodeJSON(w, r, &req, 64<<10) {
		return
	}
	user, _ := userFrom(r.Context())

	in := customer.CreateInput{
		Email:              req.Email,
		Name:               req.Name,
		TaxExempt:          req.TaxExempt,
		TaxExemptionReason: req.TaxExemptionReason,
		ActorID:            user.ID,
		CorrelationID:      correlationID(r.Context()),
		IP:                 s.clientIP(r),
	}
	if req.BillingAddress != nil {
		profile := req.BillingAddress.profile("")
		in.Profile = &profile
	}

	var record customer.Record
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		record, err = s.customerStore().Create(ctx, tx, in)
		return err
	})
	if err != nil {
		s.writeCustomerError(w, r, err)
		return
	}

	// The invitation is queued after the account commits, in its own transaction, exactly as
	// registration sends its verification message. A failure here must not fail the creation: the
	// account exists, and the operator can resend from the customer's page. "We did not send it" and
	// "we failed to send it" are different answers, so the response says which.
	sent, detail := s.sendAccountInvitation(r.Context(), record)

	out := map[string]any{
		"customer":        customerJSON(record),
		"invitation_sent": sent,
	}
	if detail != "" {
		out["invitation_detail"] = detail
	}
	if profile, err := billing.LoadProfile(r.Context(), s.pool, record.CustomerID); err == nil {
		out["billing_profile"] = profileResponse(profile)
	}

	s.publish(events.Event{Kind: "customer.created",
		Permission: string(authn.PermCustomerReadAll),
		Data:       map[string]any{"customer_id": record.CustomerID, "email": record.Email}})
	writeJSON(w, http.StatusCreated, out)
}

// sendAccountInvitation issues a reset token and queues the "your account exists" message.
//
// It returns whether the message was queued, and a sentence explaining why not when it was not. It
// never returns the token: the token is the credential, and the only place it may appear is the
// link in the mail sent to the customer's own address.
func (s *Server) sendAccountInvitation(ctx context.Context, record customer.Record) (bool, string) {
	if s.auth == nil || s.auth.Service == nil {
		return false, "authentication is not configured, so no reset link can be issued"
	}
	if s.notify == nil {
		return false, "the notification outbox is not configured"
	}

	res, err := s.auth.Service.RequestPasswordReset(ctx, record.Email)
	if err != nil {
		s.cfg.Logger.ErrorContext(ctx, "could not issue the account invitation",
			"customer_id", record.CustomerID, "error", err)
		return false, "the invitation link could not be issued"
	}
	if res.Token == "" {
		return false, "no reset link was issued for this address"
	}

	in := notify.AccountCreatedInput{
		UserID: record.UserID,
		Email:  record.Email,
		Link:   publicBaseURL() + "/reset?token=" + res.Token,
		// The lifetime is read from authn so the sentence in the mail and the expiry in the
		// database cannot disagree.
		ExpiresIn: authn.PasswordResetLifetime,
		// A prefix, never the token: the outbox must not become a store of working reset links.
		Token: res.Token[:12],
	}
	if err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := notify.AccountCreated(ctx, tx, s.notify, in)
		return err
	}); err != nil {
		s.cfg.Logger.ErrorContext(ctx, "could not queue the account invitation",
			"customer_id", record.CustomerID, "error", err)
		return false, "the invitation could not be queued"
	}
	return true, ""
}

// handleUpdateCustomer edits a customer's name, address, login address and tax exemption.
func (s *Server) handleUpdateCustomer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such customer")
		return
	}
	var req updateCustomerRequest
	if !s.decodeJSON(w, r, &req, 64<<10) {
		return
	}
	user, _ := userFrom(r.Context())

	in := customer.UpdateInput{
		Name:               req.Name,
		Email:              req.Email,
		TaxExempt:          req.TaxExempt,
		TaxExemptionReason: req.TaxExemptionReason,
		ActorID:            user.ID,
		CorrelationID:      correlationID(r.Context()),
		IP:                 s.clientIP(r),
	}
	if req.BillingAddress != nil {
		profile := req.BillingAddress.profile("")
		in.Profile = &profile
	}

	var record customer.Record
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		record, err = s.customerStore().Update(ctx, tx, id, in)
		return err
	})
	if err != nil {
		s.writeCustomerError(w, r, err)
		return
	}

	out := map[string]any{"customer": customerJSON(record)}
	if profile, err := billing.LoadProfile(r.Context(), s.pool, record.CustomerID); err == nil {
		out["billing_profile"] = profileResponse(profile)
	}

	s.publish(events.Event{Kind: "customer.updated",
		Permission: string(authn.PermCustomerReadAll),
		Data:       map[string]any{"customer_id": record.CustomerID}})
	writeJSON(w, http.StatusOK, out)
}
