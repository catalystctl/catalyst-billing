package operations

import "errors"

// A job run has three honest outcomes and needs all three. "failed" means something went wrong;
// "ok" means the job ran; "skipped" means the job deliberately did nothing AND the reason matters.
//
// A tick that stood down because another worker held the sweep is none of the first two. Recording
// it as `ok` hides that the sweep did not run; recording it as `failed` puts a red row in the
// failure viewer, which is how an operator learns to ignore red rows.
//
// The type lives here rather than in the package that writes run history because this package is
// what *produces* the decision: a job knows why it did nothing, and the recorder only needs to
// recognise it. Keeping it here also means `operations` does not have to import the HTTP layer to
// say "I skipped".

// SkipError marks a run as skipped rather than failed or ok.
type SkipError struct {
	Reason string
}

func (e *SkipError) Error() string { return "skipped: " + e.Reason }

// Skip builds a skip whose reason becomes the run's detail.
func Skip(reason string) error { return &SkipError{Reason: reason} }

// SkipReason reports whether err is a skip, and why.
func SkipReason(err error) (string, bool) {
	var s *SkipError
	if errors.As(err, &s) {
		return s.Reason, true
	}
	return "", false
}
