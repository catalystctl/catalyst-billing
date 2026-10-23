package settings

// The credit rules that arrived after the credits group was written (CRD-10).
//
// The rest of `credits.*` is declared beside the invoices and tax groups in settings.go. This
// file exists only because that slice is the merge hotspot Register was added to avoid, and a
// contributor adding one key to a busy group is exactly the case it was written for.
//
// # Why credit-on-downgrade is a setting at all
//
// A downgrade returns the unused part of a period as credit. Whether that happens is a policy
// decision, not a law of arithmetic: an operator on a no-refunds contract wants the row recorded
// and the money kept, and an operator who promised "pay only for what you use" wants it posted
// automatically. Both are legitimate, so the answer is configuration rather than a hard-coded
// default in the downgrade path.
//
// The default is on because the alternative is the defect the parity inventory recorded: a
// customer is charged for a period at a higher price, downgrades mid-period, and is owed money
// that nothing ever posts. An installation that does not want to give it back has to say so.
//
// # Why the key is declared here rather than in internal/credit
//
// A setting's name, type and default live in one registry, so the settings page, the API
// validator and every reader agree on what the key is called. internal/credit reads the value
// through `Lookup` for its default, which is what keeps the number an operator sees on the
// settings page and the number the downgrade path acts on the same number.

// CreditGroup is the settings group every credit key belongs to.
const CreditGroup = "credits"

// CreditOnDowngradeKey is `credits.credit_on_downgrade`: whether a downgrade posts the unused
// part of the period to the customer's balance.
const CreditOnDowngradeKey = "credit_on_downgrade"

func init() {
	Register(
		Definition{
			Group: CreditGroup, Key: CreditOnDowngradeKey, Kind: KindBool, Default: "true",
		},
	)
}
