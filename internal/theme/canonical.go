package theme

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Canonical form and digest.
//
// # Why a digest at all
//
// Three things need to answer "is this the same theme as that one": the cache key that turns a
// publish into a URL (`/theme/<revision>/theme.css`), the idempotency of an import (the same
// envelope imported twice must not create two revisions), and the history screen's "no change"
// detection. A digest answers all three with one value.
//
// # Why `encoding/json` is the canonical form
//
// It marshals object keys in sorted order and struct fields in declaration order, so the same
// document always produces the same bytes — which is the whole requirement. A separate
// canonicaliser would be a second serialiser that could disagree with the one that writes the
// database row, and then the digest would be a digest of something nobody stored.
//
// The consequence to remember: adding a field to Document changes every digest. That is
// acceptable because a digest is compared within a build (cache keys, duplicate imports), never
// across a deploy.

// Canonical renders a document as canonical JSON.
func Canonical(doc Document) ([]byte, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("theme: canonicalise: %w", err)
	}
	return raw, nil
}

// Digest returns the document's `sha256:<hex>` digest.
func Digest(doc Document) (string, error) {
	raw, err := Canonical(doc)
	if err != nil {
		return "", err
	}
	return DigestBytes(raw), nil
}

// DigestBytes returns `sha256:<hex>` for arbitrary bytes.
func DigestBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
