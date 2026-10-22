package i18n

// Resolution: which language is this request in?
//
// The order is fixed and it is the whole feature:
//
//	1. an explicit preference — the customer's stored choice, or a choice made on this request
//	2. the Accept-Language header
//	3. the default locale
//
// A stored preference beats the header because the customer chose it deliberately, and a header
// is a browser's guess that often follows the operating system rather than the person. The header
// beats the default because a visitor who has not signed in still has a language, and ignoring it
// would show an English page to someone whose browser says `de-DE` on every request.
//
// Two details that are easy to get wrong and are therefore explicit here:
//
//   - **A regional tag matches its base language.** A browser sends `de-DE`; an installation may
//     offer only `de`. Refusing the request and falling to English would be a bug a customer reads
//     as "this shop does not have German". So a candidate is tried exactly, then by its primary
//     subtag, and only then discarded.
//   - **q-values order the header, and `q=0` means "not this one".** `de;q=0.9, en;q=0.1` is a
//     preference for German; `de;q=0, en` means "anything but German". A parser that ignores
//     q-values picks the first tag it sees, which is right by accident for the common case and
//     wrong for every deliberate one.
//
// The result carries its own provenance (`Source`) so an operator can see why a visitor got the
// language they got, and so a test can prove the order instead of inferring it.

import (
	"context"
	"sort"
	"strconv"
	"strings"
)

// Source says which step of the chain answered.
type Source string

const (
	// SourcePreference means an explicit choice — stored on the account, or sent with the request.
	SourcePreference Source = "preference"
	// SourceHeader means the Accept-Language header answered.
	SourceHeader Source = "header"
	// SourceDefault means nothing else matched and the default locale was used.
	SourceDefault Source = "default"
)

// Resolution is the answer to "which language is this request in", with its reason.
type Resolution struct {
	// Locale is the resolved code. It is always an enabled locale.
	Locale string
	// Source is the step that produced it.
	Source Source
	// Candidate is the tag that matched, which differs from Locale when a regional tag resolved
	// to its base language: `de-DE` → `de`. Empty when the default answered.
	Candidate string
}

// Resolver resolves a locale for a request against the enabled set.
//
// It holds no state beyond the configuration it was built from, so it can be constructed per
// request cheaply and used concurrently.
type Resolver struct {
	// Default is the code used when nothing else matches.
	Default string
	// Enabled is the set a resolution may produce, in the order a switcher shows them. A
	// candidate that is not in this set is not a resolution: showing a customer a language the
	// installation does not offer is worse than showing them the default.
	Enabled []string
}

// NewResolver builds a resolver from the configured locales.
//
// `enabled` is a list of codes; an empty list falls back to the default, so a panel whose locales
// table is empty still renders rather than failing to choose a language.
func NewResolver(def string, enabled []string) Resolver {
	normalizedDefault, err := NormalizeCode(def)
	if err != nil || normalizedDefault == "" {
		normalizedDefault = DefaultCode
	}
	codes := make([]string, 0, len(enabled))
	for _, code := range enabled {
		if normalized, err := NormalizeCode(code); err == nil {
			codes = append(codes, normalized)
		}
	}
	if len(codes) == 0 {
		codes = []string{normalizedDefault}
	}
	return Resolver{Default: normalizedDefault, Enabled: codes}
}

// Resolve applies the chain: preference, then header, then default.
//
// `preference` may be empty ("this account never chose"). `header` is a raw Accept-Language value
// and may also be empty.
func (r Resolver) Resolve(preference, header string) Resolution {
	if code, ok := r.match(preference); ok {
		return Resolution{Locale: code, Source: SourcePreference, Candidate: strings.TrimSpace(preference)}
	}
	for _, candidate := range ParseAcceptLanguage(header) {
		if code, ok := r.match(candidate); ok {
			return Resolution{Locale: code, Source: SourceHeader, Candidate: candidate}
		}
	}
	// The default must be offered: NewResolver puts it in the set when the set is empty, and a
	// default that an operator disabled is refused by the schema, so this is the last resort for
	// a configuration that changed underneath a running process.
	if code, ok := r.match(r.Default); ok {
		return Resolution{Locale: code, Source: SourceDefault}
	}
	if len(r.Enabled) > 0 {
		return Resolution{Locale: r.Enabled[0], Source: SourceDefault}
	}
	return Resolution{Locale: DefaultCode, Source: SourceDefault}
}

// match reports whether a candidate names an enabled locale, exactly or by its base language.
//
// The base-language match is what makes `de-DE` resolve to `de` and `pt-BR` resolve to `pt`. The
// reverse direction is deliberately not tried: a request for `de` must not resolve to `de-AT`,
// because a shop that offers Austrian German has not thereby offered German.
func (r Resolver) match(candidate string) (string, bool) {
	normalized, err := NormalizeCode(candidate)
	if err != nil || normalized == "" {
		return "", false
	}
	for _, code := range r.Enabled {
		if code == normalized {
			return code, true
		}
	}
	base := normalized
	if i := strings.Index(base, "-"); i > 0 {
		base = base[:i]
	}
	if base != normalized {
		for _, code := range r.Enabled {
			if code == base {
				return code, true
			}
		}
	}
	return "", false
}

// Resolve reads the configuration and resolves one request's locale.
//
// This is the function an HTTP handler calls: it is one query for the locale list and one for the
// customer's stored preference, both of which are cheap and neither of which is on the money path.
func (s *Store) Resolve(ctx context.Context, preference, header string) (Resolution, error) {
	locales, err := s.Enabled(ctx)
	if err != nil {
		return Resolution{}, err
	}
	codes := make([]string, 0, len(locales))
	for _, l := range locales {
		codes = append(codes, l.Code)
	}
	return NewResolver(s.Default(ctx), codes).Resolve(preference, header), nil
}

// ParseAcceptLanguage turns a header value into candidate tags, best first.
//
// The grammar that matters: `tag;q=0.8` pairs, comma separated, where a higher q is preferred and
// q=0 means "not acceptable". A tag with no q is 1.0. The wildcard `*` is kept as a candidate so a
// resolver can treat it as "whatever the default is" — it is dropped here rather than ranked,
// because it names no language.
//
// Ties are broken by the order they appeared, which is what a client means when it writes two tags
// with the same weight: the first one is the preference.
func ParseAcceptLanguage(header string) []string {
	type candidate struct {
		tag   string
		q     float64
		order int
	}
	var candidates []candidate
	for i, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tag := part
		q := 1.0
		if semi := strings.Index(part, ";"); semi >= 0 {
			tag = strings.TrimSpace(part[:semi])
			params := part[semi+1:]
			for _, param := range strings.Split(params, ";") {
				param = strings.TrimSpace(param)
				if !strings.HasPrefix(strings.ToLower(param), "q=") {
					continue
				}
				if value, err := strconv.ParseFloat(strings.TrimSpace(param[2:]), 64); err == nil {
					q = value
				}
			}
		}
		if tag == "" || tag == "*" {
			continue
		}
		// A q outside [0,1] is a malformed header; treating it as 0 drops the tag rather than
		// letting a typo outrank a real preference.
		if q <= 0 {
			continue
		}
		candidates = append(candidates, candidate{tag: tag, q: q, order: i})
	}
	sort.SliceStable(candidates, func(a, b int) bool {
		if candidates[a].q != candidates[b].q {
			return candidates[a].q > candidates[b].q
		}
		return candidates[a].order < candidates[b].order
	})
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.tag)
	}
	return out
}
