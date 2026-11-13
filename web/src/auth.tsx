// Login through Keycloak (authorization code + PKCE S256, public client).
// Tokens are kept in memory only; sessionStorage holds just the short-lived
// state/PKCE entry of a redirect in progress. A reload therefore needs a new
// (silent, via the Keycloak SSO session) redirect. Renewal: SessionController.
import { createContext, useContext, useEffect, useRef, useSyncExternalStore, type ReactNode } from 'react';
import { hasAuthParams, useAuth } from 'react-oidc-context';
import { InMemoryWebStorage, UserManager, WebStorageStateStore } from 'oidc-client-ts';
import { useQueryClient } from '@tanstack/react-query';
import type { AppConfig } from './config';
import { currentRoute, type LoginState, type SessionController } from './session';

export function createUserManager(config: AppConfig): UserManager {
  const origin = window.location.origin;
  return new UserManager({
    authority: config.oidcAuthority,
    client_id: config.oidcClientId,
    redirect_uri: `${origin}/`,
    post_logout_redirect_uri: `${origin}/`,
    response_type: 'code',
    response_mode: 'query',
    scope: 'openid profile',
    // Tokens: memory only.
    userStore: new WebStorageStateStore({ store: new InMemoryWebStorage() }),
    // Redirect state + PKCE verifier only (no tokens), removed on return.
    stateStore: new WebStorageStateStore({ store: window.sessionStorage, prefix: 'oidc.' }),
    // Renewal is driven by SessionController through the refresh-token grant:
    // no automatic renewal, no iframe, no session-monitor iframe.
    automaticSilentRenew: false,
    monitorSession: false,
    loadUserInfo: false,
    revokeTokensOnSignout: true,
    staleStateAgeInSeconds: 600,
  });
}

/** Realm roles from the access token's "roles" claim (display only; the API enforces). */
export function rolesOf(accessToken: string | undefined): string[] {
  const payload = accessToken?.split('.')[1];
  if (!payload) return [];
  try {
    const b64 = payload.replace(/-/g, '+').replace(/_/g, '/');
    const bytes = Uint8Array.from(atob(b64.padEnd(b64.length + ((4 - (b64.length % 4)) % 4), '=')), (ch) =>
      ch.charCodeAt(0),
    );
    const claims: unknown = JSON.parse(new TextDecoder().decode(bytes));
    const roles = (claims as { roles?: unknown }).roles;
    return Array.isArray(roles) ? roles.filter((r): r is string => typeof r === 'string') : [];
  } catch {
    return [];
  }
}

/** State data of the logout redirect (no token; kept in sessionStorage until the return). */
export const SIGNOUT_STATE = { signedOut: true } as const;

/** Keycloak's return after logout: "/?state=…" without code or error. */
export function isSignoutReturn(loc: Location = window.location): boolean {
  const p = new URLSearchParams(loc.search);
  return loc.pathname === '/' && p.has('state') && !p.has('code') && !p.has('error');
}

export interface Session {
  name: string;
  roles: string[];
  signOut: () => void;
}

export const SessionContext = createContext<Session | null>(null);

export function useSession(): Session {
  const s = useContext(SessionContext);
  if (!s) throw new Error('useSession outside AuthGate');
  return s;
}

export const ROLE_LABELS: Record<string, string> = { analyst: 'analista', admin: 'administrador' };

export const MSG_OUTAGE = 'Serviço de autenticação indisponível — tente novamente em instantes';
export const MSG_REJECTED =
  'A API recusou a sessão recém-criada. Verifique o relógio do computador ou avise o administrador.';

/**
 * Renders children only for a signed-in analyst. Without a session it starts
 * the login once (keeping the current route); a user without the analyst
 * role gets the "sem acesso" page; an expired session, an unavailable
 * identity provider or a session the API refuses get their own pages.
 */
export function AuthGate({
  children,
  controller,
  signedOut = false,
}: {
  children: ReactNode;
  controller: SessionController;
  signedOut?: boolean;
}) {
  const auth = useAuth();
  const queryClient = useQueryClient();
  const started = useRef(false);
  const problem = useSyncExternalStore(controller.subscribe, controller.getProblem);

  const canAutoLogin =
    !signedOut && !auth.isLoading && !auth.isAuthenticated && !auth.activeNavigator && !auth.error && !hasAuthParams();

  useEffect(() => {
    if (canAutoLogin && !started.current) {
      started.current = true;
      void auth.signinRedirect({ state: { returnTo: currentRoute() } satisfies LoginState }).catch(() => undefined);
    }
  }, [canAutoLogin, auth]);

  // A failed return from the login page: do not leave code/state in the URL.
  useEffect(() => {
    if (auth.error && hasAuthParams()) window.history.replaceState(null, '', '/');
  }, [auth.error]);

  // A session restored at start (or after login) gets its renewal timer.
  useEffect(() => {
    if (auth.user) controller.schedule(auth.user);
  }, [auth.user, controller]);

  const login = (returnTo = '/') =>
    void auth.signinRedirect({ state: { returnTo } satisfies LoginState }).catch(() => undefined);
  const signOut = () => {
    controller.stop();
    queryClient.clear();
    // State data is required: without it oidc-client-ts sends no `state` on
    // the logout URL, Keycloak returns to "/" without one, isSignoutReturn
    // misses the return and the app starts a new login instead of "Você saiu".
    void auth.signoutRedirect({ state: SIGNOUT_STATE }).catch(() => void auth.removeUser());
  };

  if (problem === 'expired' || auth.error?.source === 'renewSilent') {
    return (
      <Standalone title="Sua sessão expirou">
        <p>Entre novamente para continuar de onde parou.</p>
        <button type="button" className="button button--primary" onClick={() => login(currentRoute())}>
          Entrar novamente
        </button>
      </Standalone>
    );
  }

  if (problem === 'outage') {
    return (
      <Standalone title="Não foi possível renovar a sessão">
        <p>{MSG_OUTAGE}</p>
        <button
          type="button"
          className="button"
          onClick={() => {
            // Renew first; only then mark what failed while the provider was
            // down as stale and show the app again (it refetches those).
            void controller.renew().then((token) => {
              if (!token) return;
              void queryClient.invalidateQueries({ predicate: (q) => q.state.status === 'error' });
              controller.clearProblem();
            });
          }}
        >
          Tentar novamente
        </button>
      </Standalone>
    );
  }

  if (problem === 'rejected') {
    return (
      <Standalone title="Sessão recusada pela API">
        <p>{MSG_REJECTED}</p>
        <div className="actions">
          <button
            type="button"
            className="button"
            onClick={() => {
              void queryClient.invalidateQueries();
              controller.clearProblem();
            }}
          >
            Tentar novamente
          </button>
          <button type="button" className="button" onClick={signOut}>
            Sair
          </button>
        </div>
      </Standalone>
    );
  }

  if (auth.error) {
    return (
      <Standalone title="Não foi possível entrar">
        <p>
          {auth.error.source === 'signinCallback'
            ? 'O retorno do login não pôde ser validado (ele pode ter expirado). Entre de novo.'
            : MSG_OUTAGE}
        </p>
        <button type="button" className="button" onClick={() => login()}>
          Entrar novamente
        </button>
      </Standalone>
    );
  }

  if (!auth.isAuthenticated || !auth.user) {
    if (signedOut && !auth.isLoading) {
      return (
        <Standalone title="Você saiu">
          <p>Sua sessão foi encerrada.</p>
          <button type="button" className="button" onClick={() => login()}>
            Entrar
          </button>
        </Standalone>
      );
    }
    return (
      <Standalone title="Bancada">
        <p role="status">Entrando…</p>
      </Standalone>
    );
  }

  const profile = auth.user.profile;
  const roles = rolesOf(auth.user.access_token);
  const session: Session = {
    name: profile.name || profile.preferred_username || 'usuário',
    roles,
    signOut,
  };

  if (!roles.includes('analyst')) {
    return (
      <Standalone title="Sem acesso">
        <p>
          A conta <strong>{session.name}</strong> entrou, mas não tem o papel de analista, necessário para consultar a
          Bancada. Peça a um administrador que atribua o papel <code>analyst</code> à sua conta.
        </p>
        <button type="button" className="button" onClick={signOut}>
          Sair
        </button>
      </Standalone>
    );
  }

  return <SessionContext.Provider value={session}>{children}</SessionContext.Provider>;
}

function Standalone({ title, children }: { title: string; children: ReactNode }) {
  useEffect(() => {
    document.title = `${title} — Bancada`;
  }, [title]);
  return (
    <main className="standalone">
      <h1>{title}</h1>
      {children}
    </main>
  );
}
