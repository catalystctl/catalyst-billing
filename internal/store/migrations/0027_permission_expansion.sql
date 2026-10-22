-- 0027_permission_expansion.sql
--
-- Ten permissions, and the decision that shaped each one.
--
-- The permission set is a closed list in code (`authn.AllPermissions`), and this migration
-- mirrors the change into `role_permissions` because `internal/authn`'s role tests compare
-- the two. That test is the reason this file exists at all: without it a code change would
-- silently widen or narrow what existing staff can do, and nothing would fail.
--
-- Why each one is a separate grant rather than borrowing an existing one:
--
--   order:create:self      Placing an order is not paying one. A zero-total order never
--                          produces an invoice, so checkout used to borrow invoice:pay:self
--                          to buy a free product — a capability the action never exercises.
--   service:write:self     Renaming your own service is not reading it. An operator may want
--                          to freeze a label during a dispute and still show the service.
--   paymentmethod:read:self  / write:self
--                          A saved payment method is a provider token. Reading it and
--                          replacing it are the customer's own acts on their own record.
--   credit:read:all        Reading a balance is an audit question; granting credit
--                          (credit:write) is a money movement. Support needs the first.
--   service:write:all      The operator-facing metadata on a service (label, description,
--                          custom properties) is not what the customer is billed, so it is
--                          not service:upgrade.
--   i18n:write             Translations are content, not access.
--   import:write           An importer writes customers, orders and invoices in bulk — a
--                          bigger hammer than customer:write:all.
--   oauth:manage           Registering an OAuth2 client is an access decision: the client
--                          acts on behalf of a user.
--   update:manage          The one permission that can replace the running binary.
--
-- The four system roles are immutable by trigger (0026), so this INSERT is the only way
-- their sets can grow — and it is reviewed in a diff, which is the point.

INSERT INTO role_permissions (role_id, permission)
SELECT r.id, seed.permission
FROM roles r
JOIN (VALUES
    ('administrator',  'credit:read:all'),
    ('administrator',  'service:write:all'),
    ('administrator',  'i18n:write'),
    ('administrator',  'import:write'),
    ('administrator',  'oauth:manage'),
    ('administrator',  'update:manage'),
    ('billing',        'credit:read:all'),
    ('billing',        'service:write:all'),
    ('billing',        'i18n:write'),
    ('billing',        'import:write'),
    ('support',        'credit:read:all'),
    ('support',        'service:write:all'),
    ('infrastructure', 'service:write:all'),
    ('infrastructure', 'update:manage')
) AS seed(role_name, permission) ON seed.role_name = r.name
WHERE r.is_system
ON CONFLICT DO NOTHING;
