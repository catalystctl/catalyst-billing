package tax

// VAT-ID validation against a service, and the history of the checks.
//
// Reverse charge is a claim that a check happened. This file is where the check happens and where
// the answer is kept, and three distinctions inside it are load-bearing:
//
//   - **A shape check is not a validation.** `NormalizeVATID` rejects a string that is not even
//     the format its state publishes, which is a fact about the string. It cannot establish that
//     the identifier belongs to a business, so it never grants reverse charge on its own.
//
//   - **"Invalid" and "unavailable" are different answers.** The service saying no means the
//     customer is a consumer for this sale, and the sale is priced at the destination rate. The
//     service not answering means nobody knows, and granting a zero rate on that would be a guess
//     on a document. So an unavailable check refuses the sale instead (see applyReverseCharge).
//
//   - **The answer is stored, not cached in a variable.** An invoice from last year must be
//     explainable by the check that justified its zero rate. `vat_id_validations` is append-only
//     (a trigger refuses UPDATE and DELETE), and the freshness window below only decides when to
//     ask again; it never rewrites what was asked before.
//
// The only service this code speaks to is VIES, because it is the official EU service with a
// published schema. Inventing a vendor-neutral "VAT API" would mean an adapter that silently
// imports nothing — the failure mode the tax importer's package comment rejects as well. An
// installation that validates against a different service needs an adapter implementing
// VATValidator, which is a code change with a test, not a configuration string.

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The three answers a validation can have. They are stored, so they are part of the schema
// (`vat_id_validations.status`).
const (
	// VATStatusValid is a confirmed identifier.
	VATStatusValid = "valid"
	// VATStatusInvalid is a refused identifier: the service was asked and said no.
	VATStatusInvalid = "invalid"
	// VATStatusUnavailable is "no answer": no service configured, a transport failure, a timeout,
	// or a malformed response. It must never be treated as either of the other two.
	VATStatusUnavailable = "unavailable"
)

// DefaultVATValidationTimeout bounds one call to the validation service. A cart render must not
// hang on a slow authority; a refusal is a better answer than a hung request.
const DefaultVATValidationTimeout = 8 * time.Second

// VATValidation is one check, as stored.
type VATValidation struct {
	CountryCode string    `json:"country_code"`
	VATID       string    `json:"vat_id"`
	Status      string    `json:"status"`
	TraderName  string    `json:"trader_name,omitempty"`
	ServiceURL  string    `json:"service_url,omitempty"`
	Detail      string    `json:"detail,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
	// Cached reports whether this answer came from the stored history rather than from a call.
	Cached bool `json:"cached"`
}

// ValidationUnavailable is the transport failure a caller may want to distinguish from a database
// error: it is a "we asked and got nothing" and it is recorded before it is returned.
type ValidationUnavailable struct {
	Detail string
}

func (e *ValidationUnavailable) Error() string {
	return "tax: VAT-ID validation service unavailable: " + e.Detail
}

// VATValidator asks a service whether an identifier is registered.
type VATValidator interface {
	// Validate returns the service's answer. A transport failure is an error; a *negative answer*
	// is a validation with Status invalid and is not an error.
	Validate(ctx context.Context, country, number string) (VATValidation, error)
	// Name is recorded as the service URL on the validation row.
	Name() string
}

// VIESValidator speaks the European Commission's VIES SOAP service.
//
// The message shape is fixed by the service, so it is spelled out here rather than derived from a
// WSDL tool: a checkout path should not depend on a generated client whose regeneration is a build
// risk. The fields read are exactly the ones the answer carries.
type VIESValidator struct {
	// URL defaults to the official endpoint when empty.
	URL string
	// Client defaults to a client with DefaultVATValidationTimeout.
	Client *http.Client
}

// DefaultVIESURL is the documented VIES endpoint.
const DefaultVIESURL = "https://ec.europa.eu/taxation_customs/vies/services/checkVatService"

// Name implements VATValidator.
func (v VIESValidator) Name() string {
	if strings.TrimSpace(v.URL) == "" {
		return DefaultVIESURL
	}
	return strings.TrimSpace(v.URL)
}

const viesEnvelope = `<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:urn="urn:ec.europa.eu:taxud:vies:services:checkVat:types">` +
	`<soapenv:Header/><soapenv:Body><urn:checkVat>` +
	`<urn:countryCode>%s</urn:countryCode><urn:vatNumber>%s</urn:vatNumber>` +
	`</urn:checkVat></soapenv:Body></soapenv:Envelope>`

// viesResponse is the part of the SOAP body this code reads. A fault and a response are mutually
// exclusive, so one struct with both is safe and keeps the decode a single pass.
type viesResponse struct {
	XMLName xml.Name
	// Fault is present when the service refused the request (INVALID_INPUT, SERVICE_UNAVAILABLE,
	// MS_UNAVAILABLE, ...). It is not an answer about the identifier.
	Fault *struct {
		Code   string `xml:"faultcode"`
		String string `xml:"faultstring"`
	} `xml:"Body>Fault"`
	Response *struct {
		CountryCode string `xml:"countryCode"`
		VATNumber   string `xml:"vatNumber"`
		Valid       string `xml:"valid"`
		Name        string `xml:"name"`
	} `xml:"Body>checkVatResponse"`
}

// Validate implements VATValidator.
func (v VIESValidator) Validate(ctx context.Context, country, number string) (VATValidation, error) {
	url := v.Name()
	body := fmt.Sprintf(viesEnvelope, country, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(body)))
	if err != nil {
		return VATValidation{}, fmt.Errorf("tax: build VAT-ID validation request: %w", err)
	}
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	req.Header.Set("SOAPAction", "checkVat")
	req.Header.Set("User-Agent", "catalyst-billing/tax")

	client := v.Client
	if client == nil {
		client = &http.Client{Timeout: DefaultVATValidationTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return VATValidation{}, fmt.Errorf("tax: call VAT-ID validation service: %w", err)
	}
	defer resp.Body.Close()
	// A SOAP fault is served with 500 and still carries a body worth reading, so the status is
	// not rejected here; the body decides.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return VATValidation{}, fmt.Errorf("tax: read VAT-ID validation response: %w", err)
	}

	validation := VATValidation{
		CountryCode: country,
		VATID:       number,
		ServiceURL:  url,
		CheckedAt:   time.Now().UTC(),
	}
	var decoded viesResponse
	if err := xml.Unmarshal(payload, &decoded); err != nil {
		return VATValidation{}, fmt.Errorf("tax: parse VAT-ID validation response (HTTP %d): %w", resp.StatusCode, err)
	}
	if decoded.Fault != nil {
		// Deliberately an error and not an "invalid" answer: the service did not evaluate the
		// identifier, so the only honest summary is "no answer".
		return VATValidation{}, fmt.Errorf("tax: VAT-ID validation service fault: %s %s",
			decoded.Fault.Code, decoded.Fault.String)
	}
	if decoded.Response == nil {
		return VATValidation{}, fmt.Errorf("tax: VAT-ID validation response has no checkVatResponse (HTTP %d)", resp.StatusCode)
	}
	switch strings.ToLower(strings.TrimSpace(decoded.Response.Valid)) {
	case "true":
		validation.Status = VATStatusValid
	case "false":
		validation.Status = VATStatusInvalid
	default:
		return VATValidation{}, fmt.Errorf("tax: VAT-ID validation response has no valid flag")
	}
	validation.TraderName = strings.TrimSpace(decoded.Response.Name)
	return validation, nil
}

// ValidateVATID returns the current answer for an identifier: the stored check when it is still
// inside the freshness window, otherwise a fresh check that is recorded before it is returned.
//
// The recording happens whatever the answer. "We asked on the 3rd and it was unavailable" is the
// fact that explains an invoice refused that day, and a history that only kept successes would not
// explain it.
func (r *Rules) ValidateVATID(ctx context.Context, customerID, country, number string, cfg Config) (VATValidation, error) {
	if cfg.VATValidationTTL <= 0 {
		cfg.VATValidationTTL = 24 * time.Hour
	}
	now := r.now()
	cached, err := r.recentValidation(ctx, country, number, now.Add(-cfg.VATValidationTTL))
	if err != nil {
		return VATValidation{}, err
	}
	if cached != nil {
		return *cached, nil
	}

	validator := r.Validator
	if validator == nil {
		if strings.TrimSpace(cfg.VATValidationURL) == "" {
			// No service and no answer. Recording the row is what lets the refusal be explained;
			// the caller turns the status into a refusal with the missing setting named.
			validation := VATValidation{
				CountryCode: country, VATID: number, Status: VATStatusUnavailable,
				Detail: "no VAT-ID validation service is configured", CheckedAt: now,
			}
			if err := r.recordValidation(ctx, customerID, validation); err != nil {
				return VATValidation{}, err
			}
			return validation, nil
		}
		validator = VIESValidator{URL: cfg.VATValidationURL}
	}

	validation, callErr := validator.Validate(ctx, country, number)
	if callErr != nil {
		validation = VATValidation{
			CountryCode: country, VATID: number, Status: VATStatusUnavailable,
			ServiceURL: validator.Name(), Detail: callErr.Error(), CheckedAt: r.now(),
		}
	}
	if validation.ServiceURL == "" {
		validation.ServiceURL = validator.Name()
	}
	if validation.CountryCode == "" {
		validation.CountryCode = country
	}
	if validation.VATID == "" {
		validation.VATID = number
	}
	// The rules clock decides freshness, not the service's: a validator that stamps its own
	// response with wall-clock time would make a test, a replay or a clock skew look like a cache
	// hit. The service's request date is information; this is the fact we store.
	validation.CheckedAt = r.now()
	if err := r.recordValidation(ctx, customerID, validation); err != nil {
		return VATValidation{}, err
	}
	if callErr != nil && validation.Status == VATStatusUnavailable {
		// Returned alongside the recorded row: the caller refuses, and the detail it shows is the
		// service's own words rather than a generic "validation failed".
		return validation, &ValidationUnavailable{Detail: validation.Detail}
	}
	return validation, nil
}

// recentValidation reads the newest stored check for an identifier, when it is at or after
// `since`.
func (r *Rules) recentValidation(ctx context.Context, country, number string, since time.Time) (*VATValidation, error) {
	var (
		v  VATValidation
		id string
	)
	err := r.q.QueryRow(ctx, `
		SELECT id, country_code, vat_id, status, trader_name, service_url, detail, checked_at
		  FROM vat_id_validations
		 WHERE country_code = $1 AND vat_id = $2 AND checked_at >= $3
		 ORDER BY checked_at DESC
		 LIMIT 1`, country, number, since).
		Scan(&id, &v.CountryCode, &v.VATID, &v.Status, &v.TraderName, &v.ServiceURL, &v.Detail, &v.CheckedAt)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("tax: read VAT-ID validation history: %w", err)
	}
	v.Cached = true
	return &v, nil
}

// recordValidation appends one check. It never updates: the table's trigger refuses that, because
// an issued invoice's reverse-charge treatment points at this row.
func (r *Rules) recordValidation(ctx context.Context, customerID string, v VATValidation) error {
	if v.CheckedAt.IsZero() {
		v.CheckedAt = r.now()
	}
	_, err := r.q.Exec(ctx, `
		INSERT INTO vat_id_validations
			(country_code, vat_id, status, trader_name, service_url, detail, customer_id, checked_at)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, '')::uuid, $8)`,
		v.CountryCode, v.VATID, v.Status, v.TraderName, v.ServiceURL, v.Detail, customerID, v.CheckedAt)
	if err != nil {
		return fmt.Errorf("tax: record VAT-ID validation: %w", err)
	}
	return nil
}

// ValidationHistory lists an identifier's checks, newest first, for the screen that has to explain
// a reverse-charge decision.
func (r *Rules) ValidationHistory(ctx context.Context, country, number string, limit int) ([]VATValidation, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := r.pool.Query(ctx, `
		SELECT country_code, vat_id, status, trader_name, service_url, detail, checked_at
		  FROM vat_id_validations
		 WHERE country_code = $1 AND vat_id = $2
		 ORDER BY checked_at DESC
		 LIMIT $3`, country, number, limit)
	if err != nil {
		return nil, fmt.Errorf("tax: read VAT-ID validation history: %w", err)
	}
	defer rows.Close()
	out := make([]VATValidation, 0, limit)
	for rows.Next() {
		var v VATValidation
		if err := rows.Scan(&v.CountryCode, &v.VATID, &v.Status, &v.TraderName, &v.ServiceURL,
			&v.Detail, &v.CheckedAt); err != nil {
			return nil, fmt.Errorf("tax: scan VAT-ID validation: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
