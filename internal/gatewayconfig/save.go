package gatewayconfig

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/settings"
	"github.com/jackc/pgx/v5"
)

// Errors a caller branches on when writing.
var (
	// ErrEnvOverride is "the environment supplies this value, so saving it would do
	// nothing". Refusing is the point: a form that accepts a value the runtime ignores makes
	// the operator believe a change took effect.
	ErrEnvOverride = errors.New("gatewayconfig: the environment overrides this value")
)

// Save writes configuration and returns the configuration as it now reads.
//
// Everything is one transaction: the settings rows, their audit rows, and the read-back.
// That is what makes the response the committed state rather than a guess, and it is what
// stops a half-saved form being visible to the next reader.
//
// A secret sent as the redaction marker is skipped by the settings store, so "load the page,
// change the provider, save" cannot overwrite a credential with the literal "[redacted]".
// A secret sent as the empty string *does* clear it, which is how an operator removes a key
// they rotated out.
func (r *Resolver) Save(ctx context.Context, kind Kind, values map[string]string,
	actorID string, auditor audit.Auditor) (Config, error) {
	current, err := r.Load(ctx, kind)
	if err != nil {
		return Config{}, err
	}
	group := groupFor(kind)

	// Refuse a write the environment would ignore, before writing any of the group: a bulk
	// save that fails on its last key must not leave the first nine applied.
	for key := range values {
		if err := r.checkEditable(kind, current, key); err != nil {
			return Config{}, err
		}
	}

	// A provider must be one this build can launch. An unknown one would otherwise be stored
	// and then fail every payment with "no extension binary", which is a worse error at a
	// worse moment.
	if raw, ok := values["provider"]; ok {
		provider := strings.TrimSpace(raw)
		if provider != "" {
			if _, known := providerFor(kind, provider); !known {
				return Config{}, fmt.Errorf("%w: %q", ErrUnknownProvider, provider)
			}
		}
	}

	// Enabling a gateway with no provider is a configuration that fails at the first
	// payment. Refusing it here is cheaper than a support ticket later.
	if kind == GatewayKind && truthy(values["enabled"]) {
		provider := current.Provider
		if raw, ok := values["provider"]; ok {
			provider = strings.TrimSpace(raw)
		}
		if provider == "" {
			return Config{}, fmt.Errorf("%w: choose a provider before enabling the gateway", ErrNotConfigured)
		}
	}

	if _, err := settings.New(r.pool).Update(ctx, group, values, actorID, relabel{inner: auditor, kind: kind}); err != nil {
		return Config{}, err
	}
	return r.Load(ctx, kind)
}

// checkEditable rejects a write to a field the environment supplies.
func (r *Resolver) checkEditable(kind Kind, current Config, key string) error {
	switch key {
	case "provider":
		if !current.ProviderEditable {
			return fmt.Errorf("%w: %s", ErrEnvOverride, providerEnv(kind))
		}
		return nil
	case "enabled":
		if kind == GatewayKind && !current.EnabledEditable {
			return fmt.Errorf("%w: %s", ErrEnvOverride, providerEnv(kind))
		}
	}
	for _, v := range current.Values {
		if v.Key != key {
			continue
		}
		if !v.Editable {
			return fmt.Errorf("%w: %s", ErrEnvOverride, v.Env)
		}
	}
	return nil
}

// groupFor maps a kind to its settings group.
func groupFor(kind Kind) string {
	if kind == ServerKind {
		return "servers"
	}
	return "gateway"
}

// truthy reads a boolean the way the settings store does.
func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes":
		return true
	}
	return false
}

// relabel renames the settings store's audit action to the integration's own, so an
// operator filtering the audit log for "who changed the gateway" gets the credential
// changes and not every setting in the panel.
//
// The row is still written by the settings store inside the transaction that wrote the
// value, which is what keeps house rule 5 true: an audit row cannot survive a rollback.
type relabel struct {
	inner audit.Auditor
	kind  Kind
}

// RecordAction implements audit.Auditor.
func (l relabel) RecordAction(ctx context.Context, tx pgx.Tx, action, targetType, targetID string,
	changes map[string]any) error {
	if l.inner == nil {
		return nil
	}
	if action == "setting.update" {
		if l.kind == ServerKind {
			action = "server.config.update"
		} else {
			action = "gateway.config.update"
		}
	}
	if changes != nil {
		changes["integration"] = string(l.kind)
	}
	return l.inner.RecordAction(ctx, tx, action, targetType, targetID, changes)
}
