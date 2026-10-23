package i18n

// Machine-readable refusals: which field, which stable code, in which catalogue key.
//
// # The boundary this file draws
//
// Go error strings are English by construction. There is no honest way to translate
// `fmt.Errorf("the default locale must be enabled")` for a Spanish reader without translating the
// Go that produced it, and a project that tries ends up with two sources of truth for every
// sentence: the code and the catalogue, drifting.
//
// What *is* translatable is a stable identifier. Every problem this API returns already carries a
// machine `code`; a client can render `error.<code>` from the message catalogue in the reader's
// locale, and fall back to the server's English `detail` when that key has not been translated
// yet. For a validation failure the client also needs to know *which* field failed, because "this
// field is required" is a different sentence per field in many languages, and one generic sentence
// with no field is a sentence the user cannot act on.
//
// So a refusal may carry:
//
//   - a problem `code` (stable, documented, branchable), and
//   - a list of `FieldProblem`s, each naming the input field and a stable per-field code.
//
// The English `detail` stays. It is the developer-facing explanation and the client's last-resort
// fallback. That is the whole boundary: **codes translate, sentences exist for humans reading
// English**.

import (
	"errors"
	"fmt"
)

// FieldProblem is one input field a refusal is about.
//
// Code is a stable identifier, not a sentence: `required`, `invalid`, `duplicate`, `unknown`. The
// client composes the message from it, so it must not be reworded when the English text is.
type FieldProblem struct {
	Field string `json:"field"`
	Code  string `json:"code"`
	// Detail is the English explanation for this field, when there is one worth showing. It is
	// optional because a client that knows the code does not need it.
	Detail string `json:"detail,omitempty"`
}

// Per-field codes. They are deliberately few: a client that has to know forty codes to render a
// form has not gained a translatable interface, it has gained a lookup table.
const (
	FieldRequired  = "required"
	FieldInvalid   = "invalid"
	FieldDuplicate = "duplicate"
	FieldUnknown   = "unknown"
)

// fieldError is an ErrInvalidInput that names the field it is about.
type fieldError struct {
	problem FieldProblem
}

func (e fieldError) Error() string { return e.problem.Detail }

// Is keeps errors.Is(err, ErrInvalidInput) true, so every existing handler mapping is unchanged.
func (e fieldError) Is(target error) bool { return target == ErrInvalidInput }

// invalidField refuses one input field.
//
// It replaces invalid() where the field is knowable. A refusal that cannot name a field still uses
// invalid(): inventing a field name for a cross-field rule (the default must be enabled) would
// point the client's error at a control that is not the problem.
func invalidField(field, code, format string, args ...any) error {
	return fieldError{problem: FieldProblem{
		Field:  field,
		Code:   code,
		Detail: fmt.Sprintf(format, args...),
	}}
}

// FieldProblems returns the per-field problems an error carries, or nil.
//
// It walks the error chain because a caller may have wrapped the refusal with context ("i18n:
// install locale: …"). errors.As is what makes that work without the handler knowing the type.
func FieldProblems(err error) []FieldProblem {
	var fe fieldError
	if errors.As(err, &fe) {
		return []FieldProblem{fe.problem}
	}
	return nil
}

// ProblemKey is the message-catalogue key that translates one problem code.
//
// The `error.` prefix is a namespace, not decoration: it is what makes "translate the error
// messages" a prefix query on /locales, and what stops a problem code from colliding with a
// screen's own key (`not_found` is also a plausible storefront label).
//
// A code is already lower snake case by convention; ProblemKey does not rewrite it, because a
// key that no longer matches the code is a key nobody can find from the response.
func ProblemKey(code string) string { return "error." + code }
