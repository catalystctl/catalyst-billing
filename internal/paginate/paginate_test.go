package paginate_test

import (
	"errors"
	"testing"

	"github.com/catalyst/billing/internal/paginate"
)

func TestCursorRoundTrip(t *testing.T) {
	cases := []struct{ sortKey, id string }{
		{"2024-01-02T03:04:05.000000006Z", "0f8fad5b-d9cb-469f-a165-70867728950e"},
		{"customer@example.test", "a"},
		{"", "b"},
		{"-12.5", "c"},
	}
	for _, c := range cases {
		encoded := paginate.Encode(c.sortKey, c.id)
		gotKey, gotID, err := paginate.Decode(encoded)
		if err != nil {
			t.Fatalf("decode %q: %v", encoded, err)
		}
		if gotKey != c.sortKey || gotID != c.id {
			t.Fatalf("round trip = (%q, %q), want (%q, %q)", gotKey, gotID, c.sortKey, c.id)
		}
	}
}

// An empty cursor is the first page, not an error: a client that omits the parameter
// must not have to special-case it.
func TestEmptyCursorIsFirstPage(t *testing.T) {
	key, id, err := paginate.Decode("")
	if err != nil {
		t.Fatalf("empty cursor: %v", err)
	}
	if key != "" || id != "" {
		t.Fatalf("empty cursor decoded to (%q, %q)", key, id)
	}
}

// A malformed cursor must be an error rather than a silent reset to page one: a silent
// reset makes a client loop forever over the first page.
func TestMalformedCursorRejected(t *testing.T) {
	for _, bad := range []string{
		"not-base64!!",
		"YWJj",                    // decodes to "abc": no separator
		"YWJjAA==",                // separator with no id
		string(make([]byte, 600)), // absurdly long
	} {
		if _, _, err := paginate.Decode(bad); !errors.Is(err, paginate.ErrInvalidCursor) {
			t.Fatalf("decode(%q) error = %v, want ErrInvalidCursor", bad, err)
		}
	}
}

func TestClamp(t *testing.T) {
	cases := map[int]int{
		-5:   paginate.DefaultLimit,
		0:    paginate.DefaultLimit,
		1:    1,
		25:   25,
		100:  100,
		101:  paginate.MaxLimit,
		1000: paginate.MaxLimit,
	}
	for in, want := range cases {
		if got := paginate.Clamp(in); got != want {
			t.Fatalf("Clamp(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestParseLimit(t *testing.T) {
	if n, err := paginate.ParseLimit(""); err != nil || n != paginate.DefaultLimit {
		t.Fatalf("empty limit = (%d, %v)", n, err)
	}
	if n, err := paginate.ParseLimit("1000"); err != nil || n != paginate.MaxLimit {
		t.Fatalf("limit 1000 = (%d, %v), want the maximum", n, err)
	}
	if _, err := paginate.ParseLimit("abc"); err == nil {
		t.Fatal("a non-numeric limit was accepted")
	}
}

// Next must not hand out a cursor for a partial page: a client that follows it performs
// one more request that returns nothing.
func TestNextOnlyWhenPageIsFull(t *testing.T) {
	rows := []string{"a", "b"}
	key := func(string) string { return "k" }
	id := func(s string) string { return s }

	if c := paginate.Next(rows, 5, key, id); c != nil {
		t.Fatalf("partial page produced a cursor: %q", *c)
	}
	if c := paginate.Next(rows, 2, key, id); c == nil {
		t.Fatal("a full page produced no cursor")
	} else if k, i, err := paginate.Decode(*c); err != nil || k != "k" || i != "b" {
		t.Fatalf("cursor = (%q, %q, %v), want the last row", k, i, err)
	}
	if c := paginate.Next([]string{}, 5, key, id); c != nil {
		t.Fatalf("an empty page produced a cursor: %q", *c)
	}
}
