package openapi

// The catalogue's structural operations: the category tree and the product → server assignment.
//
// Three things are stated in the document rather than left to a handler's comment, because they
// are the parts a client has to be able to rely on:
//
//   - A category tree is refused at two specific points, with two specific codes.
//     `category_cycle` (409) means the move would make a category its own ancestor; it is
//     enforced by the database, so it cannot be raced. `category_too_deep` (422) means the move
//     would push a branch past four levels, checked on the branch rather than on the moved row.
//
//   - A category with sub-categories cannot be deleted. It is refused (409 `in_use`) rather than
//     reparented, because reparenting changes a URL a customer may have bookmarked.
//
//   - A product's server is the *same* target the price version already names. The plan's region
//     wins when it sets one; the product's server supplies it otherwise. It is not a second
//     routing mechanism, and the document says so where a reader will look for it.

func init() { RegisterDescriptions(catalogNestingOperations) }

// categoryNodeSchema is one node of the tree. It repeats the flat category's fields so a client
// that already reads `product_count` keeps working, and adds the three the tree needs.
func categoryNodeSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"required": []string{
			"id", "slug", "name", "sort_order", "depth", "path", "subtree_product_count",
		},
		"properties": map[string]any{
			"id":          map[string]any{"type": "string", "format": "uuid"},
			"slug":        map[string]any{"type": "string"},
			"name":        map[string]any{"type": "string"},
			"description": map[string]any{"type": "string"},
			"sort_order":  map[string]any{"type": "integer"},
			"hidden":      map[string]any{"type": "boolean"},
			"parent_id": map[string]any{
				"type":        []string{"string", "null"},
				"format":      "uuid",
				"description": "Null for a root.",
			},
			"depth": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"maximum":     4,
				"description": "1 for a root. Nesting deeper than 4 is refused by the database.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "Slash-joined slug chain, e.g. `game-servers/minecraft`.",
			},
			"product_count": map[string]any{
				"type":        "integer",
				"description": "Products filed directly in this category.",
			},
			"subtree_product_count": map[string]any{
				"type": "integer",
				"description": "Products in this category and in every category below it. This is the " +
					"number a filter click needs: a product filed under a child must be findable from " +
					"the parent.",
			},
			"children": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "object"},
				"description": "The child nodes, in display order.",
			},
		},
	}
}

// serverSchema is one provisioner target.
func serverSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id", "slug", "name", "region", "node", "enabled"},
		"properties": map[string]any{
			"id":   map[string]any{"type": "string", "format": "uuid"},
			"slug": map[string]any{"type": "string"},
			"name": map[string]any{"type": "string"},
			"provider": map[string]any{
				"type": "string",
				"description": "The provisioning extension that owns this target. Empty means the " +
					"installation's configured extension.",
			},
			"region": map[string]any{
				"type": "string",
				"description": "Passed to the adapter as `CreateRequest.Region` — the same field a " +
					"price version's `provision_region` fills.",
			},
			"node": map[string]any{
				"type":        "string",
				"description": "The provider's own node/group label, passed as `server_node` metadata.",
			},
			"enabled": map[string]any{
				"type":        "boolean",
				"description": "A disabled target keeps its products and services and is not offered for a new assignment.",
			},
			"sort_order":    map[string]any{"type": "integer"},
			"product_count": map[string]any{"type": "integer", "description": "Products sold on this target."},
			"created_at":    map[string]any{"type": "string", "format": "date-time"},
			"updated_at":    map[string]any{"type": "string", "format": "date-time"},
		},
	}
}

// serverRequestSchema is the body of a create or an update: the schema, without the fields the
// server computes.
func serverRequestSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"slug", "name"},
		"properties": map[string]any{
			"slug":       map[string]any{"type": "string", "pattern": "^[a-z0-9]+(-[a-z0-9]+)*$"},
			"name":       map[string]any{"type": "string"},
			"provider":   map[string]any{"type": "string", "maxLength": 128},
			"region":     map[string]any{"type": "string", "maxLength": 128},
			"node":       map[string]any{"type": "string", "maxLength": 128},
			"enabled":    map[string]any{"type": "boolean"},
			"sort_order": map[string]any{"type": "integer"},
		},
	}
}

// placementSchema is a product's structural placement.
func placementSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"product_id", "category_path"},
		"properties": map[string]any{
			"product_id": map[string]any{"type": "string", "format": "uuid"},
			"server_id":  map[string]any{"type": []string{"string", "null"}, "format": "uuid"},
			"server": map[string]any{
				"type":        []string{"object", "null"},
				"description": "The resolved target, or null when the product has none.",
				"properties":  serverSchema()["properties"],
			},
			"category_id":        map[string]any{"type": []string{"string", "null"}, "format": "uuid"},
			"category_parent_id": map[string]any{"type": []string{"string", "null"}, "format": "uuid"},
			"category_path": map[string]any{
				"type":        "string",
				"description": "Slash-joined slug chain of the product's category, e.g. `game-servers/minecraft`.",
			},
			"category_ancestor_slugs": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Every category from the root down to the product's own.",
			},
		},
	}
}

func catalogNestingOperations() map[string]Operation {
	return map[string]Operation{
		// ------------------------------------------------------------ staff reads --

		"GET /api/v1/staff/catalog/categories/tree": {
			Summary: "Read the category tree",
			Description: "Returns every category, nested, with the parent it hangs from, its depth (1 for a " +
				"root), its slash-joined slug path, and `subtree_product_count` — the products in the " +
				"category and in every category below it. A flat catalogue comes back as a list of " +
				"roots with no children, exactly as before. Hidden categories are included here " +
				"(staff may see drafts); the public read prunes them.",
			Responses: []Response{
				{Status: 200, Description: "The tree", Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"data":      map[string]any{"type": "array", "items": categoryNodeSchema()},
						"max_depth": map[string]any{"type": "integer", "description": "The nesting bound the database enforces (4)."},
					},
				}},
				{Status: 403, Description: "The caller lacks `invoice:read:all`", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/catalog/products/{id}/placement": {
			Summary: "Read where a product sits",
			Description: "The product's provisioner target and its place in the category tree, in one read. " +
				"It is separate from the product shape because the product shape is also read by the " +
				"storefront, and a visitor must never be handed a node name or a server id.",
			Responses: []Response{
				{Status: 200, Description: "The placement", Schema: placementSchema()},
				{Status: 404, Description: "No such product", Schema: ProblemRef()},
			},
		},
		"GET /api/v1/staff/catalog/servers": {
			Summary: "List provisioner targets",
			Description: "The nodes a product can be sold onto, disabled ones included so the panel can " +
				"re-enable one. These are targets *on* the provider the installation configures in its " +
				"`servers.*` settings, not the provider configuration itself.",
			Responses: []Response{
				{Status: 200, Description: "The targets", Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"data": map[string]any{"type": "array", "items": serverSchema()},
					},
				}},
			},
		},

		// ----------------------------------------------------------- staff writes --

		"PUT /api/v1/staff/catalog/categories/{id}/parent": {
			Summary: "Move a category",
			Description: "Sets the category's parent, or moves it to the root when `parent_id` is null. " +
				"Two moves are refused by the database rather than by this service, because two " +
				"concurrent writes can each look safe and together form a cycle: 409 `category_cycle` " +
				"when the move would make a category its own ancestor, and 422 `category_too_deep` when " +
				"it would push any part of the branch past four levels. The depth check is on the whole " +
				"subtree, so moving a three-deep branch under a two-deep parent is refused even though " +
				"the moved row itself would only sit at level three. The response is the whole tree, so " +
				"the panel re-renders from the truth rather than from what it hoped happened.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"parent_id"},
				"properties": map[string]any{
					"parent_id": map[string]any{
						"type":        []string{"string", "null"},
						"format":      "uuid",
						"description": "The new parent, or null to make the category a root.",
					},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The tree after the move", Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"data":      map[string]any{"type": "array", "items": categoryNodeSchema()},
						"max_depth": map[string]any{"type": "integer"},
					},
				}},
				{Status: 404, Description: "No such category, or no such parent", Schema: ProblemRef()},
				{Status: 409, Description: "The move would create a cycle (`category_cycle`)", Schema: ProblemRef()},
				{Status: 422, Description: "The move would exceed four levels (`category_too_deep`)", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/catalog/products/{id}/server": {
			Summary: "Assign a product to a server",
			Description: "Sets the provisioner target a product is sold onto, or clears it when `server_id` is " +
				"null. This is catalogue metadata: it decides where the *next* purchase is provisioned and " +
				"changes nothing about services that already exist, whose region was copied into their own " +
				"config at checkout. It is not a second routing mechanism — the value becomes the same " +
				"`region` a price version's `provision_region` already supplies, and a plan that names a " +
				"region still wins.",
			RequestSchema: map[string]any{
				"type":     "object",
				"required": []string{"server_id"},
				"properties": map[string]any{
					"server_id": map[string]any{
						"type":        []string{"string", "null"},
						"format":      "uuid",
						"description": "The target, or null to fall back to the plan's own region.",
					},
				},
			},
			Responses: []Response{
				{Status: 200, Description: "The placement after the change", Schema: placementSchema()},
				{Status: 404, Description: "No such product, or no such server", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/catalog/servers": {
			Summary: "Create a provisioner target",
			Description: "Adds a node a product can be sold onto. The slug is the identity an operator reads " +
				"in a report, so it is unique case-insensitively. `region` is what the adapter receives as " +
				"`CreateRequest.Region`; `node` is the provider's own label and is passed as `server_node` " +
				"metadata beside it.",
			RequestSchema: serverRequestSchema(),
			Responses: []Response{
				{Status: 201, Description: "The created target", Schema: serverSchema()},
				{Status: 409, Description: "That slug is already in use (`slug_taken`)", Schema: ProblemRef()},
				{Status: 422, Description: "The target would not be valid", Schema: ProblemRef()},
			},
		},
		"PUT /api/v1/staff/catalog/servers/{id}": {
			Summary: "Edit a provisioner target",
			Description: "Changes a target's name, region, node or enabled state. A change affects where the " +
				"next purchase provisions; existing services keep the target they were created with. " +
				"Disabling is how an operator stops selling onto a node without disturbing what is already " +
				"there.",
			RequestSchema: serverRequestSchema(),
			Responses: []Response{
				{Status: 200, Description: "The updated target", Schema: serverSchema()},
				{Status: 404, Description: "No such target", Schema: ProblemRef()},
				{Status: 409, Description: "That slug is already in use (`slug_taken`)", Schema: ProblemRef()},
				{Status: 422, Description: "The target would not be valid", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/catalog/servers/{id}": {
			Summary: "Delete a provisioner target",
			Description: "Refused while a product is still sold on the target (409 `in_use`), because deleting " +
				"it would silently move those products back to their plan's region — a provisioning change " +
				"nobody asked for. Disable the target instead, or clear the products' assignments first.",
			Responses: []Response{
				{Status: 204, Description: "Deleted"},
				{Status: 404, Description: "No such target", Schema: ProblemRef()},
				{Status: 409, Description: "Products are still sold on it (`in_use`)", Schema: ProblemRef()},
			},
		},

		// ---------------------------------------------------------------- public --

		"GET /api/v1/catalog/categories": {
			Summary: "The storefront's category tree",
			Description: "The visible category tree, nested, with `product_count` reporting the whole branch " +
				"below each node. A hidden category is omitted together with everything under it: hiding a " +
				"branch means hiding what is under it. Cached for sixty seconds, like the rest of the " +
				"anonymous storefront.",
			Responses: []Response{
				{Status: 200, Description: "The visible tree", Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"categories": map[string]any{"type": "array", "items": categoryNodeSchema()},
					},
				}},
			},
		},
		"GET /api/v1/catalog/category/{slug}": {
			Summary: "One storefront category page",
			Description: "The category, its visible children, and every product in the branch below it — " +
				"which is what makes nesting worth having: clicking \"Game servers\" lists the Minecraft " +
				"products filed under its child. The predicates are the storefront's usual ones (not " +
				"hidden, priced, in a visible category) plus one the flat read cannot express: a product " +
				"whose ancestor is hidden is not offered. A hidden or absent category is 404, " +
				"indistinguishable from a missing one.",
			Responses: []Response{
				{Status: 200, Description: "The category and its products", Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"category": categoryNodeSchema(),
						"products": map[string]any{
							"type":        "array",
							"items":       map[string]any{"type": "object"},
							"description": "The public product shape the storefront already reads.",
						},
					},
				}},
				{Status: 404, Description: "No such category, or it is not visible", Schema: ProblemRef()},
			},
		},
	}
}
