package notify

// Service notices whose facts all live in the same three tables.
//
// `ServerSuspended` takes its variables from the caller, like every other event function — but the
// caller that knows a suspension happened is the provisioning worker, which only has the operation
// row. Making it join services → customers → customer_accounts itself would be the third copy of
// that query in this package, and the copy that forgets the owner-account ordering is the one that
// mails the wrong address.
//
// So the loader lives here, next to the event, the way ServiceActivated does:
//
//	notify.ServiceSuspended(ctx, tx, w.notify, op.ServiceID, reason, at)
//
// The alternative — no call site at all — is what the parity audit found: the template existed,
// the catalogue listed it, and no suspension ever produced a mail.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ServiceSuspended tells a customer their service was suspended.
//
// A service that no longer exists is not an error: the row may have been removed between the
// operation and this call, and a customer who no longer holds the service does not need a
// suspension notice about it.
func ServiceSuspended(ctx context.Context, tx pgx.Tx, queue *Queue, serviceID, reason string,
	at time.Time) (Outcome, error) {

	res := Outcome{EventKey: EventServerSuspended}
	if serviceID == "" {
		return res, errors.New("notify: suspension has no service")
	}

	var (
		email       string
		productName string
		userID      *string
	)
	err := tx.QueryRow(ctx, `
		SELECT c.email, p.name,
		       (SELECT ca.user_id::text FROM customer_accounts ca
		         WHERE ca.customer_id = c.id
		         ORDER BY (ca.role = 'owner') DESC, ca.created_at
		         LIMIT 1)
		  FROM services s
		  JOIN customers c ON c.id = s.customer_id
		  JOIN products  p ON p.id = s.product_id
		 WHERE s.id = $1`, serviceID).
		Scan(&email, &productName, &userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("notify: read suspension: %w", err)
	}

	in := ServerSuspendedInput{
		Email:       email,
		ServiceID:   serviceID,
		ServiceName: productName,
		Reason:      reason,
		SuspendedAt: at,
	}
	if userID != nil {
		in.UserID = *userID
	}
	out, err := ServerSuspended(ctx, tx, queue, in)
	if err != nil {
		return out, fmt.Errorf("notify: service suspension: %w", err)
	}
	return out, nil
}
