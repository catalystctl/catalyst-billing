package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/credit"
	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

// Config options (UPG-07…UPG-11, SVC-07).
//
// A config option is what a hosting customer actually buys: "8 GB", "2 vCPU", "extra
// backup". It is not a checkout field. It changes the price of a *subscription*, so the
// selection lives on the service and the renewal engine prices from it — otherwise a
// customer who upgrades to 8 GB renews at the 2 GB price, which is the defect the parity
// backlog calls out by name.
//
// Two shapes are therefore first-class here:
//
//	Selection  — {option id: value} plus the priced contribution of each option
//	Resolution — the exact total of a selection against one price version
//
// The resolver is what the checkout path and the renewal engine both call, so a price
// quoted at checkout and a price charged at renewal cannot disagree: they are the same
// function reading the same rows.

// ConfigOptionTypes is the closed set the database CHECK enforces.
var ConfigOptionTypes = []string{"dropdown", "radio", "checkbox", "text", "number"}

// MaxConfigOptionName bounds a name so a catalog entry cannot become a paragraph.
const MaxConfigOptionName = 120

// ConfigOption is one configurable option of a product.
type ConfigOption struct {
	ID          string
	ProductID   string
	ParentID    *string
	Name        string
	Description string
	Type        string
	Choices     []string
	EnvVariable string
	Required    bool
	Hidden      bool
	Upgradable  bool
	SortOrder   int
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// Prices is the per-price-version amount of each choice. It is only populated by the
	// staff read that needs it; the resolver loads it itself so it cannot be stale.
	Prices []ConfigOptionPrice
}

// ConfigOptionPrice is one choice's price on one price version.
type ConfigOptionPrice struct {
	ID             string
	ConfigOptionID string
	PriceVersionID string
	Choice         string
	AmountMinor    int64
	SetupMinor     int64
}

// ConfigOptionInput is what the panel sends to create or edit an option.
type ConfigOptionInput struct {
	ProductID   string
	ParentID    *string
	Name        string
	Description string
	Type        string
	Choices     []string
	EnvVariable string
	Required    bool
	Hidden      bool
	// Upgradable is a pointer so an edit that does not mention it keeps the current value
	// instead of silently resetting it to false.
	Upgradable *bool
	SortOrder  int
}

// Validate checks an option definition.
func (in ConfigOptionInput) Validate() error {
	if strings.TrimSpace(in.ProductID) == "" {
		return fmt.Errorf("%w: a product is required", ErrInvalidInput)
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return fmt.Errorf("%w: a name is required", ErrInvalidInput)
	}
	if len(name) > MaxConfigOptionName {
		return fmt.Errorf("%w: the name is longer than %d characters", ErrInvalidInput, MaxConfigOptionName)
	}
	valid := false
	for _, t := range ConfigOptionTypes {
		if in.Type == t {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("%w: type must be one of %s", ErrInvalidInput,
			strings.Join(ConfigOptionTypes, ", "))
	}
	switch in.Type {
	case "dropdown", "radio":
		if len(in.Choices) == 0 {
			return fmt.Errorf("%w: a %s option needs at least one choice", ErrInvalidInput, in.Type)
		}
	case "checkbox", "text", "number":
		// A checkbox has no choices; a text or number option is free-form. Choices sent for
		// these are ignored rather than refused, so a client that sends the field
		// unconditionally still works.
	}
	seen := make(map[string]struct{}, len(in.Choices))
	for _, c := range in.Choices {
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("%w: a choice cannot be empty", ErrInvalidInput)
		}
		if _, dup := seen[c]; dup {
			return fmt.Errorf("%w: duplicate choice %q", ErrInvalidInput, c)
		}
		seen[c] = struct{}{}
	}
	return nil
}

// CreateConfigOption inserts an option.
func (s *Store) CreateConfigOption(ctx context.Context, in ConfigOptionInput) (ConfigOption, error) {
	if err := in.Validate(); err != nil {
		return ConfigOption{}, err
	}
	var out ConfigOption
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.createConfigOptionTx(ctx, tx, in)
		if err != nil {
			return err
		}
		return s.audit(ctx, tx, "config_option.create", "config_option", out.ID, "",
			map[string]any{"product_id": out.ProductID, "name": out.Name, "type": out.Type})
	})
	if err != nil {
		return ConfigOption{}, err
	}
	return out, nil
}

// CreateConfigOptionTx inserts an option inside a caller's transaction, for a seeder or a
// checkout that creates the option and its first price in one commit.
func (s *Store) CreateConfigOptionTx(ctx context.Context, tx pgx.Tx, in ConfigOptionInput) (string, error) {
	if err := in.Validate(); err != nil {
		return "", err
	}
	out, err := s.createConfigOptionTx(ctx, tx, in)
	if err != nil {
		return "", err
	}
	return out.ID, nil
}

func (s *Store) createConfigOptionTx(ctx context.Context, tx pgx.Tx, in ConfigOptionInput) (ConfigOption, error) {
	upgradable := true
	if in.Upgradable != nil {
		upgradable = *in.Upgradable
	}
	choices, err := json.Marshal(nonNilChoices(in.Choices))
	if err != nil {
		return ConfigOption{}, fmt.Errorf("service: encode choices: %w", err)
	}

	var out ConfigOption
	var raw []byte
	err = tx.QueryRow(ctx, `
		INSERT INTO config_options
			(product_id, parent_id, name, description, option_type, choices, env_variable,
			 required, hidden, upgradable, sort_order)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id, product_id, parent_id, name, description, option_type, choices,
		          env_variable, required, hidden, upgradable, sort_order, created_at, updated_at`,
		in.ProductID, in.ParentID, strings.TrimSpace(in.Name), in.Description, in.Type,
		choices, in.EnvVariable, in.Required, in.Hidden, upgradable, in.SortOrder).
		Scan(&out.ID, &out.ProductID, &out.ParentID, &out.Name, &out.Description, &out.Type,
			&raw, &out.EnvVariable, &out.Required, &out.Hidden, &out.Upgradable,
			&out.SortOrder, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConfigOption{}, ErrNotFound
	}
	if err != nil {
		return ConfigOption{}, fmt.Errorf("service: create config option: %w", err)
	}
	if err := json.Unmarshal(raw, &out.Choices); err != nil {
		out.Choices = nil
	}
	return out, nil
}

// UpdateConfigOption edits an option.
//
// `upgradable` is preserved when the input does not mention it, so an edit from a screen that
// does not show the flag cannot turn a live-changeable option into a fixed one.
func (s *Store) UpdateConfigOption(ctx context.Context, id string, in ConfigOptionInput) (ConfigOption, error) {
	if err := in.Validate(); err != nil {
		return ConfigOption{}, err
	}
	var out ConfigOption
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		choices, err := json.Marshal(nonNilChoices(in.Choices))
		if err != nil {
			return fmt.Errorf("service: encode choices: %w", err)
		}
		var raw []byte
		err = tx.QueryRow(ctx, `
			UPDATE config_options
			SET name = $2, description = $3, option_type = $4, choices = $5,
			    env_variable = $6, required = $7, hidden = $8,
			    upgradable = COALESCE($9, upgradable), sort_order = $10,
			    parent_id = $11
			WHERE id = $1
			RETURNING id, product_id, parent_id, name, description, option_type, choices,
			          env_variable, required, hidden, upgradable, sort_order, created_at, updated_at`,
			id, strings.TrimSpace(in.Name), in.Description, in.Type, choices, in.EnvVariable,
			in.Required, in.Hidden, in.Upgradable, in.SortOrder, in.ParentID).
			Scan(&out.ID, &out.ProductID, &out.ParentID, &out.Name, &out.Description,
				&out.Type, &raw, &out.EnvVariable, &out.Required, &out.Hidden,
				&out.Upgradable, &out.SortOrder, &out.CreatedAt, &out.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("service: update config option: %w", err)
		}
		if err := json.Unmarshal(raw, &out.Choices); err != nil {
			out.Choices = nil
		}
		return s.audit(ctx, tx, "config_option.update", "config_option", id, "",
			map[string]any{"name": out.Name, "type": out.Type})
	})
	if err != nil {
		return ConfigOption{}, err
	}
	return out, nil
}

// DeleteConfigOption removes an option.
//
// A selection on a live service is a priced fact; deleting the option underneath it would
// leave `service_configs` rows pointing at nothing and a renewal that cannot re-price. So an
// option that any service has selected is refused rather than cascaded.
func (s *Store) DeleteConfigOption(ctx context.Context, id string) error {
	return s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var inUse bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM service_configs WHERE config_option_id = $1)`, id).
			Scan(&inUse); err != nil {
			return fmt.Errorf("service: check option use: %w", err)
		}
		if inUse {
			return fmt.Errorf("%w: a live service has selected this option", ErrInvalidInput)
		}
		tag, err := tx.Exec(ctx, `DELETE FROM config_options WHERE id = $1`, id)
		if err != nil {
			return fmt.Errorf("service: delete config option: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, "config_option.delete", "config_option", id, "", nil)
	})
}

// ConfigOptions lists a product's options with their per-version prices.
func (s *Store) ConfigOptions(ctx context.Context, productID string) ([]ConfigOption, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, product_id, parent_id, name, description, option_type, choices,
		       env_variable, required, hidden, upgradable, sort_order, created_at, updated_at
		FROM config_options
		WHERE product_id = $1
		ORDER BY sort_order, name`, productID)
	if err != nil {
		return nil, fmt.Errorf("service: list config options: %w", err)
	}
	defer rows.Close()

	var out []ConfigOption
	index := map[string]int{}
	for rows.Next() {
		var (
			o   ConfigOption
			raw []byte
		)
		if err := rows.Scan(&o.ID, &o.ProductID, &o.ParentID, &o.Name, &o.Description,
			&o.Type, &raw, &o.EnvVariable, &o.Required, &o.Hidden, &o.Upgradable,
			&o.SortOrder, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, fmt.Errorf("service: scan config option: %w", err)
		}
		if err := json.Unmarshal(raw, &o.Choices); err != nil {
			o.Choices = nil
		}
		out = append(out, o)
		index[o.ID] = len(out) - 1
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("service: list config options: %w", err)
	}
	rows.Close()

	if len(out) == 0 {
		return out, nil
	}

	priceRows, err := s.pool.Query(ctx, `
		SELECT p.id, p.config_option_id, p.price_version_id, p.choice, p.amount_minor, p.setup_minor
		FROM config_option_prices p
		JOIN config_options o ON o.id = p.config_option_id
		WHERE o.product_id = $1
		ORDER BY p.choice`, productID)
	if err != nil {
		return nil, fmt.Errorf("service: list config option prices: %w", err)
	}
	defer priceRows.Close()
	for priceRows.Next() {
		var p ConfigOptionPrice
		if err := priceRows.Scan(&p.ID, &p.ConfigOptionID, &p.PriceVersionID, &p.Choice,
			&p.AmountMinor, &p.SetupMinor); err != nil {
			return nil, fmt.Errorf("service: scan config option price: %w", err)
		}
		if idx, ok := index[p.ConfigOptionID]; ok {
			out[idx].Prices = append(out[idx].Prices, p)
		}
	}
	return out, priceRows.Err()
}

// SetConfigOptionPrice sets one choice's amount and setup fee on one price version.
//
// It is an upsert on (option, price version, choice) because that is the natural key: the
// same choice priced twice is one price, not two rows whose order decides the answer.
func (s *Store) SetConfigOptionPrice(ctx context.Context, optionID, priceVersionID, choice string,
	amountMinor, setupMinor int64) (ConfigOptionPrice, error) {
	if amountMinor < 0 || setupMinor < 0 {
		return ConfigOptionPrice{}, fmt.Errorf("%w: an amount cannot be negative", ErrInvalidInput)
	}
	var out ConfigOptionPrice
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// The option must exist and belong to the price version's product. Without this a
		// typo would create a price that no selection can ever match.
		var matches bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM config_options o
				JOIN price_versions pv ON pv.product_id = o.product_id
				WHERE o.id = $1 AND pv.id = $2)`, optionID, priceVersionID).Scan(&matches); err != nil {
			return fmt.Errorf("service: check option product: %w", err)
		}
		if !matches {
			return fmt.Errorf("%w: that option and price version belong to different products", ErrInvalidInput)
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO config_option_prices
				(config_option_id, price_version_id, choice, amount_minor, setup_minor)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (config_option_id, price_version_id, choice)
			DO UPDATE SET amount_minor = EXCLUDED.amount_minor,
			              setup_minor = EXCLUDED.setup_minor
			RETURNING id, config_option_id, price_version_id, choice, amount_minor, setup_minor`,
			optionID, priceVersionID, choice, amountMinor, setupMinor).
			Scan(&out.ID, &out.ConfigOptionID, &out.PriceVersionID, &out.Choice,
				&out.AmountMinor, &out.SetupMinor); err != nil {
			return fmt.Errorf("service: set config option price: %w", err)
		}
		return s.audit(ctx, tx, "config_option.price", "config_option", optionID, "",
			map[string]any{
				"price_version_id": priceVersionID,
				"choice":           choice,
				"amount_minor":     amountMinor,
				"setup_minor":      setupMinor,
			})
	})
	if err != nil {
		return ConfigOptionPrice{}, err
	}
	return out, nil
}

// DeleteConfigOptionPrice removes one choice's price.
func (s *Store) DeleteConfigOptionPrice(ctx context.Context, optionID, priceVersionID, choice string) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM config_option_prices
		WHERE config_option_id = $1 AND price_version_id = $2 AND choice = $3`,
		optionID, priceVersionID, choice)
	if err != nil {
		return fmt.Errorf("service: delete config option price: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SelectionContribution is one option's priced contribution to a selection.
type SelectionContribution struct {
	ConfigOptionID string
	Name           string
	EnvVariable    string
	Value          string
	Amount         money.Amount
	Setup          money.Amount
}

// ResolvedSelection is a priced selection.
type ResolvedSelection struct {
	PriceVersionID string
	Currency       string
	// Options is the contribution of each option, in the option's own sort order. It is what
	// the invoice line explains, so the customer can see why the price is what it is.
	Options []SelectionContribution
	// Total is the sum of the option amounts: the recurring difference this selection makes
	// to the price of the plan.
	Total money.Amount
	// SetupTotal is the sum of the setup fees, charged once.
	SetupTotal money.Amount
	// Env is the environment the provisioner receives, one entry per option with an
	// env_variable. It is derived here rather than in the provisioner so the mapping is
	// reviewable in one place.
	Env map[string]string
}

// ResolveSelection prices a selection against one price version, exactly.
//
// This is the function the checkout path and the renewal engine both call. It is
// deliberately pure with respect to money: no rounding happens inside it beyond the sums,
// because every amount it reads is already whole minor units, and adding whole units cannot
// introduce a fraction.
//
// A selection naming an option the product does not offer, or a value the option does not
// offer, is refused rather than ignored: silently dropping it would charge for a selection
// the customer did not get.
func (s *Store) ResolveSelection(ctx context.Context, productID, priceVersionID string,
	selection map[string]string) (ResolvedSelection, error) {
	return s.resolveSelection(ctx, s.pool, productID, priceVersionID, selection)
}

// ResolveSelectionTx is ResolveSelection inside a caller's transaction, so checkout can
// price and insert in one commit.
func (s *Store) ResolveSelectionTx(ctx context.Context, tx pgx.Tx, productID, priceVersionID string,
	selection map[string]string) (ResolvedSelection, error) {
	return s.resolveSelection(ctx, tx, productID, priceVersionID, selection)
}

func (s *Store) resolveSelection(ctx context.Context, q Querier, productID, priceVersionID string,
	selection map[string]string) (ResolvedSelection, error) {
	var currency string
	var interval string
	err := q.QueryRow(ctx, `
		SELECT currency, interval FROM price_versions WHERE id = $1 AND product_id = $2`,
		priceVersionID, productID).Scan(&currency, &interval)
	if errors.Is(err, pgx.ErrNoRows) {
		return ResolvedSelection{}, fmt.Errorf("%w: the price version does not belong to that product", ErrInvalidInput)
	}
	if err != nil {
		return ResolvedSelection{}, fmt.Errorf("service: load price version: %w", err)
	}
	cur, err := currencyOf(currency)
	if err != nil {
		return ResolvedSelection{}, err
	}

	out := ResolvedSelection{
		PriceVersionID: priceVersionID,
		Currency:       currency,
		Options:        []SelectionContribution{},
		Total:          money.Zero(cur),
		SetupTotal:     money.Zero(cur),
		Env:            map[string]string{},
	}

	rows, err := q.Query(ctx, `
		SELECT o.id, o.name, o.option_type, o.env_variable, o.required, o.hidden, o.choices,
		       o.sort_order
		FROM config_options o
		WHERE o.product_id = $1
		ORDER BY o.sort_order, o.name`, productID)
	if err != nil {
		return ResolvedSelection{}, fmt.Errorf("service: list options: %w", err)
	}
	type optionRow struct {
		id, name, typ, env string
		required, hidden   bool
		choices            []string
	}
	var options []optionRow
	for rows.Next() {
		var (
			o   optionRow
			raw []byte
		)
		if err := rows.Scan(&o.id, &o.name, &o.typ, &o.env, &o.required, &o.hidden, &raw,
			new(int)); err != nil {
			rows.Close()
			return ResolvedSelection{}, fmt.Errorf("service: scan option: %w", err)
		}
		if err := json.Unmarshal(raw, &o.choices); err != nil {
			o.choices = nil
		}
		options = append(options, o)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return ResolvedSelection{}, fmt.Errorf("service: list options: %w", err)
	}
	rows.Close()

	// Every option the caller named must exist for this product.
	byID := make(map[string]optionRow, len(options))
	for _, o := range options {
		byID[o.id] = o
	}
	for id := range selection {
		if _, ok := byID[id]; !ok {
			return ResolvedSelection{}, fmt.Errorf("%w: %s is not an option of this product",
				ErrOptionNotAvailable, id)
		}
	}

	// A required option that was not chosen is refused: it is what makes "8 GB" a real
	// requirement rather than a suggestion.
	for _, o := range options {
		if o.required && !o.hidden {
			if _, ok := selection[o.id]; !ok {
				return ResolvedSelection{}, fmt.Errorf("%w: %s is required", ErrOptionNotAvailable, o.name)
			}
		}
	}

	for _, o := range options {
		value, chosen := selection[o.id]
		if !chosen {
			continue
		}
		if err := validValue(o.typ, o.choices, value); err != nil {
			return ResolvedSelection{}, fmt.Errorf("%w: %s: %s", ErrOptionNotAvailable, o.name, err)
		}

		var amountMinor, setupMinor int64
		err := q.QueryRow(ctx, `
			SELECT amount_minor, setup_minor FROM config_option_prices
			WHERE config_option_id = $1 AND price_version_id = $2 AND choice = $3`,
			o.id, priceVersionID, value).Scan(&amountMinor, &setupMinor)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return ResolvedSelection{}, fmt.Errorf("service: read option price: %w", err)
		}
		// A choice with no price row is free, which is what the migration documents.

		amount := money.New(amountMinor, cur)
		setup := money.New(setupMinor, cur)
		if out.Total, err = out.Total.Add(amount); err != nil {
			return ResolvedSelection{}, err
		}
		if out.SetupTotal, err = out.SetupTotal.Add(setup); err != nil {
			return ResolvedSelection{}, err
		}
		out.Options = append(out.Options, SelectionContribution{
			ConfigOptionID: o.id,
			Name:           o.name,
			EnvVariable:    o.env,
			Value:          value,
			Amount:         amount,
			Setup:          setup,
		})
		if o.env != "" {
			out.Env[o.env] = value
		}
	}
	return out, nil
}

// validValue checks a chosen value against the option's type.
func validValue(typ string, choices []string, value string) error {
	switch typ {
	case "dropdown", "radio":
		for _, c := range choices {
			if c == value {
				return nil
			}
		}
		return fmt.Errorf("%q is not one of the offered choices", value)
	case "checkbox":
		// A checkbox is on or off; the only stored value is "on".
		if value == "" || strings.EqualFold(value, "false") || value == "off" {
			return fmt.Errorf("%q is not a checked value", value)
		}
		return nil
	case "number":
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("a number is required")
		}
		for _, r := range value {
			if (r < '0' || r > '9') && r != '.' && r != '-' {
				return fmt.Errorf("%q is not a number", value)
			}
		}
		return nil
	default: // text
		return nil
	}
}

// ServiceConfig is one stored selection on a service.
type ServiceConfig struct {
	ID             string
	ServiceID      string
	ConfigOptionID string
	Value          string
	AmountMinor    int64
	SetupMinor     int64
	CreatedAt      time.Time

	// Joined for display.
	Name        string
	Type        string
	Description string
	EnvVariable string
	Choices     []string
	Upgradable  bool
	SortOrder   int
}

// ServiceConfigs returns a service's stored selection.
func (s *Store) ServiceConfigs(ctx context.Context, serviceID string) ([]ServiceConfig, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sc.id, sc.service_id, sc.config_option_id, sc.value, sc.amount_minor,
		       sc.setup_minor, sc.created_at,
		       o.name, o.option_type, o.description, o.env_variable, o.choices,
		       o.upgradable, o.sort_order
		FROM service_configs sc
		JOIN config_options o ON o.id = sc.config_option_id
		WHERE sc.service_id = $1
		ORDER BY o.sort_order, o.name`, serviceID)
	if err != nil {
		return nil, fmt.Errorf("service: list service configs: %w", err)
	}
	defer rows.Close()

	out := []ServiceConfig{}
	for rows.Next() {
		var (
			c   ServiceConfig
			raw []byte
		)
		if err := rows.Scan(&c.ID, &c.ServiceID, &c.ConfigOptionID, &c.Value, &c.AmountMinor,
			&c.SetupMinor, &c.CreatedAt, &c.Name, &c.Type, &c.Description, &c.EnvVariable,
			&raw, &c.Upgradable, &c.SortOrder); err != nil {
			return nil, fmt.Errorf("service: scan service config: %w", err)
		}
		if err := json.Unmarshal(raw, &c.Choices); err != nil {
			c.Choices = nil
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ConfigChangeRequest asks to change one option on a live service.
type ConfigChangeRequest struct {
	ServiceID      string
	ConfigOptionID string
	Value          string
	UserID         string
	Reason         string
	Staff          bool
}

// ChangeServiceConfig changes one option on a live service and charges the prorated delta.
//
// It is an upgrade in everything but name: the option's amount changes what the subscription
// renews at, so the difference for the rest of the period is charged (or credited) exactly as
// a product upgrade would be. That is why it goes through the same proration and the same
// `service_upgrades` row: two mechanisms for "the price changed mid-cycle" would be two
// places for the arithmetic to drift.
func (s *Store) ChangeServiceConfig(ctx context.Context, req ConfigChangeRequest) (UpgradeResult, error) {
	var out UpgradeResult
	now := s.clock()

	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			productID  string
			subID      string
			customerID string
			status     string
		)
		var err error
		if req.Staff {
			err = tx.QueryRow(ctx, `
				SELECT product_id, COALESCE(subscription_id::text, ''), customer_id, status
				FROM services WHERE id = $1 FOR UPDATE`, req.ServiceID).
				Scan(&productID, &subID, &customerID, &status)
		} else {
			err = tx.QueryRow(ctx, `
				SELECT s.product_id, COALESCE(s.subscription_id::text, ''), s.customer_id, s.status
				FROM services s
				JOIN customer_accounts ca ON ca.customer_id = s.customer_id
				WHERE s.id = $1 AND ca.user_id = $2
				FOR UPDATE OF s`, req.ServiceID, req.UserID).
				Scan(&productID, &subID, &customerID, &status)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("service: lock service: %w", err)
		}
		if status == "terminated" || status == "terminating" {
			return fmt.Errorf("%w: a %s service cannot be changed", ErrNotUpgradable, status)
		}
		if subID == "" {
			return fmt.Errorf("%w: this service has no billing schedule", ErrNotUpgradable)
		}

		sub, err := scanSubscription(tx.QueryRow(ctx, `
			SELECT `+subscriptionColumns+` FROM subscriptions WHERE id = $1 FOR UPDATE`, subID))
		if err != nil {
			return err
		}

		var (
			upgradable bool
			optionName string
			optionType string
			choices    []byte
			product    string
		)
		err = tx.QueryRow(ctx, `
			SELECT upgradable, name, option_type, choices, product_id
			FROM config_options WHERE id = $1`, req.ConfigOptionID).
			Scan(&upgradable, &optionName, &optionType, &choices, &product)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no such option", ErrOptionNotAvailable)
		}
		if err != nil {
			return fmt.Errorf("service: load option: %w", err)
		}
		if product != productID {
			return fmt.Errorf("%w: the option belongs to a different product", ErrOptionNotAvailable)
		}
		if !upgradable {
			return fmt.Errorf("%w: %s", ErrOptionNotUpgradable, optionName)
		}

		// The value must be one the option offers. Without this a caller could store "64 GB"
		// against a 2/8/16 GB dropdown, which would price as free (no matching row) and leave
		// a service whose selection the provisioner cannot act on.
		var offered []string
		if err := json.Unmarshal(choices, &offered); err != nil {
			offered = nil
		}
		if err := validValue(optionType, offered, req.Value); err != nil {
			return fmt.Errorf("%w: %s: %s", ErrOptionNotAvailable, optionName, err)
		}

		// The delta is the new contribution minus the stored one, priced against the
		// subscription's current price version. Both sides come from the same resolver, so a
		// free choice and a missing price row cannot be mistaken for each other.
		price, err := loadPriceVersion(ctx, tx, sub.PriceVersionID)
		if err != nil {
			return err
		}
		cur, err := currencyOf(price.Currency)
		if err != nil {
			return err
		}

		var previousMinor int64
		err = tx.QueryRow(ctx, `
			SELECT amount_minor FROM service_configs
			WHERE service_id = $1 AND config_option_id = $2`,
			req.ServiceID, req.ConfigOptionID).Scan(&previousMinor)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("service: read current selection: %w", err)
		}

		var newMinor, newSetup int64
		err = tx.QueryRow(ctx, `
			SELECT amount_minor, setup_minor FROM config_option_prices
			WHERE config_option_id = $1 AND price_version_id = $2 AND choice = $3`,
			req.ConfigOptionID, sub.PriceVersionID, req.Value).Scan(&newMinor, &newSetup)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("service: read option price: %w", err)
		}

		delta := money.New(newMinor-previousMinor, cur)

		start, end := billingPeriod(sub, now)
		proration, err := ComputeProration(start, end, now)
		if err != nil {
			return err
		}
		prorated, err := money.ApplyRate(delta, proration.Ratio, money.RoundHalfUp)
		if err != nil {
			return err
		}

		out = UpgradeResult{
			ServiceID:          req.ServiceID,
			FromPriceVersionID: sub.PriceVersionID,
			ToPriceVersionID:   sub.PriceVersionID,
			Currency:           price.Currency,
			ProratedMinor:      prorated.Minor(),
			Proration:          proration,
			Status:             UpgradePending,
		}
		switch {
		case prorated.IsPositive():
			out.Direction = "upgrade"
		case prorated.IsNegative():
			out.Direction = "downgrade"
		default:
			out.Direction = "none"
		}

		var invoiceID any
		if prorated.IsPositive() {
			if s.Issuer == nil {
				return fmt.Errorf("%w: invoice issuance is not configured", ErrInvalidInput)
			}
			id, num, err := s.issueProratedInvoice(ctx, tx, proratedInvoice{
				CustomerID:     customerID,
				SubscriptionID: sub.ID,
				PriceVersionID: sub.PriceVersionID,
				Amount:         prorated,
				Description: fmt.Sprintf("Prorated change to %s: %s (%s of %s days remaining)",
					optionName, req.Value, days(proration.Remaining), days(proration.Total)),
			})
			if err != nil {
				return err
			}
			invoiceID = id
			out.InvoiceID, out.InvoiceNumber = id, num
			// An option change that costs money is an invoice like any other, so `credits.auto_use`
			// applies to it too (CRD-03). Leaving this path out would mean the one change a customer
			// makes from their own service page was the one their balance never paid for.
			if _, err := credit.ApplyCreditToInvoice(ctx, tx, id); err != nil {
				return err
			}
		} else if prorated.IsNegative() {
			out.CreditMinor = -prorated.Minor()
			out.CreditPending = true
		}

		// The stored selection carries the price it contributed at selection time, so a later
		// option-price change does not silently re-price an existing service. That is why the
		// amount is written here rather than joined at read time.
		if _, err := tx.Exec(ctx, `
			INSERT INTO service_configs
				(service_id, config_option_id, value, amount_minor, setup_minor)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (service_id, config_option_id)
			DO UPDATE SET value = EXCLUDED.value,
			              amount_minor = EXCLUDED.amount_minor,
			              setup_minor = EXCLUDED.setup_minor`,
			req.ServiceID, req.ConfigOptionID, req.Value, newMinor, newSetup); err != nil {
			return fmt.Errorf("service: store selection: %w", err)
		}

		var upgradeID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO service_upgrades
				(service_id, from_price_version_id, to_price_version_id, prorated_minor,
				 currency, invoice_id, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING id`,
			req.ServiceID, sub.PriceVersionID, sub.PriceVersionID, prorated.Minor(),
			price.Currency, invoiceID, UpgradePending).Scan(&upgradeID); err != nil {
			return fmt.Errorf("service: record option change: %w", err)
		}
		out.UpgradeID = upgradeID

		return s.audit(ctx, tx, "service.config_change", "service", req.ServiceID, req.UserID,
			map[string]any{
				"upgrade_id":       upgradeID,
				"config_option_id": req.ConfigOptionID,
				"option":           optionName,
				"option_type":      optionType,
				"value":            req.Value,
				"previous_minor":   previousMinor,
				"new_minor":        newMinor,
				"prorated_minor":   prorated.Minor(),
				"invoice_id":       out.InvoiceID,
				"reason":           req.Reason,
			})
	})
	if err != nil {
		return UpgradeResult{}, err
	}
	return out, nil
}

// A plan change and its config options as one priced change (UPG-02).
//
// The parity inventory's complaint about the customer upgrade flow is precise: the plan was
// changed through one request and the options through another, so "move me to the 8 GB plan" was
// two economic events with two confirmations and two prorated charges. That is not only clumsy, it
// is wrong: the base price and the option prices are one rental, the renewal engine bills them as
// one (base plus one line per chosen option), so splitting the change across two requests prices
// each half against a period the other half has already moved.
//
// `ChangePlan` is therefore the whole change. It reuses `upgrade` — the same lock order, the same
// reachability and currency and interval rules, the same `ComputeProration`/`ProratedDifference`
// with one RoundHalfUp, the same `issueProratedInvoice` — and only tells it to price the
// selection as well. There is no second arithmetic here; adding one would be how the same change
// came to cost two different amounts on two screens.
//
// What a downgrade owes is unchanged: the negative prorated difference is recorded on the
// `service_upgrades` row and posted by internal/credit, which then applies the change.

// PlanChangeRequest asks to move a service to another price version, choosing that plan's config
// options in the same request.
type PlanChangeRequest struct {
	ServiceID string
	// PriceVersionID is the target. It must be reachable from the service's product and bill in
	// the same currency on the same interval.
	PriceVersionID string
	// Selection is the target plan's options, keyed by option id. A nil map means "do not touch
	// the options" and is what the bare upgrade route sends; an empty map means "this plan has no
	// options", which is a different statement and prices as one.
	Selection map[string]string
	UserID    string
	Reason    string
	// Staff bypasses the customer scoping, like every other staff path on a service.
	Staff bool
}

// ChangePlan moves a service to a target price version with the target plan's options.
func (s *Store) ChangePlan(ctx context.Context, req PlanChangeRequest) (UpgradeResult, error) {
	if strings.TrimSpace(req.ServiceID) == "" || strings.TrimSpace(req.PriceVersionID) == "" {
		return UpgradeResult{}, fmt.Errorf("%w: a service and a target plan are required", ErrInvalidInput)
	}
	selection := req.Selection
	if selection == nil {
		// An absent selection is "do not touch the options". A caller that meant "this plan has no
		// options" has to say so, because the two price differently: the first leaves the stored
		// options on the invoice, the second takes them away.
		selection = map[string]string{}
	}
	return s.upgrade(ctx, UpgradeRequest{
		ServiceID:      req.ServiceID,
		PriceVersionID: req.PriceVersionID,
		Selection:      selection,
		UserID:         req.UserID,
		Reason:         req.Reason,
		Staff:          req.Staff,
	})
}

// storedSelectionTotal prices a service's stored selection on one price version.
//
// The rule is the renewal engine's, deliberately, and it is the only rule that keeps a quoted
// change and the next invoice in step: an option is priced from `config_option_prices` at the
// version being billed, a choice with no row there is free, and a selection that recorded a
// non-zero amount but no longer has a price row is refused rather than valued at zero. Billing
// zero for something the customer is paying for is the revenue leak that made the renewal engine
// read this table in the first place.
func (s *Store) storedSelectionTotal(ctx context.Context, q Querier, serviceID string,
	current priceVersion, cur money.Currency) (money.Amount, error) {
	total := money.Zero(cur)
	rows, err := q.Query(ctx, `
		SELECT sc.value, sc.amount_minor, cop.amount_minor, co.name
		FROM service_configs sc
		JOIN config_options co ON co.id = sc.config_option_id
		LEFT JOIN config_option_prices cop
		       ON cop.config_option_id = sc.config_option_id
		      AND cop.price_version_id = $2
		      AND cop.choice = sc.value
		WHERE sc.service_id = $1
		ORDER BY co.sort_order, co.name, co.id`, serviceID, current.ID)
	if err != nil {
		return money.Amount{}, fmt.Errorf("service: read stored selection: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			value    string
			recorded int64
			priced   *int64
			name     string
		)
		if err := rows.Scan(&value, &recorded, &priced, &name); err != nil {
			return money.Amount{}, fmt.Errorf("service: scan stored selection: %w", err)
		}
		var contribution int64
		switch {
		case priced != nil:
			contribution = *priced
		case recorded == 0:
			// Free at selection time (a checkbox, a text or number option) and free now.
			contribution = 0
		default:
			return money.Amount{}, fmt.Errorf(
				"%w: %s (%s) is paid but the catalogue no longer prices it at this plan",
				ErrOptionNotAvailable, name, value)
		}
		if total, err = total.Add(money.New(contribution, cur)); err != nil {
			return money.Amount{}, err
		}
	}
	return total, rows.Err()
}

// applyPendingSelection moves a change's chosen options into the service, inside the transaction
// that repriced it.
//
// It replaces the *previous plan's* options and upserts the chosen ones. Deleting by product rather
// than emptying the table is deliberate: an option the operator attached to the same product but
// did not show the customer (a hidden option) must survive the change, and it would be silently
// dropped — and its recurring amount with it — by a blanket delete.
func (s *Store) applyPendingSelection(ctx context.Context, tx pgx.Tx, upgradeID, serviceID, toPriceVersionID string) error {
	rows, err := tx.Query(ctx, `
		SELECT config_option_id, value, amount_minor, setup_minor
		FROM service_upgrade_configs
		WHERE upgrade_id = $1
		ORDER BY config_option_id`, upgradeID)
	if err != nil {
		return fmt.Errorf("service: read chosen options: %w", err)
	}
	type chosen struct {
		optionID    string
		value       string
		amountMinor int64
		setupMinor  int64
	}
	var selection []chosen
	for rows.Next() {
		var c chosen
		if err := rows.Scan(&c.optionID, &c.value, &c.amountMinor, &c.setupMinor); err != nil {
			rows.Close()
			return fmt.Errorf("service: scan chosen option: %w", err)
		}
		selection = append(selection, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("service: read chosen options: %w", err)
	}
	rows.Close()

	if len(selection) == 0 {
		return nil
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM service_configs sc
		USING config_options co, price_versions pv
		WHERE sc.service_id = $1
		  AND co.id = sc.config_option_id
		  AND pv.id = $2
		  AND co.product_id <> pv.product_id`, serviceID, toPriceVersionID); err != nil {
		return fmt.Errorf("service: clear the previous plan's options: %w", err)
	}

	for _, c := range selection {
		if _, err := tx.Exec(ctx, `
			INSERT INTO service_configs
				(service_id, config_option_id, value, amount_minor, setup_minor)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (service_id, config_option_id)
			DO UPDATE SET value = EXCLUDED.value,
			              amount_minor = EXCLUDED.amount_minor,
			              setup_minor = EXCLUDED.setup_minor`,
			serviceID, c.optionID, c.value, c.amountMinor, c.setupMinor); err != nil {
			return fmt.Errorf("service: store chosen option: %w", err)
		}
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM service_upgrade_configs WHERE upgrade_id = $1`, upgradeID); err != nil {
		return fmt.Errorf("service: clear chosen options: %w", err)
	}
	return nil
}

// nonNilChoices renders a nil slice as an empty JSON array rather than null, so a client can
// always iterate it.
func nonNilChoices(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
