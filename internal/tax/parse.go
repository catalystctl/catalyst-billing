package tax

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/money"
)

// Parsing a rate document.
//
// # The two formats, and why they are generic
//
// There is no vendor schema in here. No public tax authority publishes a documented, stable
// "rate list" JSON that this repository could verify without network access, and inventing one
// from memory would produce an importer that accepts a document, imports nothing, and reports
// success — the worst possible outcome, because the operator would believe the rates were current.
//
// So the supported formats are the *rate table's own column names*, in two encodings:
//
//	JSON: an array of objects, or an object with a "rates" array, e.g.
//	  [{"country_code":"DE","rate":"0.19","tax_class":"standard","inclusive":true}]
//
//	CSV: a header row naming the same columns, e.g.
//	  country_code,region_code,tax_class,rate,inclusive,effective_from,name
//	  DE,,standard,0.19,true,2024-01-01,VAT - Germany
//
// Recognised columns are `name`, `country_code`, `region_code`, `tax_class`, `rate`, `inclusive`
// and `effective_from`. `country_code` and `rate` are required; the rest have defaults. An
// unrecognised column is ignored rather than refused, because a vendor export always carries
// columns this importer has no use for (an id, a description, a timestamp) and refusing the whole
// document over one of them would make the format unusable.
//
// A rate is a *fraction*: 0.19 is nineteen per cent. A bare JSON number is read as that same
// fraction, never as a percentage, and `19` is therefore rejected (1900%) rather than silently
// charged. `"19%"` is accepted because the rate table's own form accepts it.

// Parse reads a document into rows.
//
// It returns the rows that were well formed, one rejection per row that was not, and an error
// only when the document cannot be read at all (invalid JSON, no CSV header, a missing required
// column). That split is the whole point of the per-row rule: a document with three bad rows out
// of two hundred is a document with three findings, not a failed import.
func Parse(format string, body []byte) ([]Row, []Rejection, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", FormatJSON:
		return parseJSON(body)
	case FormatCSV:
		return parseCSV(body)
	default:
		return nil, nil, fmt.Errorf("%w: format %q is not one of %s",
			ErrBadDocument, format, strings.Join(Formats, ", "))
	}
}

// parseJSON reads an array of objects, or an object carrying one under "rates".
func parseJSON(body []byte) ([]Row, []Rejection, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	// UseNumber keeps a rate out of a float64. 0.1 is not representable in binary floating point,
	// and a tax rate is money.
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, badDocument(0, "the document is not valid JSON: %v", err)
	}
	var items []any
	switch typed := doc.(type) {
	case []any:
		items = typed
	case map[string]any:
		rates, ok := typed["rates"]
		if !ok {
			return nil, nil, badDocument(0,
				"the document is an object with no \"rates\" array; expected an array of rate objects")
		}
		list, ok := rates.([]any)
		if !ok {
			return nil, nil, badDocument(0, "\"rates\" is not an array")
		}
		items = list
	default:
		return nil, nil, badDocument(0,
			"the document is neither an array of rate objects nor an object with a \"rates\" array")
	}
	if len(items) == 0 {
		// An empty array is a legitimate answer ("no rates"), but it is also what a broken export
		// produces. It is not an error here: the preview says "0 rows", which is the honest
		// report, and the operator can see that nothing would change.
		return nil, nil, nil
	}

	rows := make([]Row, 0, len(items))
	rejections := make([]Rejection, 0)
	seen := make(map[string]int, len(items))
	for index, item := range items {
		line := index + 1
		object, ok := item.(map[string]any)
		if !ok {
			rejections = append(rejections, Rejection{
				Line: line, Key: fmt.Sprintf("#%d", line), Code: CodeFieldCount,
				Detail: "the entry is not an object",
			})
			continue
		}
		raw := rawRow{
			Line:          line,
			Name:          jsonString(object["name"]),
			Country:       jsonString(object["country_code"]),
			Region:        jsonString(object["region_code"]),
			Class:         jsonString(object["tax_class"]),
			Rate:          jsonString(object["rate"]),
			Inclusive:     jsonString(object["inclusive"]),
			EffectiveFrom: jsonString(object["effective_from"]),
		}
		appendRow(&rows, &rejections, seen, raw)
	}
	return rows, rejections, nil
}

// parseCSV reads a header row and the rows beneath it.
func parseCSV(body []byte) ([]Row, []Rejection, error) {
	reader := csv.NewReader(bytes.NewReader(body))
	reader.TrimLeadingSpace = true
	// Ragged rows are a per-row finding rather than a reason to stop reading: a vendor export
	// with one trailing comma is common, and refusing the document over it helps nobody.
	reader.FieldsPerRecord = -1
	reader.ReuseRecord = false

	header, err := reader.Read()
	if err == io.EOF {
		return nil, nil, badDocument(0, "the document is empty")
	}
	if err != nil {
		return nil, nil, badDocument(0, "the header row cannot be read: %v", err)
	}
	columns := make(map[string]int, len(header))
	for index, name := range header {
		key := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(name, "\ufeff")))
		if key == "" {
			continue
		}
		if _, dup := columns[key]; !dup {
			columns[key] = index
		}
	}
	for _, required := range []string{"country_code", "rate"} {
		if _, ok := columns[required]; !ok {
			return nil, nil, badDocument(0,
				"the header has no %q column; expected country_code,rate at minimum", required)
		}
	}

	rows := make([]Row, 0, 64)
	rejections := make([]Rejection, 0)
	seen := make(map[string]int, 64)
	line := 1
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		line++
		if err != nil {
			// A quoting error is that row's problem. The reader can continue, so the import does:
			// one unquoted comma in row 40 must not hide rows 41 to 200.
			rejections = append(rejections, Rejection{
				Line: line, Key: fmt.Sprintf("#%d", line), Code: CodeFieldCount,
				Detail: fmt.Sprintf("the row cannot be read as CSV: %v", err),
			})
			continue
		}
		if len(record) != len(header) {
			rejections = append(rejections, Rejection{
				Line: line, Key: fmt.Sprintf("#%d", line), Code: CodeFieldCount,
				Detail: fmt.Sprintf("the row has %d fields, the header has %d", len(record), len(header)),
			})
			continue
		}
		raw := rawRow{
			Line:          line,
			Name:          column(record, columns, "name"),
			Country:       column(record, columns, "country_code"),
			Region:        column(record, columns, "region_code"),
			Class:         column(record, columns, "tax_class"),
			Rate:          column(record, columns, "rate"),
			Inclusive:     column(record, columns, "inclusive"),
			EffectiveFrom: column(record, columns, "effective_from"),
		}
		if strings.TrimSpace(raw.Country) == "" && strings.TrimSpace(raw.Rate) == "" &&
			strings.TrimSpace(raw.Name) == "" {
			// A blank line at the end of an export is not a rejected rate.
			continue
		}
		appendRow(&rows, &rejections, seen, raw)
	}
	return rows, rejections, nil
}

// rawRow is one row of the document as text, before validation.
type rawRow struct {
	Line          int
	Name          string
	Country       string
	Region        string
	Class         string
	Rate          string
	Inclusive     string
	EffectiveFrom string
}

// appendRow validates one row and files it as either a rate or a rejection.
//
// The duplicate check lives here because "the same jurisdiction twice in one document" is not a
// malformed row: it is an ambiguous document. The first statement wins and the second is refused
// by name, which is the only defensible answer — taking the last one would make the imported rate
// depend on row order, and the resolver already refuses to do that.
func appendRow(rows *[]Row, rejections *[]Rejection, seen map[string]int, raw rawRow) {
	row, rejection := validate(raw)
	if rejection != nil {
		*rejections = append(*rejections, *rejection)
		return
	}
	key := row.Jurisdiction()
	if first, dup := seen[key]; dup {
		*rejections = append(*rejections, Rejection{
			Line: raw.Line, Key: key, Code: CodeDuplicateInSource,
			Detail: fmt.Sprintf("%s already appears on line %d; the first row wins", key, first),
		})
		return
	}
	seen[key] = raw.Line
	*rows = append(*rows, row)
}

// validate turns one raw row into a rate, or into the reason it is not one.
func validate(raw rawRow) (Row, *Rejection) {
	reject := func(code, format string, args ...any) (Row, *Rejection) {
		return Row{}, &Rejection{
			Line:   raw.Line,
			Key:    strings.TrimSpace(raw.Country),
			Code:   code,
			Detail: fmt.Sprintf(format, args...),
		}
	}

	country := strings.ToUpper(strings.TrimSpace(raw.Country))
	if country == "" {
		return reject(CodeMissingCountry, "the row has no country_code")
	}
	if !validCountry(country) {
		return reject(CodeInvalidCountry,
			"country_code %q is not two upper-case letters or %q", raw.Country, WildcardCountry)
	}
	region := strings.ToUpper(strings.TrimSpace(raw.Region))
	if country == WildcardCountry && region != "" {
		return reject(CodeRegionOnWildcard,
			"a wildcard country cannot name a region, and this row names %q", raw.Region)
	}
	if len(region) > 8 {
		return reject(CodeInvalidRegion, "region_code %q is longer than 8 characters", raw.Region)
	}
	class := strings.TrimSpace(raw.Class)
	if class == "" {
		class = DefaultTaxClass
	}
	if len(class) > 64 {
		return reject(CodeInvalidClass, "tax_class is longer than 64 characters")
	}

	rawRate := strings.TrimSpace(raw.Rate)
	if rawRate == "" {
		return reject(CodeMissingRate, "the row has no rate")
	}
	ratio, err := money.ParseRatio(rawRate)
	if err != nil {
		return reject(CodeInvalidRate,
			"rate %q is not an exact number (use \"0.19\", \"19%%\" or \"1/5\")", rawRate)
	}
	if ratio.IsNegative() {
		return reject(CodeInvalidRate, "rate %q is negative", rawRate)
	}
	if ratio.Float64() > 1 {
		// A rate above 100% is almost always a percentage that was not divided by 100 — `19`
		// meaning nineteen per cent. Charging 1900% is worse than refusing the row, so the row is
		// refused and the detail says which form is expected.
		return reject(CodeInvalidRate,
			"rate %q is more than 100%%; a rate is a fraction (0.19) or a percentage (19%%)", rawRate)
	}

	inclusive := false
	switch strings.ToLower(strings.TrimSpace(raw.Inclusive)) {
	case "", "false", "0", "no", "n":
		inclusive = false
	case "true", "1", "yes", "y":
		inclusive = true
	default:
		return reject(CodeInvalidInclusive,
			"inclusive %q is not true or false", raw.Inclusive)
	}

	var from time.Time
	if rawFrom := strings.TrimSpace(raw.EffectiveFrom); rawFrom != "" {
		parsed, err := parseEffectiveFrom(rawFrom)
		if err != nil {
			return reject(CodeInvalidDate,
				"effective_from %q is not a date (use 2024-01-01 or an RFC 3339 instant)", rawFrom)
		}
		from = parsed
	}

	row := Row{
		Line:          raw.Line,
		Name:          strings.TrimSpace(raw.Name),
		CountryCode:   country,
		RegionCode:    region,
		TaxClass:      class,
		Rate:          rawRate,
		Inclusive:     inclusive,
		EffectiveFrom: from,
	}
	if row.Name == "" {
		// The rate table requires a name, and a document that carries none still has to produce a
		// row an operator can recognise. The jurisdiction is the one label the document did state.
		row.Name = row.Jurisdiction()
	}
	return row, nil
}

// parseEffectiveFrom accepts a date or a full instant.
func parseEffectiveFrom(raw string) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed.UTC(), nil
	}
	if parsed, err := time.Parse("2006-01-02", raw); err == nil {
		// A date is a day, and the rate applies from the start of that day in UTC. Local midnight
		// would make the same document mean different instants in two deployments.
		return parsed.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("tax: %q is not a date", raw)
}

// jsonString renders a JSON value as the text the validator reads.
//
// A rate is allowed to arrive as a JSON number, and `json.Number` keeps the literal the document
// wrote, so "0.19" is never routed through a float64.
func jsonString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case json.Number:
		return typed.String()
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return fmt.Sprint(typed)
	}
}

func column(record []string, columns map[string]int, name string) string {
	index, ok := columns[name]
	if !ok || index >= len(record) {
		return ""
	}
	return record[index]
}
