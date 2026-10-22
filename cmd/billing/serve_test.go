package main

import (
	"testing"

	"github.com/catalyst/billing/internal/store"
)

// TestServeConfigHonoursEnv pins the two settings that are read from the environment and were
// previously either misread or not read at all.
//
// Both are worth a test rather than a comment because each failure is silent:
//
//   - BILLING_EMBEDDED_WORKER was exported by three scripts and read by nothing, so the scripts
//     passed `false` harmlessly and would have been wrong the day someone passed `true`. The job
//     loop simply would not start inside `serve`, and the operator would conclude the jobs were
//     broken.
//   - BILLING_INSECURE_COOKIES is a kill switch, so `secureCookies` is its negation. A sign error
//     there is a security regression: cookies marked Secure on a plain-HTTP loopback panel are
//     never sent back, so sign-in silently fails, and cookies left non-Secure in production are
//     sent in the clear.
func TestServeConfigHonoursEnv(t *testing.T) {
	t.Run("embedded worker defaults to off", func(t *testing.T) {
		t.Setenv("BILLING_EMBEDDED_WORKER", "")
		if got := serveConfigFromEnv().embeddedWorker; got {
			t.Fatal("embeddedWorker defaulted to true; jobs must not share the serve process by default")
		}
	})

	for _, on := range []string{"1", "true", "yes"} {
		t.Run("embedded worker honours "+on, func(t *testing.T) {
			t.Setenv("BILLING_EMBEDDED_WORKER", on)
			if got := serveConfigFromEnv().embeddedWorker; !got {
				t.Fatalf("BILLING_EMBEDDED_WORKER=%q did not enable the embedded worker", on)
			}
		})
	}

	t.Run("secure cookies default on", func(t *testing.T) {
		t.Setenv("BILLING_INSECURE_COOKIES", "")
		if got := serveConfigFromEnv().secureCookies; !got {
			t.Fatal("secureCookies defaulted to false; the Secure flag must be on unless explicitly disabled")
		}
	})

	for _, on := range []string{"1", "true", "yes"} {
		t.Run("insecure cookies honoured "+on, func(t *testing.T) {
			t.Setenv("BILLING_INSECURE_COOKIES", on)
			if got := serveConfigFromEnv().secureCookies; got {
				t.Fatalf("BILLING_INSECURE_COOKIES=%q did not disable the Secure flag", on)
			}
		})
	}

	// A value that is not a recognised truthy token must not enable a kill switch, because the
	// switches guard destructive subsystems: "off", "0" and a typo all mean "leave it on".
	for _, off := range []string{"0", "false", "no", "off", "banana"} {
		t.Run("not-truthy ignored: "+off, func(t *testing.T) {
			t.Setenv("BILLING_EMBEDDED_WORKER", off)
			t.Setenv("BILLING_INSECURE_COOKIES", off)
			cfg := serveConfigFromEnv()
			if cfg.embeddedWorker {
				t.Fatalf("BILLING_EMBEDDED_WORKER=%q enabled the worker", off)
			}
			if !cfg.secureCookies {
				t.Fatalf("BILLING_INSECURE_COOKIES=%q disabled Secure cookies", off)
			}
		})
	}
}

// TestServeConfigDefaults keeps the documented defaults honest.
//
// .env.example states each default in prose, so a change here that is not mirrored there makes
// the two disagree — and the operator trusts the example file, not the source.
func TestServeConfigDefaults(t *testing.T) {
	t.Setenv("BILLING_HTTP_ADDR", "")
	t.Setenv("BILLING_PUBLIC_URL", "")
	t.Setenv("BILLING_LOG_LEVEL", "")
	t.Setenv("BILLING_LOG_FORMAT", "")
	t.Setenv("BILLING_TRUSTED_PROXIES", "")

	cfg := serveConfigFromEnv()
	if cfg.addr != "127.0.0.1:8080" {
		t.Errorf("addr default = %q, want 127.0.0.1:8080 (documented in .env.example)", cfg.addr)
	}
	if cfg.logFormat != "text" {
		t.Errorf("logFormat default = %q, want text", cfg.logFormat)
	}
	if cfg.logLevel != "info" {
		t.Errorf("logLevel default = %q, want info", cfg.logLevel)
	}
	// An empty trusted-proxy list is the safe default: no forwarding header is believed, so no
	// client can choose its own throttling bucket or spoof its address.
	if cfg.trustedProxies != "" {
		t.Errorf("trustedProxies default = %q, want empty", cfg.trustedProxies)
	}
}

// TestPublicBaseURLIsDerived keeps the notification and redirect links pointing at the panel.
func TestPublicBaseURLIsDerived(t *testing.T) {
	t.Setenv("BILLING_PUBLIC_URL", "https://billing.example.test/")
	if got := publicBaseURL(); got != "https://billing.example.test" {
		t.Fatalf("publicBaseURL() = %q, want the trailing slash trimmed", got)
	}
}

// TestStoreConfigRequiresDatabase documents that a missing database is a hard failure, not a
// silent fallback to a default DSN: guessing a connection string is how a panel talks to the
// wrong database.
func TestStoreConfigRequiresDatabase(t *testing.T) {
	t.Setenv("BILLING_DATABASE_URL", "")
	if cfg := store.ConfigFromEnv(); cfg.URL != "" {
		t.Fatalf("ConfigFromEnv invented a database URL %q from an empty environment", cfg.URL)
	}
}
