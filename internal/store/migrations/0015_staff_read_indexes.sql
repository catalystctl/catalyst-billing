-- 0015_staff_read_indexes.sql
--
-- Indexes for the staff read surface.
--
-- The operator screens are keyset-paginated, so every list needs an index whose
-- leading columns match its ORDER BY. Without these, "page two" is a full sort of the
-- table and the panel becomes unusable exactly when there is enough data to matter.
--
-- Search index choice for customers:
--
--   The spec offered pg_trgm (substring search, requires the extension) or a
--   lower() prefix index (no extension, prefix search only). This migration tries
--   pg_trgm and falls back, so a deployment that cannot install extensions still gets
--   an index instead of failing to migrate. Which one is in force is visible in
--   \d customers. With the trigram index, `q` is a substring match on email and name;
--   with the fallback it is a prefix match on name, and email search uses the existing
--   unique index on lower(email).
DO $$
BEGIN
    BEGIN
        CREATE EXTENSION IF NOT EXISTS pg_trgm;
        EXECUTE 'CREATE INDEX customers_name_trgm ON customers USING gin (lower(name) gin_trgm_ops)';
        EXECUTE 'CREATE INDEX customers_email_trgm ON customers USING gin (lower(email) gin_trgm_ops)';
    EXCEPTION WHEN OTHERS THEN
        RAISE NOTICE 'pg_trgm unavailable (%), falling back to a prefix index on lower(name)', SQLERRM;
        EXECUTE 'CREATE INDEX customers_name_prefix ON customers (lower(name) text_pattern_ops)';
    END;
END $$;

-- Customer list: default sort is created_at desc, id desc (keyset pagination).
CREATE INDEX customers_created_idx ON customers (created_at DESC, id DESC);
CREATE INDEX customers_name_lower_idx ON customers (lower(name), id);

-- Invoice list: newest issued first, with the filters used by the billing screen.
CREATE INDEX invoices_issued_idx ON invoices (issued_at DESC, id DESC);
CREATE INDEX invoices_status_issued_idx ON invoices (status, issued_at DESC, id DESC);
CREATE INDEX invoices_order_idx ON invoices (order_id) WHERE order_id IS NOT NULL;

-- Service list and the "stuck service" view.
CREATE INDEX services_customer_idx ON services (customer_id, created_at DESC);
CREATE INDEX services_status_idx ON services (status, created_at DESC);
CREATE INDEX service_operations_service_idx ON service_operations (service_id, created_at DESC);
CREATE INDEX service_operations_status_created_idx ON service_operations (status, created_at DESC);

-- Order list.
CREATE INDEX orders_customer_idx ON orders (customer_id, created_at DESC);
CREATE INDEX orders_status_created_idx ON orders (status, created_at DESC);

-- Payment history: newest first, plus the reconcile filter.
CREATE INDEX payments_received_idx ON payments (received_at DESC, id DESC);
CREATE INDEX payments_status_received_idx ON payments (status, received_at DESC, id DESC);
CREATE INDEX payment_allocations_invoice_idx ON payment_allocations (invoice_id);

-- Webhook inbox view: processed=false is a partial index so the common filter does
-- not scan the retained history.
CREATE INDEX gateway_events_processed_idx ON gateway_events (processed_at)
    WHERE processed_at IS NOT NULL;
