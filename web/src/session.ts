// The session as the API client and the gate see it. Renewal is driven here,
// always through the refresh-token grant (oidc-client-ts's automatic renewal
// and its iframe fallback are never used). The controller also remembers, in
// memory, whether the current session was created by a login that a 401
// started, so a second rejection shows a page instead of looping through
// Keycloak.
import { ErrorResponse, type User, type UserManager } from 'oidc-client-ts';
import type { ApiAuth } from './api/client';

/** What stops the app: expired session, identity provider down, or the API refusing a fresh session. */
export type SessionProblem = 'expired' | 'outage' | 'rejected' | null;

/** Seconds before expiry at which the access token is renewed. */
export const RENEW_BEFORE_SECONDS = 60;

/** State kept through the login redirect (sessionStorage, no tokens). */
export interface LoginState {
  returnTo: string;
  /** The login was started because the API answered 401. */
  reauth?: boolean;
}

const CONTROL_OR_BACKSLASH = /[\u0000-\u001f\u007f\\]/;

/** A same-origin route (path + search + hash) or "/". */
export function safeRoute(v: unknown): string {
  if (typeof v !== 'string' || !v.startsWith('/') || CONTROL_OR_BACKSLASH.test(v)) return '/';
  try {
    const u = new URL(v, window.location.origin);
    if (u.origin !== window.location.origin) return '/';
    return u.pathname + u.search + u.hash;
  } catch {
    return '/';
  }
}

export function currentRoute(): string {
  return window.location.pathname + window.location.search + window.location.hash;
}

export class SessionController implements ApiAuth {
  readonly userManager: UserManager;
  private problem: SessionProblem = null;
  private listeners = new Set<() => void>();
  private timer: ReturnType<typeof setTimeout> | undefined;
  private renewing: Promise<string | null> | null = null;
  /** The current session came from a login started by a 401 and no API call has succeeded since. */
  private reauth = false;

  constructor(userManager: UserManager) {
    this.userManager = userManager;
    userManager.events.addUserLoaded((u) => this.schedule(u));
    userManager.events.addUserUnloaded(() => this.stop());
  }

  // --- observable problem state (useSyncExternalStore) ---

  subscribe = (fn: () => void): (() => void) => {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  };

  getProblem = (): SessionProblem => this.problem;

  private setProblem(p: SessionProblem): void {
    if (this.problem === p) return;
    this.problem = p;
    this.listeners.forEach((fn) => fn());
  }

  /** Clears a problem so the app shows again (its queries reload; no login). */
  clearProblem(): void {
    this.setProblem(null);
  }

  // --- login return ---

  /** After the code exchange: drop code/state from the URL, go back, remember a 401-started login. */
  afterSignin = (user: User | undefined | void): void => {
    const state = (user?.state ?? null) as Partial<LoginState> | null;
    this.reauth = state?.reauth === true;
    window.history.replaceState(null, '', safeRoute(state?.returnTo));
  };

  // --- renewal ---

  /** Renews shortly before the access token expires. */
  schedule(user: User): void {
    this.stop();
    const left = user.expires_in;
    if (left === undefined) return;
    const ms = Math.max(0, (left - RENEW_BEFORE_SECONDS) * 1000);
    this.timer = setTimeout(() => void this.renew(), ms);
  }

  stop(): void {
    if (this.timer !== undefined) clearTimeout(this.timer);
    this.timer = undefined;
  }

  // --- ApiAuth ---

  accessToken = async (): Promise<string | null> => (await this.userManager.getUser())?.access_token ?? null;

  /**
   * Refresh-token grant only, one at a time. No refresh token, or the
   * provider refusing it (invalid_grant…), means the session expired; a
   * network or server failure means the provider is unavailable.
   */
  renew = (): Promise<string | null> => {
    this.renewing ??= this.doRenew().finally(() => {
      this.renewing = null;
    });
    return this.renewing;
  };

  private async doRenew(): Promise<string | null> {
    const user = await this.userManager.getUser();
    if (!user?.refresh_token) {
      this.setProblem('expired');
      return null;
    }
    try {
      const fresh = await this.userManager.signinSilent();
      return fresh?.access_token ?? null;
    } catch (err) {
      this.setProblem(err instanceof ErrorResponse ? 'expired' : 'outage');
      return null;
    }
  }

  /**
   * The API refused a renewed token. Start one login marked as reauth; if the
   * current session already came from such a login, stop and show the page.
   */
  login = async (returnTo: string): Promise<boolean> => {
    if (this.reauth) {
      this.setProblem('rejected');
      return false;
    }
    const state: LoginState = { returnTo: safeRoute(returnTo), reauth: true };
    await this.userManager.signinRedirect({ state });
    return true;
  };

  confirmed = (): void => {
    this.reauth = false;
  };
}
