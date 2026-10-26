package openapi

func init() { RegisterDescriptions(operationActionDescriptions) }

func operationActionDescriptions() map[string]Operation {
	return map[string]Operation{
		"POST /api/v1/staff/operations/{id}/retry": {
			Summary: "Retry a failed operation",
			Description: "Requeues a failed provisioning operation using its original idempotency key. " +
				"The service generation is checked under lock so a stale retry cannot mutate a newer service state.",
			Responses: []Response{
				{Status: 202, Description: "The operation was requeued", Schema: AcceptedSchema()},
				{Status: 404, Description: "Operation not found", Schema: ProblemRef()},
				{Status: 409, Description: "The operation cannot be retried", Schema: ProblemRef()},
			},
		},
		"POST /api/v1/staff/operations/{id}/resolve": {
			Summary: "Resolve an operation",
			Description: "Records an operator decision that the remote outcome is settled and closes the " +
				"operation without silently changing the service, invoice, or ledger.",
			Responses: []Response{
				{Status: 200, Description: "The resolved operation"},
				{Status: 404, Description: "Operation not found", Schema: ProblemRef()},
				{Status: 409, Description: "The operation cannot be resolved", Schema: ProblemRef()},
			},
		},
		"DELETE /api/v1/staff/operations/{id}": {
			Summary: "Delete a failed operation",
			Description: "Removes only a terminal failed queue row. Unknown and needs-review outcomes " +
				"remain visible until an operator resolves them.",
			Responses: []Response{
				{Status: 204, Description: "The failed operation was deleted"},
				{Status: 404, Description: "Operation not found", Schema: ProblemRef()},
				{Status: 409, Description: "Only failed operations can be deleted", Schema: ProblemRef()},
			},
		},
	}
}
