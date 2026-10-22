// The contract between the shell and a page.
//
// A page is a function of its context. It gets the API client, the session, the current
// route and the shell's services (toasts, dialogs, navigation), and returns a node. It does
// not import the shell, which is what keeps a page testable and stops one screen's
// convenience from becoming another's dependency.

import type { Api, LiveEvent } from "./api";

export interface SessionUser {
  id: string;
  email: string;
  display_name: string;
  email_verified: boolean;
  /** Always an array in the client: loadSession normalises the server's null. */
  roles: string[];
  is_staff: boolean;
}

/**
 * The caller's own second-factor state, from `GET /api/v1/auth/me`.
 *
 * The field names are the ones `GET /api/v1/portal/mfa` uses, because both are rendered from the
 * same `authn.MFAStatusFor` read and two carriers of one fact must not disagree about what to call
 * it. `available` is false only when the state could not be read — a failed read is not a claim
 * that the factor is off, so no screen may offer to enrol on top of it.
 */
export interface MfaState {
  has_mfa: boolean;
  enrolled_at: string | null;
  recovery_codes_remaining: number;
  enrolment_required: boolean;
  reset_at: string | null;
  reset_by: string | null;
  available: boolean;
}

export interface Session {
  user: SessionUser;
  permissions: Set<string>;
  csrfToken: string;
  expiresAt: string;
  /** True when the account owes the panel a second factor: an operator reset it, or it is
   *  otherwise required. The shell sends the customer to the enrolment screen while it is set,
   *  and `ctx.refreshSession()` clears it once a new factor is confirmed. */
  mfaEnrolmentRequired: boolean;
  /** The caller's own second factor, so the shell and the account screens can say on/off as a
   *  fact rather than guessing. Refreshed by `ctx.refreshSession()` after an enrolment. */
  mfa: MfaState;
}

export interface RouteContext {
  path: string;
  params: Record<string, string>;
  query: URLSearchParams;
}

export interface Crumb {
  label: string;
  href?: string;
}

export interface ToastOptions {
  detail?: string;
  tone?: "success" | "danger" | "info";
  /** Milliseconds; 0 keeps it until dismissed. */
  duration?: number;
}

export interface ConfirmOptions {
  title: string;
  body?: string | Node;
  confirmLabel?: string;
  cancelLabel?: string;
  danger?: boolean;
  /** Requires the user to type this exact string before confirming. */
  phrase?: string;
  note?: string;
}

export interface FormDialogOptions {
  title: string;
  subtitle?: string;
  body: Node;
  submitLabel?: string;
  cancelLabel?: string;
  danger?: boolean;
  /** Returns an error message to keep the dialog open, or nothing to close it. */
  onSubmit: () => Promise<string | void>;
}

export interface Ctx {
  readonly api: Api;
  readonly session: Session;
  readonly route: RouteContext;

  can(permission: string): boolean;
  navigate(path: string, options?: { replace?: boolean }): void;
  /** Updates the query string without remounting the page. */
  replaceQuery(patch: Record<string, string | null>): URLSearchParams;
  toast(title: string, options?: ToastOptions): void;
  confirm(options: ConfirmOptions): Promise<boolean>;
  formDialog(options: FormDialogOptions): Promise<void>;
  setTitle(title: string): void;
  setCrumbs(crumbs: Crumb[]): void;
  refreshSession(): Promise<void>;
  /** Registers cleanup (an abort controller, a timer) run when the page is replaced. */
  onDispose(fn: () => void): void;
  /** Subscribes to committed state changes from the live feed; cleaned up on navigation. */
  onLive(fn: (event: LiveEvent) => void): void;
}

export interface AuthCtx {
  readonly api: Api;
  readonly query: URLSearchParams;
  /** Path parameters, so a public page like /products/:slug can read its slug. */
  readonly params: Record<string, string>;
  navigate(path: string, options?: { replace?: boolean }): void;
  onSignedIn(session: Session): void;
  toast(title: string, options?: ToastOptions): void;
}

export type PageFn = (ctx: Ctx) => Node | Promise<Node>;

export interface PageMeta {
  title: string;
  crumb?: string;
}

export interface PageModule {
  default: PageFn;
  meta?: PageMeta;
}

export type Loader = () => Promise<PageModule>;
