package tax

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/commerce"
	"github.com/catalyst/billing/internal/store"
)

// Fetching is a real HTTP GET, bounded in every dimension a remote document can exceed.
//
// The bounds are not decoration: this URL is configuration an operator types, so it can point at
// anything, and a fetch that can hang for an hour inside a worker tick or buffer a gigabyte is a
// denial of service with a settings form. So the client has a timeout, the body has a ceiling, and
// the status has to be a 2xx.

// DefaultFetchTimeout bounds one fetch. A rate document is a few kilobytes; a source that cannot
// answer in this long is a source that is down.
const DefaultFetchTimeout = 20 * time.Second

// DefaultMaxBody bounds the response. The largest published rate table in the world is far below
// this; the ceiling exists so a misconfigured URL cannot fill memory.
const DefaultMaxBody = 4 << 20

// userAgent identifies the panel to the source, so an operator running a feed can see who polled
// it. It is deliberately not a browser's.
const userAgent = "catalyst-billing-tax-import/1"

// defaultClient is shared by every Importer the HTTP layer builds.
//
// One client, not one per request: a panel that fetches the same source from a preview and then an
// import would otherwise open a new connection pool for each, and the source would see a new TCP
// handshake per click. The timeout is the bound; the transport is the standard one.
var defaultClient = &http.Client{Timeout: DefaultFetchTimeout}

// Importer is the import surface: one configured source, one rate table, one clock.
//
// Client and Now are fields rather than parameters so a test can point the importer at an
// `httptest` server and pin the instant a rate takes effect. Neither is a hook for production
// code: `New` fills both.
type Importer struct {
	pool     *store.Pool
	commerce *commerce.Store
	// Client performs the fetch. It must not be nil.
	Client *http.Client
	// MaxBody bounds a response body.
	MaxBody int64
	// Now is the clock used for "effective from when the import ran" and for the fetched-at
	// stamp. Injectable so a test can prove a scheduled refresh is due.
	Now func() time.Time
}

// New constructs an Importer against a pool.
func New(pool *store.Pool) *Importer {
	i := &Importer{
		pool:     pool,
		Client:   defaultClient,
		MaxBody:  DefaultMaxBody,
		Now:      func() time.Time { return time.Now().UTC() },
		commerce: commerce.New(pool),
	}
	// One clock for the importer and for the rate store: the store decides whether a rate has
	// taken effect, and it must be deciding with the same "now" the import used to resolve the
	// effective date. Two clocks here would be two answers to "is this rate live".
	i.commerce.Now = func() time.Time { return i.now() }
	return i
}

func (i *Importer) now() time.Time {
	if i == nil || i.Now == nil {
		return time.Now().UTC()
	}
	return i.Now().UTC()
}

func (i *Importer) maxBody() int64 {
	if i == nil || i.MaxBody <= 0 {
		return DefaultMaxBody
	}
	return i.MaxBody
}

func (i *Importer) client() *http.Client {
	if i == nil || i.Client == nil {
		return &http.Client{Timeout: DefaultFetchTimeout}
	}
	return i.Client
}

// Fetch retrieves the document the source names.
//
// It is exported because the screen offers "test the source" as its own action: an operator who
// has just typed a URL wants to know it answers before they preview what it says.
func (i *Importer) Fetch(ctx context.Context, src Source) (fetchPayload, error) {
	format := src.Format
	if format == "" {
		format = FormatJSON
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return fetchPayload{}, fmt.Errorf("%w: %v", ErrFetch, err)
	}
	req.Header.Set("User-Agent", userAgent)
	if format == FormatCSV {
		req.Header.Set("Accept", "text/csv, text/plain;q=0.9, */*;q=0.1")
	} else {
		req.Header.Set("Accept", "application/json, */*;q=0.1")
	}

	resp, err := i.client().Do(req)
	if err != nil {
		return fetchPayload{}, fmt.Errorf("%w: %v", ErrFetch, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The body is not echoed: a source that answers 500 with an HTML error page would put
		// that page in a run report, and the status is the part an operator can act on.
		return fetchPayload{}, fmt.Errorf("%w: the source answered %s", ErrFetch, resp.Status)
	}

	limit := i.maxBody()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fetchPayload{}, fmt.Errorf("%w: %v", ErrFetch, err)
	}
	if int64(len(body)) > limit {
		return fetchPayload{}, fmt.Errorf("%w: the document is larger than %d bytes", ErrFetch, limit)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return fetchPayload{}, fmt.Errorf("%w: the document is empty", ErrFetch)
	}
	return fetchPayload{
		URL:       src.URL,
		Format:    format,
		Body:      body,
		FetchedAt: i.now(),
	}, nil
}

// readSource loads the configured source and refuses a disabled one.
//
// A disabled source is not fetched by anyone: not by the preview, not by the import, not by the
// worker. The switch means "stop calling this document", and a switch that only stops *some*
// callers is not a switch.
func (i *Importer) readSource(ctx context.Context) (Source, error) {
	src, err := i.Source(ctx)
	if err != nil {
		return Source{}, err
	}
	if !src.Enabled {
		return Source{}, ErrSourceDisabled
	}
	return src, nil
}
