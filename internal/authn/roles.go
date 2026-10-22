package authn

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Editable roles (IDN-13, ADM-13).
//
// The four roles in authz.go encode a separation of duties the approval flow depends on:
// billing can refund and cannot terminate, infrastructure the reverse. Those four stay
// system roles and cannot be edited, which is why this file adds a *table* beside the code
// map rather than replacing it.
//
// The split is:
//
//   - A **system** role's permission set is whatever rolePermissions says. The database
//     seeds the same set so an access review can be answered with SQL, and a test compares
//     the two so they cannot drift.
//   - A **custom** role's permission set is rows in role_permissions. It may only contain
//     permissions from AllPermissions(): a permission no handler checks is a permission
//     that does nothing, and storing one would let an operator believe they granted
//     something.
//   - A user's effective permissions are the union over the roles they hold. For a system
//     role that is the code map (the source of truth); for a custom role it is the table.
//     Resolving both through one accessor is what makes the Authorizer unchanged.

var (
	// ErrRoleNotFound is returned for an unknown role id or name.
	ErrRoleNotFound = errors.New("authn: no such role")
	// ErrSystemRole is returned when an edit or delete targets a system role.
	ErrSystemRole = errors.New("authn: a system role cannot be changed")
	// ErrRoleInUse is returned when deleting a role that someone holds.
	ErrRoleInUse = errors.New("authn: the role is assigned to a user")
	// ErrInvalidRole is returned for a malformed name or description.
	ErrInvalidRole = errors.New("authn: invalid role")
)

// RoleRecord is a stored role with its permissions.
type RoleRecord struct {
	ID          string
	Name        string
	Description string
	// System marks one of the four roles the code's separation of duties is written
	// against. The API refuses to edit or delete it.
	System      bool
	CreatedAt   time.Time
	Permissions []Permission
}

// Role is the wire-facing name. The two exist so a caller cannot accidentally treat a
// stored role as a declared one: `Role("administrator")` is a constant, RoleRecord is a row.

// CreateRole adds a custom role with the given permissions.
//
// The permission list is validated against the declared set before anything is written: a
// role containing an unknown permission would be a role that looks wider than it is, and
// the operator would have no way to tell.
func (s *Service) CreateRole(ctx context.Context, name, description string, perms []Permission) (RoleRecord, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if err := validateRoleName(name); err != nil {
		return RoleRecord{}, err
	}
	description = strings.TrimSpace(description)
	if len(description) > 500 {
		return RoleRecord{}, fmt.Errorf("%w: the description is too long", ErrInvalidRole)
	}
	unique, err := dedupePermissions(perms)
	if err != nil {
		return RoleRecord{}, err
	}

	var out RoleRecord
	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO roles (name, description, is_system) VALUES ($1, $2, false)
			RETURNING id, name, description, is_system, created_at`,
			name, description).
			Scan(&out.ID, &out.Name, &out.Description, &out.System, &out.CreatedAt); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: a role named %q already exists", ErrInvalidRole, name)
			}
			return fmt.Errorf("authn: create role: %w", err)
		}
		for _, p := range unique {
			if _, err := tx.Exec(ctx,
				`INSERT INTO role_permissions (role_id, permission) VALUES ($1, $2)`,
				out.ID, string(p)); err != nil {
				return fmt.Errorf("authn: grant permission: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return RoleRecord{}, err
	}
	out.Permissions = unique
	return out, nil
}

// UpdateRolePermissions replaces a custom role's permission set.
//
// Replacement rather than add/remove: the screen is a matrix, the operator submits the
// matrix, and a diff computed server-side from two submits that raced would be wrong in a
// way nobody can see.
func (s *Service) UpdateRolePermissions(ctx context.Context, roleID string, perms []Permission) (RoleRecord, error) {
	unique, err := dedupePermissions(perms)
	if err != nil {
		return RoleRecord{}, err
	}

	var out RoleRecord
	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var system bool
		err := tx.QueryRow(ctx,
			`SELECT id, name, description, is_system, created_at FROM roles WHERE id = $1 FOR UPDATE`,
			roleID).
			Scan(&out.ID, &out.Name, &out.Description, &system, &out.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRoleNotFound
		}
		if err != nil {
			return fmt.Errorf("authn: load role: %w", err)
		}
		if system {
			return ErrSystemRole
		}
		out.System = false

		if _, err := tx.Exec(ctx, `DELETE FROM role_permissions WHERE role_id = $1`, roleID); err != nil {
			return fmt.Errorf("authn: clear permissions: %w", err)
		}
		for _, p := range unique {
			if _, err := tx.Exec(ctx,
				`INSERT INTO role_permissions (role_id, permission) VALUES ($1, $2)`,
				roleID, string(p)); err != nil {
				return fmt.Errorf("authn: grant permission: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return RoleRecord{}, err
	}
	out.Permissions = unique
	return out, nil
}

// DeleteRole removes a custom role that nobody holds.
//
// A role still assigned to a user is refused rather than cascaded: deleting it would
// silently strip that user's access, and "my permissions disappeared" is an incident, not
// a convenience.
func (s *Service) DeleteRole(ctx context.Context, roleID string) error {
	return s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var system bool
		err := tx.QueryRow(ctx, `SELECT is_system FROM roles WHERE id = $1 FOR UPDATE`, roleID).Scan(&system)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRoleNotFound
		}
		if err != nil {
			return fmt.Errorf("authn: load role: %w", err)
		}
		if system {
			return ErrSystemRole
		}

		var holders int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM staff_roles WHERE role_id = $1`, roleID).Scan(&holders); err != nil {
			return fmt.Errorf("authn: count role holders: %w", err)
		}
		if holders > 0 {
			return fmt.Errorf("%w (%d user(s) hold it)", ErrRoleInUse, holders)
		}

		if _, err := tx.Exec(ctx, `DELETE FROM roles WHERE id = $1`, roleID); err != nil {
			return fmt.Errorf("authn: delete role: %w", err)
		}
		return nil
	})
}

// ListRoles returns every role, system roles first, each with its permission set.
//
// System roles read their permissions from the code map, custom roles from the table. The
// two are merged here rather than in the caller so the screen and the authorizer cannot
// disagree about what a role means.
func (s *Service) ListRoles(ctx context.Context) ([]RoleRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, description, is_system, created_at
		FROM roles
		ORDER BY is_system DESC, name`)
	if err != nil {
		return nil, fmt.Errorf("authn: list roles: %w", err)
	}
	defer rows.Close()

	out := []RoleRecord{}
	byID := map[string]int{}
	for rows.Next() {
		var r RoleRecord
		if err := rows.Scan(&r.ID, &r.Name, &r.Description, &r.System, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("authn: scan role: %w", err)
		}
		r.Permissions = []Permission{}
		byID[r.ID] = len(out)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("authn: read roles: %w", err)
	}

	// A system role's permissions are the code map; the table copy exists for review, not
	// for enforcement, so a hand-edited row cannot widen what the code grants.
	for i := range out {
		if out[i].System {
			out[i].Permissions = declaredRolePermissions(out[i].Name)
		}
	}

	permRows, err := s.pool.Query(ctx, `
		SELECT role_id, permission FROM role_permissions ORDER BY permission`)
	if err != nil {
		return nil, fmt.Errorf("authn: list role permissions: %w", err)
	}
	defer permRows.Close()
	for permRows.Next() {
		var (
			roleID string
			perm   string
		)
		if err := permRows.Scan(&roleID, &perm); err != nil {
			return nil, fmt.Errorf("authn: scan role permission: %w", err)
		}
		idx, ok := byID[roleID]
		if !ok || out[idx].System {
			continue
		}
		out[idx].Permissions = append(out[idx].Permissions, Permission(perm))
	}
	if err := permRows.Err(); err != nil {
		return nil, fmt.Errorf("authn: read role permissions: %w", err)
	}
	for i := range out {
		sort.Slice(out[i].Permissions, func(a, b int) bool {
			return out[i].Permissions[a] < out[i].Permissions[b]
		})
	}
	return out, nil
}

// RoleByID loads one role with its permissions.
func (s *Service) RoleByID(ctx context.Context, roleID string) (RoleRecord, error) {
	roles, err := s.ListRoles(ctx)
	if err != nil {
		return RoleRecord{}, err
	}
	for _, r := range roles {
		if r.ID == roleID {
			return r, nil
		}
	}
	return RoleRecord{}, ErrRoleNotFound
}

// RoleByName loads one role by name.
func (s *Service) RoleByName(ctx context.Context, name string) (RoleRecord, error) {
	roles, err := s.ListRoles(ctx)
	if err != nil {
		return RoleRecord{}, err
	}
	for _, r := range roles {
		if r.Name == name {
			return r, nil
		}
	}
	return RoleRecord{}, ErrRoleNotFound
}

// AllRolePermissions returns every custom role's permission set, keyed by role name.
//
// It is the input to Authorizer.SetTableRoles, which is a *global* map: a role means the same
// thing for every user who holds it, so resolving it per user would be the same query
// repeated. System roles are omitted because the Authorizer ignores them by design — the
// compiled-in map wins, and a row must not be able to redefine "administrator".
//
// The caller refreshes this at startup and whenever a role changes; the returned map is a
// snapshot, not a live view.
func (s *Service) AllRolePermissions(ctx context.Context) (map[Role][]Permission, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.name, rp.permission
		FROM roles r
		LEFT JOIN role_permissions rp ON rp.role_id = r.id
		WHERE NOT r.is_system
		ORDER BY r.name, rp.permission`)
	if err != nil {
		return nil, fmt.Errorf("authn: load role permissions: %w", err)
	}
	defer rows.Close()

	out := map[Role][]Permission{}
	for rows.Next() {
		var (
			name string
			perm *string
		)
		if err := rows.Scan(&name, &perm); err != nil {
			return nil, fmt.Errorf("authn: scan role permission: %w", err)
		}
		if _, ok := out[Role(name)]; !ok {
			out[Role(name)] = []Permission{}
		}
		if perm != nil && *perm != "" {
			out[Role(name)] = append(out[Role(name)], Permission(*perm))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("authn: read role permissions: %w", err)
	}
	return out, nil
}

// RolePermissionsFor resolves a user's staff permissions from the roles table.
//
// This is the accessor the Authorizer uses when a role_id is set. It returns one entry per
// held role, so the caller can union the code map and the table without a second query per
// role. A user with no staff role returns an empty map, not an error.
func (s *Service) RolePermissionsFor(ctx context.Context, userID string) (map[Role][]Permission, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.name, COALESCE(rp.permission, '')
		FROM staff_roles sr
		JOIN roles r ON r.id = sr.role_id
		LEFT JOIN role_permissions rp ON rp.role_id = r.id
		WHERE sr.user_id = $1
		ORDER BY r.name, rp.permission`, userID)
	if err != nil {
		return nil, fmt.Errorf("authn: load role permissions: %w", err)
	}
	defer rows.Close()

	out := map[Role][]Permission{}
	for rows.Next() {
		var (
			name string
			perm string
		)
		if err := rows.Scan(&name, &perm); err != nil {
			return nil, fmt.Errorf("authn: scan role permission: %w", err)
		}
		if _, ok := out[Role(name)]; !ok {
			out[Role(name)] = []Permission{}
		}
		if perm != "" {
			out[Role(name)] = append(out[Role(name)], Permission(perm))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("authn: read role permissions: %w", err)
	}
	return out, nil
}

// PermissionsForUser resolves the effective permission set for a user id.
//
// It is the table-backed equivalent of Authorizer.Permissions, and it exists so a caller
// that has only an id (a CLI, a worker, an access-review export) does not have to build a
// User first. It unions the customer baseline, the code map for system roles, and the
// table for custom roles — the same three inputs the Authorizer uses.
func (s *Service) PermissionsForUser(ctx context.Context, userID string) (map[Permission]struct{}, error) {
	user, err := s.UserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	fromTable, err := s.AllRolePermissions(ctx)
	if err != nil {
		return nil, err
	}
	return UnionPermissions(user.Roles, fromTable), nil
}

// UnionPermissions computes the effective permission set.
//
// A system role contributes its code-map set; a custom role contributes its table rows. The
// customer baseline is always included, because a staff member is also a person with their
// own account. Exported so the Authorizer and the API describe permissions identically.
func UnionPermissions(roles []string, fromTable map[Role][]Permission) map[Permission]struct{} {
	out := make(map[Permission]struct{}, len(customerPermissions)+16)
	for _, p := range customerPermissions {
		out[p] = struct{}{}
	}
	for _, name := range roles {
		if declared, ok := rolePermissions[Role(name)]; ok {
			for _, p := range declared {
				out[p] = struct{}{}
			}
			continue
		}
		for _, p := range fromTable[Role(name)] {
			// A row naming an undeclared permission is ignored rather than granted: the
			// declared set is the contract, and a stale row must fail closed.
			if knownPermission(p) {
				out[p] = struct{}{}
			}
		}
	}
	return out
}

// declaredRolePermissions returns the code-map set for a system role, sorted.
func declaredRolePermissions(name string) []Permission {
	declared, ok := rolePermissions[Role(name)]
	if !ok {
		return []Permission{}
	}
	out := append([]Permission(nil), declared...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// dedupePermissions validates and de-duplicates a submitted permission set.
func dedupePermissions(perms []Permission) ([]Permission, error) {
	seen := make(map[Permission]struct{}, len(perms))
	out := make([]Permission, 0, len(perms))
	for _, p := range perms {
		if !knownPermission(p) {
			return nil, fmt.Errorf("%w: %q is not a permission this build declares", ErrInvalidRole, p)
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// validateRoleName enforces the same shape the migration's CHECK does, so the error is a
// 400 from the handler rather than a 500 from the constraint.
func validateRoleName(name string) error {
	if len(name) < 2 || len(name) > 40 {
		return fmt.Errorf("%w: a role name must be 2 to 40 characters", ErrInvalidRole)
	}
	for i, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-'
		if i == 0 {
			ok = r >= 'a' && r <= 'z'
		}
		if !ok {
			return fmt.Errorf("%w: a role name is lower-case letters, digits, '-' and '_'", ErrInvalidRole)
		}
	}
	// The four declared names are reserved even before the row is read, so a concurrent
	// create cannot produce a second "administrator".
	if IsStaffRole(name) {
		return fmt.Errorf("%w: %q is a reserved role name", ErrInvalidRole, name)
	}
	return nil
}

// isUniqueViolation reports whether err is a PostgreSQL unique-constraint failure.
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
