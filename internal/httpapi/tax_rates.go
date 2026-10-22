// The tax-rate surface, as the panel's own screens see it.
//
// The rate table's CRUD is in commerce_tax.go: creating, closing and deleting a dated row. This
// file is the other half — where the rows come from — and it exists as its own registrar so the
// import surface can grow without editing a list four domains share.
//
// The handlers live in tax_import.go. What is here is the registration and the one read the rate
// list needs in order to answer "is this number mine or the source's".

package httpapi

// init registers the tax-rate import surface.
func init() {
	RegisterRoutes("88-tax-import", func(r *router) { registerTaxImportRoutes(r, r.s) })
}
