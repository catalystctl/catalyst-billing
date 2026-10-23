package httpapi

// Saved payment methods and billing agreements over HTTP (PAY-10, PAY-11, PAY-13).
//
// The domain rules live in internal/payments: one default per customer, a token saved twice
// is one method, a customer-scoped query that answers 404 for someone else's id, and the
// off-session charge that goes through the same settlement path as a webhook. This file is
// the boundary and nothing more.
//
// Three properties are enforced here rather than in the domain:
//
//   - **The token never leaves.** `payments.Method` has no exported token field, and every
//     response below is built by methodJSON, which names each field explicitly. The tests
//     assert on the raw response bytes, so a field added later cannot quietly leak.
//   - **A saved method has a mandate.** Saving writes the billing agreement in the same
//     transaction; a charge refuses a method with no active agreement. "Why were we allowed
//     to charge this card" is a row, not a convention.
//   - **A decline is honest.** The invoice stays open, the method records the failure, and
//     dunning escalates it. Nothing here marks an invoice paid on the strength of a request
//     it could not make, and nothing here suspends a service directly.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/fulfilment"
	"github.com/catalyst/billing/internal/gatewayconfig"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/portal"
)

func init() {
	RegisterRoutes("39-payment-methods", registerPaymentMethods)
}

// savedGateway is the extension surface a saved method needs. It is an interface so the
// HTTP tests can drive the whole boundary without launching a child process; production
// uses *extproto.Client, which already has these three methods.
type SavedGateway interface {
	Call(ctx context.Context, method string, params, result any) error
	Info() extproto.InitializeResult
	Close() error
}

// OpenSavedGateway launches the configured extension. It is a variable so a test can
// replace it and so the production path is the same gatewayconfig.Load + Launch the
// checkout and worker paths use; a second way to start the adapter would eventually be
// configured differently.
var OpenSavedGateway = func(ctx context.Context, s *Server) (SavedGateway, string, error) {
	if s.pool == nil {
		return nil, "", gatewayconfig.ErrNotConfigured
	}
	cfg, err := gatewayconfig.New(s.pool).Load(ctx, gatewayconfig.GatewayKind)
	if err != nil {
		return nil, "", err
	}
	if !cfg.Configured() {
		return nil, "", gatewayconfig.ErrNotConfigured
	}
	client, err := gatewayconfig.Launch(ctx, cfg, 30*time.Second)
	if err != nil {
		return nil, "", err
	}
	return client, cfg.Provider, nil
}

// offSessionWiring is how cmd/billing may attach a long-lived adapter. It is optional: the
// charge handlers launch the configured extension themselves, so the feature works without
// a second wiring change in a package this work does not own. Tests use the same seam.
type offSessionWiring struct {
	charger payments.OffSessionCharger
	applier payments.Applier
	account string
}

var (
	offSessionMu sync.RWMutex
	offSession   offSessionWiring
)

// WithOffSessionCharging attaches the adapter that can charge a saved method and the
// settlement path it must use. The applier is the same one the webhook processor uses.
func (s *Server) WithOffSessionCharging(c payments.OffSessionCharger, app payments.Applier, account string) *Server {
	offSessionMu.Lock()
	offSession = offSessionWiring{charger: c, applier: app, account: account}
	offSessionMu.Unlock()
	return s
}

func currentOffSession() offSessionWiring {
	offSessionMu.RLock()
	defer offSessionMu.RUnlock()
	return offSession
}

func registerPaymentMethods(r *router) {
	r.get("/api/v1/portal/payment-methods/saved",
		authn.PermPaymentMethodReadSelf, r.s.handleListSavedMethods)
	r.mutation(http.MethodPost, "/api/v1/portal/payment-methods/saved",
		authn.PermPaymentMethodWriteSelf, r.s.handleSavePaymentMethod)
	r.get("/api/v1/portal/payment-methods/saved/{id}",
		authn.PermPaymentMethodReadSelf, r.s.handleGetSavedMethod)
	r.mutation(http.MethodPost, "/api/v1/portal/payment-methods/saved/{id}/default",
		authn.PermPaymentMethodWriteSelf, r.s.handleSetDefaultMethod)
	r.mutation(http.MethodDelete, "/api/v1/portal/payment-methods/saved/{id}",
		authn.PermPaymentMethodWriteSelf, r.s.handleRemoveMethod)

	// The hosted mandate flow. The customer adds a card without this panel ever seeing the
	// number: the provider's page collects it and the adapter reports a reusable token.
	r.mutation(http.MethodPost, "/api/v1/portal/payment-methods/agreements",
		authn.PermPaymentMethodWriteSelf, r.s.handleStartAgreement)
	r.get("/api/v1/portal/payment-methods/agreements/{id}",
		authn.PermPaymentMethodReadSelf, r.s.handleGetAgreement)
	r.mutation(http.MethodDelete, "/api/v1/portal/payment-methods/agreements/{id}",
		authn.PermPaymentMethodWriteSelf, r.s.handleCancelAgreement)

	// Staff visibility and the two charges an operator/cron needs. The per-invoice action is
	// the useful one; the sweep is the same ChargeDue a worker would call.
	r.staffGet("/api/v1/staff/customers/{id}/payment-methods",
		authn.PermCustomerReadAll, r.s.handleStaffCustomerMethods)
	r.staffMutation(http.MethodPost, "/api/v1/staff/invoices/{id}/charge-saved-method",
		authn.PermPaymentReconcile, r.s.handleStaffChargeInvoice)
	r.staffMutation(http.MethodPost, "/api/v1/staff/payments/charge-due",
		authn.PermPaymentReconcile, r.s.handleStaffChargeDue)
}

// agreementsFor builds the domain service for one request, with the request's auditor so
// the mandate audit row names the customer who gave it.
func (s *Server) agreementsFor(r *http.Request) *payments.Agreements {
	svc := payments.NewAgreements(s.pool).WithAuditor(s.auditorFor(r, "")).WithNotifier(s.notify)
	if w := currentOffSession(); w.charger != nil {
		svc = svc.WithCharger(w.charger).WithApplier(w.applier).WithAccount(w.account)
	}
	return svc
}

func (s *Server) savedMethodProvider() string {
	if s.gateway != nil {
		return s.gateway.Name()
	}
	return s.provider
}

// methodJSON is the one wire shape of a saved method.
//
// The provider token is deliberately absent: it is named nowhere, so it cannot be added by
// marshalling a struct that grows a field later.
func methodJSON(m payments.Method) map[string]any {
	out := map[string]any{
		"id":         m.ID,
		"provider":   m.Provider,
		"brand":      m.Brand,
		"last4":      m.Last4,
		"label":      m.Label,
		"default":    m.IsDefault,
		"display":    methodDisplay(m),
		"created_at": m.CreatedAt.UTC(),
		"updated_at": m.UpdatedAt.UTC(),
	}
	if m.ExpiryMonth != nil && m.ExpiryYear != nil {
		out["expiry_month"] = *m.ExpiryMonth
		out["expiry_year"] = *m.ExpiryYear
	}
	if m.LastUsedAt != nil {
		out["last_used_at"] = m.LastUsedAt.UTC()
	}
	if m.LastFailureAt != nil {
		out["last_failure_at"] = m.LastFailureAt.UTC()
	}
	if m.LastFailureCode != "" {
		out["last_failure_code"] = m.LastFailureCode
	}
	return out
}

func methodDisplay(m payments.Method) string {
	brand := strings.TrimSpace(m.Brand)
	if brand != "" {
		brand = strings.ToUpper(brand[:1]) + brand[1:]
	}
	switch {
	case brand != "" && m.Last4 != "":
		return brand + " ending " + m.Last4
	case m.Last4 != "":
		return "Card ending " + m.Last4
	case brand != "":
		return brand
	}
	return "Saved payment method"
}

func agreementJSON(a payments.BillingAgreement) map[string]any {
	out := map[string]any{
		"id":                    a.ID,
		"provider":              a.Provider,
		"status":                a.Status,
		"payment_method_id":     a.PaymentMethodID,
		"provider_agreement_id": a.ProviderAgreementID,
		"terms_version":         a.TermsVersion,
		"created_at":            a.CreatedAt.UTC(),
		"updated_at":            a.UpdatedAt.UTC(),
	}
	if a.ConsentAt != nil {
		out["consent_at"] = a.ConsentAt.UTC()
	}
	if a.CancelledAt != nil {
		out["cancelled_at"] = a.CancelledAt.UTC()
	}
	return out
}

// savedMethodsJSON returns the methods plus the policy the screen explains. Whether this
// installation may charge automatically is a deployment fact, so it travels with the rows.
func (s *Server) savedMethodsJSON(r *http.Request, methods []payments.Method) map[string]any {
	rows := make([]map[string]any, 0, len(methods))
	hasDefault := false
	for _, m := range methods {
		rows = append(rows, methodJSON(m))
		if m.IsDefault {
			hasDefault = true
		}
	}
	enabled, reason := s.canChargeOffSession(r.Context())
	out := map[string]any{
		"methods":     rows,
		"has_default": hasDefault,
		"off_session": map[string]any{
			"enabled": enabled,
			"reason":  reason,
		},
	}
	if provider := s.savedMethodProvider(); provider != "" {
		out["off_session"].(map[string]any)["provider"] = provider
	}
	return out
}

// canChargeOffSession answers whether the customer should be promised an automatic charge.
//
// A wired charger is trusted without a launch. Otherwise the configured extension is asked
// for its handshake capability, which is the only honest answer: a gateway that cannot
// charge must not be described as one that will.
func (s *Server) canChargeOffSession(ctx context.Context) (bool, string) {
	svc := payments.NewAgreements(s.pool)
	enabled, err := svc.OffSessionEnabled(ctx)
	if err != nil {
		return false, "the off-session setting could not be read"
	}
	if !enabled {
		return false, "charging a saved method automatically is turned off"
	}
	if w := currentOffSession(); w.charger != nil {
		if w.applier == nil || w.account == "" {
			return false, "off-session charging is not wired on this installation"
		}
		return true, ""
	}
	if s.account == "" {
		return false, "off-session charging is not wired on this installation"
	}
	gw, _, err := OpenSavedGateway(ctx, s)
	if err != nil {
		return false, "the payment gateway is not available"
	}
	defer func() { _ = gw.Close() }()
	if !gw.Info().HasCapability(payments.CapGatewayCharge) {
		return false, "the configured payment gateway cannot charge a saved method"
	}
	return true, ""
}

// handleListSavedMethods returns the caller's saved methods.
func (s *Server) handleListSavedMethods(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	customerID, ok := s.customerFor(w, r)
	if !ok {
		return
	}
	methods, err := s.agreementsFor(r).List(r.Context(), customerID)
	if err != nil {
		s.paymentMethodError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.savedMethodsJSON(r, methods))
}

// saveMethodBody is the request shape. There is no number, cvc or expiry-date string: the
// token comes from the provider, and the type not having a field for a PAN is the cheapest
// way to keep this panel out of card-data scope.
type saveMethodBody struct {
	Token       string `json:"token"`
	Brand       string `json:"brand"`
	Last4       string `json:"last4"`
	Label       string `json:"label"`
	ExpiryMonth *int   `json:"expiry_month"`
	ExpiryYear  *int   `json:"expiry_year"`
	Default     bool   `json:"default"`
	// TermsVersion is the version of the recurring-payment terms the customer accepted.
	TermsVersion string `json:"terms_version"`
}

func (s *Server) handleSavePaymentMethod(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	customerID, ok := s.customerFor(w, r)
	if !ok {
		return
	}
	provider := s.savedMethodProvider()
	if provider == "" {
		writeProblem(w, http.StatusServiceUnavailable, "gateway_unavailable",
			"no payment gateway is configured, so there is nothing to save a method for")
		return
	}
	var body saveMethodBody
	if !s.decodeJSON(w, r, &body, 16<<10) {
		return
	}
	now := s.now()
	m, err := s.agreementsFor(r).Save(r.Context(), payments.SaveMethodRequest{
		CustomerID:       customerID,
		Provider:         provider,
		Token:            body.Token,
		Brand:            body.Brand,
		Last4:            body.Last4,
		Label:            body.Label,
		ExpiryMonth:      body.ExpiryMonth,
		ExpiryYear:       body.ExpiryYear,
		MakeDefault:      body.Default,
		ConsentAt:        &now,
		ConsentIP:        s.clientIP(r),
		ConsentUserAgent: r.UserAgent(),
		TermsVersion:     body.TermsVersion,
	})
	if err != nil {
		s.paymentMethodError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, methodJSON(m))
}

func (s *Server) handleGetSavedMethod(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	customerID, ok := s.customerFor(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	m, err := s.agreementsFor(r).Get(r.Context(), customerID, id)
	if err != nil {
		s.paymentMethodError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, methodJSON(m))
}

func (s *Server) handleSetDefaultMethod(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	customerID, ok := s.customerFor(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	m, err := s.agreementsFor(r).SetDefault(r.Context(), customerID, id)
	if err != nil {
		s.paymentMethodError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, methodJSON(m))
}

func (s *Server) handleRemoveMethod(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	customerID, ok := s.customerFor(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	svc := s.agreementsFor(r)
	m, err := svc.Get(r.Context(), customerID, id)
	if err != nil {
		s.paymentMethodError(w, r, err)
		return
	}
	agreements, _ := svc.ListAgreements(r.Context(), customerID)
	providerRef := ""
	for _, ag := range agreements {
		if ag.PaymentMethodID == id && ag.Status == "active" {
			providerRef = ag.ProviderAgreementID
			break
		}
	}

	if err := svc.Remove(r.Context(), customerID, id); err != nil {
		s.paymentMethodError(w, r, err)
		return
	}

	// The local row is gone, so no charge can use it. Detaching at the provider is hygiene,
	// not correctness: a provider that is unreachable must not turn removing a card into an
	// error the customer cannot get past.
	if gw, _, err := OpenSavedGateway(r.Context(), s); err == nil {
		var out payments.CancelBillingAgreementResult
		_ = gw.Call(r.Context(), payments.MethodGatewayCancelBillingAgreement,
			&payments.CancelBillingAgreementParams{
				OperationKey: "method-remove:" + id,
				AgreementID:  providerRef,
				MethodToken:  m.Token(),
			}, &out)
		_ = gw.Close()
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleStartAgreement begins the provider's hosted flow. The local row is created first so
// the success URL can name it; the provider reference is filled in afterwards.
func (s *Server) handleStartAgreement(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	customerID, ok := s.customerFor(w, r)
	if !ok {
		return
	}
	provider := s.savedMethodProvider()
	if provider == "" {
		writeProblem(w, http.StatusServiceUnavailable, "gateway_unavailable", "no payment gateway is configured")
		return
	}
	svc := s.agreementsFor(r)
	ag, err := svc.CreatePendingAgreement(r.Context(), payments.AgreementRequest{
		CustomerID:       customerID,
		Provider:         provider,
		ConsentIP:        s.clientIP(r),
		ConsentUserAgent: r.UserAgent(),
	})
	if err != nil {
		s.paymentMethodError(w, r, err)
		return
	}

	success := s.checkoutSuccessURL
	if success == "" {
		success = "/portal/payment-methods"
	}
	sep := "?"
	if strings.Contains(success, "?") {
		sep = "&"
	}
	success += sep + "agreement=" + ag.ID

	gw, _, err := OpenSavedGateway(r.Context(), s)
	if err != nil {
		_ = svc.CancelAgreement(r.Context(), customerID, ag.ID)
		s.cfg.Logger.ErrorContext(r.Context(), "billing agreement launch failed", "error", err)
		writeProblem(w, http.StatusServiceUnavailable, "gateway_unavailable", "the payment gateway is unavailable")
		return
	}
	defer func() { _ = gw.Close() }()
	var res payments.CreateBillingAgreementResult
	err = gw.Call(r.Context(), payments.MethodGatewayCreateBillingAgreement,
		&payments.CreateBillingAgreementParams{
			OperationKey: "agreement:" + ag.ID,
			CustomerID:   customerID,
			SuccessURL:   success,
			CancelURL:    s.checkoutCancelURL,
			Metadata:     map[string]string{"agreement_id": ag.ID},
		}, &res)
	if err != nil {
		_ = svc.CancelAgreement(r.Context(), customerID, ag.ID)
		s.cfg.Logger.ErrorContext(r.Context(), "create billing agreement failed", "error", err)
		writeProblem(w, http.StatusServiceUnavailable, "gateway_unavailable", "the payment gateway is unavailable")
		return
	}
	if _, err := svc.AttachProviderReference(r.Context(), customerID, ag.ID, res.AgreementID, ""); err != nil {
		s.paymentMethodError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"agreement_id": ag.ID,
		"redirect_url": res.CheckoutURL,
		"expires_at":   res.ExpiresAt.UTC(),
	})
}

// handleGetAgreement reads the provider's answer and, once confirmed, saves the method it
// produced. The token travels from the adapter into the domain's unexported field and is
// never part of this response.
func (s *Server) handleGetAgreement(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	customerID, ok := s.customerFor(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	svc := s.agreementsFor(r)
	ag, err := svc.GetAgreement(r.Context(), customerID, id)
	if err != nil {
		s.paymentMethodError(w, r, err)
		return
	}
	if ag.Status == "active" && ag.PaymentMethodID != "" {
		m, err := svc.Get(r.Context(), customerID, ag.PaymentMethodID)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"agreement": agreementJSON(ag), "method": methodJSON(m)})
			return
		}
	}
	if ag.ProviderAgreementID == "" {
		writeJSON(w, http.StatusOK, map[string]any{"agreement": agreementJSON(ag)})
		return
	}

	gw, provider, err := OpenSavedGateway(r.Context(), s)
	if err != nil {
		writeProblem(w, http.StatusServiceUnavailable, "gateway_unavailable", "the payment gateway is unavailable")
		return
	}
	defer func() { _ = gw.Close() }()
	var res payments.QueryBillingAgreementResult
	if err := gw.Call(r.Context(), payments.MethodGatewayQueryBillingAgreement,
		&payments.QueryBillingAgreementParams{AgreementID: ag.ProviderAgreementID}, &res); err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "query billing agreement failed", "error", err)
		writeProblem(w, http.StatusServiceUnavailable, "gateway_unavailable", "the payment gateway is unavailable")
		return
	}
	if res.Status != "succeeded" || res.MethodToken == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"agreement": agreementJSON(ag),
			"status":    res.Status,
		})
		return
	}
	now := s.now()
	m, err := svc.Save(r.Context(), payments.SaveMethodRequest{
		CustomerID:          customerID,
		Provider:            provider,
		Token:               res.MethodToken,
		Brand:               res.Brand,
		Last4:               res.Last4,
		ExpiryMonth:         intPtr(res.ExpiryMonth),
		ExpiryYear:          intPtr(res.ExpiryYear),
		ProviderAgreementID: ag.ProviderAgreementID,
		ProviderCustomerID:  res.ProviderCustomerID,
		ConsentAt:           &now,
		ConsentIP:           s.clientIP(r),
		ConsentUserAgent:    r.UserAgent(),
		TermsVersion:        ag.TermsVersion,
	})
	if err != nil {
		s.paymentMethodError(w, r, err)
		return
	}
	// Re-read so the response carries the activated agreement, not the pending one.
	if fresh, err := svc.GetAgreement(r.Context(), customerID, id); err == nil {
		ag = fresh
	}
	writeJSON(w, http.StatusOK, map[string]any{"agreement": agreementJSON(ag), "method": methodJSON(m)})
}

func (s *Server) handleCancelAgreement(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	customerID, ok := s.customerFor(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	svc := s.agreementsFor(r)
	ag, err := svc.GetAgreement(r.Context(), customerID, id)
	if err != nil {
		s.paymentMethodError(w, r, err)
		return
	}
	if err := svc.CancelAgreement(r.Context(), customerID, id); err != nil {
		s.paymentMethodError(w, r, err)
		return
	}
	// Best effort at the provider: the local row already stops any charge, and a provider
	// that is unreachable must not turn removing a card into an error.
	if ag.ProviderAgreementID != "" {
		if gw, _, err := OpenSavedGateway(r.Context(), s); err == nil {
			var out payments.CancelBillingAgreementResult
			_ = gw.Call(r.Context(), payments.MethodGatewayCancelBillingAgreement,
				&payments.CancelBillingAgreementParams{
					OperationKey: "agreement-cancel:" + id,
					AgreementID:  ag.ProviderAgreementID,
				}, &out)
			_ = gw.Close()
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleStaffCustomerMethods is the staff-side visibility: the saved methods and the
// mandates behind them, with the same no-token wire shape the customer sees.
func (s *Server) handleStaffCustomerMethods(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	customerID := r.PathValue("id")
	if !portal.ValidID(customerID) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	svc := s.agreementsFor(r)
	methods, err := svc.List(r.Context(), customerID)
	if err != nil {
		s.paymentMethodError(w, r, err)
		return
	}
	agreements, err := svc.ListAgreements(r.Context(), customerID)
	if err != nil {
		s.paymentMethodError(w, r, err)
		return
	}
	rows := make([]map[string]any, 0, len(methods))
	for _, m := range methods {
		rows = append(rows, methodJSON(m))
	}
	agRows := make([]map[string]any, 0, len(agreements))
	for _, a := range agreements {
		agRows = append(agRows, agreementJSON(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"methods": rows, "agreements": agRows})
}

// chargeOutcomeJSON is the one wire shape of a charge outcome.
func chargeOutcomeJSON(out payments.ChargeOutcome) map[string]any {
	m := map[string]any{
		"invoice_id": out.InvoiceID,
		"status":     string(out.Status),
		"detail":     out.Detail,
	}
	if out.MethodID != "" {
		m["method_id"] = out.MethodID
	}
	if out.PaymentID != "" {
		m["payment_id"] = out.PaymentID
	}
	if out.Amount != nil {
		m["amount"] = amountJSON(*out.Amount)
	}
	return m
}

func (s *Server) chargeService(ctx context.Context) (*payments.Agreements, func(), error) {
	// The notifier is what makes a declined charge reach the customer instead of only the
	// payment_attempts table (NTF-07).
	svc := payments.NewAgreements(s.pool).WithNotifier(s.notify)
	if w := currentOffSession(); w.charger != nil {
		return svc.WithCharger(w.charger).WithApplier(w.applier).WithAccount(w.account), func() {}, nil
	}
	gw, provider, err := OpenSavedGateway(ctx, s)
	if err != nil {
		return nil, nil, err
	}
	next := fulfilment.NewApplier(s.pool)
	if s.notify != nil {
		next = next.WithNotifier(s.notify)
	}
	return svc.WithCharger(payments.ExtensionCharger{Caller: gw, Provider: provider}).
		WithApplier(next).WithAccount(s.account), func() { _ = gw.Close() }, nil
}

func (s *Server) handleStaffChargeInvoice(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	invoiceID := r.PathValue("id")
	if !portal.ValidID(invoiceID) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such invoice")
		return
	}
	svc, closeFn, err := s.chargeService(r.Context())
	if err != nil {
		writeProblem(w, http.StatusServiceUnavailable, "gateway_unavailable", "the payment gateway is unavailable")
		return
	}
	defer closeFn()
	out, err := svc.ChargeInvoice(r.Context(), invoiceID)
	if err != nil && !errors.Is(err, payments.ErrDeclined) {
		s.chargeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, chargeOutcomeJSON(out))
}

func (s *Server) handleStaffChargeDue(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	svc, closeFn, err := s.chargeService(r.Context())
	if err != nil {
		writeProblem(w, http.StatusServiceUnavailable, "gateway_unavailable", "the payment gateway is unavailable")
		return
	}
	defer closeFn()
	outcomes, err := svc.ChargeDue(r.Context(), 50)
	if err != nil {
		// A decline is an outcome, not a failure of the sweep. A transport or configuration
		// failure is returned, and the outcomes that did run are still reported beside it.
		s.cfg.Logger.ErrorContext(r.Context(), "off-session sweep failed", "error", err)
	}
	rows := make([]map[string]any, 0, len(outcomes))
	for _, out := range outcomes {
		rows = append(rows, chargeOutcomeJSON(out))
	}
	writeJSON(w, http.StatusOK, map[string]any{"outcomes": rows})
}

func (s *Server) chargeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, payments.ErrOffSessionDisabled):
		writeProblem(w, http.StatusConflict, "off_session_disabled",
			"charging a saved method automatically is off for this installation")
	case errors.Is(err, payments.ErrOffSessionUnsupported):
		writeProblem(w, http.StatusConflict, "off_session_unsupported",
			"the configured payment gateway cannot charge a saved method")
	case errors.Is(err, payments.ErrNoAgreement):
		writeProblem(w, http.StatusConflict, "no_agreement",
			"the saved method has no active billing agreement")
	case errors.Is(err, payments.ErrNoDefaultMethod):
		writeProblem(w, http.StatusConflict, "no_default_method",
			"the customer has no default saved method")
	case errors.Is(err, payments.ErrNotChargeable):
		writeProblem(w, http.StatusConflict, "not_chargeable", "the invoice cannot be charged")
	case errors.Is(err, payments.ErrAmountMismatch):
		writeProblem(w, http.StatusConflict, "amount_mismatch",
			"the provider settled an amount that does not match the invoice")
	case errors.Is(err, payments.ErrNotConfigured):
		writeProblem(w, http.StatusServiceUnavailable, "charging_unconfigured",
			"off-session charging is not configured on this installation")
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "charge saved method failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "gateway_error",
			"the payment gateway could not complete the charge")
	}
}

func (s *Server) paymentMethodError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, payments.ErrMethodNotFound), errors.Is(err, payments.ErrAgreementNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
	case errors.Is(err, payments.ErrInvalidMethod), errors.Is(err, payments.ErrInvalidAgreement):
		writeProblem(w, http.StatusBadRequest, "invalid_payment_method", err.Error())
	case errors.Is(err, payments.ErrNoAgreement):
		writeProblem(w, http.StatusConflict, "no_agreement", "the saved method has no active billing agreement")
	case errors.Is(err, payments.ErrOffSessionDisabled):
		writeProblem(w, http.StatusConflict, "off_session_disabled",
			"charging a saved method automatically is turned off for this installation")
	case errors.Is(err, payments.ErrOffSessionUnsupported):
		writeProblem(w, http.StatusConflict, "off_session_unsupported",
			"the configured payment gateway cannot charge a saved method")
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "payment method request failed", "path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error", "the request could not be completed")
	}
}

func intPtr(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}
