package notify

// The activation message (SF-11).
//
// A service becoming live is the one moment a customer needs to be told something the panel
// cannot say generically: how to reach the thing they bought. The catalog has carried a
// per-product `email_template` column since 0020 and nothing read it, so every activation mail
// said the same sentence regardless of whether the product was a game server with a port, a
// panel login, or a domain whose nameservers have to be changed.
//
// # Why the loader lives here and not in the caller
//
// The event functions in events.go take their variables from the caller, deliberately: the
// caller is the one that knows the invoice number, the amount and the route shape. The
// activation event differs in one respect — the facts it needs are all in three tables the
// provisioning worker already holds a transaction open on, and the *product snippet* is the part
// that is easy to forget. A worker that passed ServiceName but not Instructions would compile,
// send, and produce exactly the generic mail this work exists to replace.
//
// So the call site is one line and the query — service to product to customer — is written once,
// next to the template that consumes it:
//
//	notify.ServiceActivated(ctx, tx, w.notify, op.ServiceID)
//
// # Which event it is
//
// `new_server_created`, the event this panel already catalogues as "server created". It is not a
// second activation event: two events that both mean "your service is ready" would be two
// templates an operator has to keep in step, and one of them would end up stale.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ServiceActivated announces that a service is live, with the product's own instructions.
//
// It reads the service, its product and its customer inside tx, so the message is written in the
// same transaction as the status change that made it true. A service that does not exist is not
// an error: the row may have been removed between the operation and this call, and a customer who
// no longer has the service does not need an activation mail about it.
//
// A missing *product* is also tolerated, in the one way that matters: a service row whose product
// was deleted cannot exist (services.product_id is a foreign key), so ErrNoRows here always means
// the service itself is gone.
func ServiceActivated(ctx context.Context, tx pgx.Tx, queue *Queue, serviceID string) (Outcome, error) {
	res := Outcome{EventKey: EventNewServerCreated}
	if serviceID == "" {
		return res, errors.New("notify: activation has no service")
	}

	var (
		email        string
		externalID   string
		productName  string
		instructions string
		userID       *string
	)
	err := tx.QueryRow(ctx, `
		SELECT c.email, COALESCE(s.external_id, ''), p.name, p.email_template,
		       (SELECT ca.user_id::text FROM customer_accounts ca
		         WHERE ca.customer_id = c.id
		         ORDER BY ca.created_at
		         LIMIT 1)
		  FROM services s
		  JOIN customers c ON c.id = s.customer_id
		  JOIN products  p ON p.id = s.product_id
		 WHERE s.id = $1`, serviceID).
		Scan(&email, &externalID, &productName, &instructions, &userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("notify: read activation: %w", err)
	}

	in := ServerCreatedInput{
		Email: email,
		// ServiceName is the *product* name: it is what the customer bought and what they will
		// recognise in the subject line. The customer's own name is not used for it — a mail
		// whose subject is the recipient's name reads like phishing.
		ServiceID:    serviceID,
		ServiceName:  productName,
		ServerName:   orDefault(externalID, productName),
		Instructions: instructions,
	}
	if userID != nil {
		in.UserID = *userID
	}

	out, err := ServerCreated(ctx, tx, queue, in)
	if err != nil {
		return out, fmt.Errorf("notify: service activation: %w", err)
	}
	return out, nil
}
