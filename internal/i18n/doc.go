// Package i18n owns locales, the message catalog and the resolution of "which language is
// this request in".
//
// Paymenter ships a language file per locale in its source tree, so adding a language is a
// pull request, a release and a deploy. Here a locale is a row and a message is a row, which
// moves translation to an operator's screen. Three rules shape the code:
//
//  1. **The default is enforced by the database, not by this package.** `locales_one_default`
//     is a partial unique index, so "two defaults" cannot be stored; a `CHECK` requires the
//     default to be enabled. This package demotes the incumbent in the same transaction that
//     promotes a replacement, which is the only thing an application has to get right.
//
//  2. **Fallback is a read-time merge, never a stored copy.** A key missing in a locale
//     resolves to the default locale's text and then to the key itself — never to an empty
//     string, because a blank label renders as a design choice rather than as a bug. Storing
//     the English text into every locale would make "untranslated" indistinguishable from
//     "translated to the same words", so coverage could not be reported and the editor could
//     not show what still needs work.
//
//  3. **Resolution is ordered and observable.** A stored preference wins over the
//     `Accept-Language` header, because the customer chose it; the header wins over the
//     default. `Resolution.Source` says which of the three answered, so an operator can see
//     why a visitor got the language they got, and a test can prove the order rather than
//     infer it.
//
// The package reads and writes configuration only. It never touches a document, a ledger
// entry or an audit row: a locale changes how a page reads, never what a number means.
package i18n
