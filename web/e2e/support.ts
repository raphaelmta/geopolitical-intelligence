// Helpers of the end-to-end test, kept apart so the later measurement step
// can reuse the authenticated-API plumbing: a real Keycloak login through
// the browser and the access token taken from the app's own API requests,
// kept in memory only (never written to disk or printed).
import { expect, type APIRequestContext, type Page } from '@playwright/test';

export const APP = 'http://localhost:5173';
export const API = 'http://localhost:8080';
export const KEYCLOAK = 'http://localhost:8081';
export const REALM = 'geoint-e2e';

/** Test users of deploy/keycloak/realm-e2e.json (throwaway e2e stack only). */
export const USERS = {
  analista: { username: 'analista', password: 'e2e-only-analista', name: 'Analista E2E' },
  admin: { username: 'admin-e2e', password: 'e2e-only-admin-e2e', name: 'Admin E2E' },
} as const;
export type User = (typeof USERS)[keyof typeof USERS];

/** The login form of the e2e realm (authorization endpoint). */
export async function expectKeycloakLogin(page: Page): Promise<void> {
  await expect(page).toHaveURL(new RegExp(`^${KEYCLOAK}/realms/${REALM}/(protocol/openid-connect/auth|login-actions/)`));
  await expect(page.getByRole('textbox', { name: /username/i })).toBeVisible();
}

/**
 * Logs in through the real Keycloak form (authorization code + PKCE S256)
 * starting from path, and waits until the app shows that page signed in.
 */
export async function login(page: Page, user: User, path = '/'): Promise<void> {
  await page.goto(path);
  await expectKeycloakLogin(page);
  const auth = new URL(page.url());
  expect(auth.searchParams.get('client_id')).toBe('geoint-web');
  expect(auth.searchParams.get('response_type')).toBe('code');
  expect(auth.searchParams.get('code_challenge_method')).toBe('S256');
  expect(auth.searchParams.get('code_challenge')).toBeTruthy();
  expect(auth.searchParams.get('redirect_uri')).toBe(`${APP}/`);
  await page.getByRole('textbox', { name: /username/i }).fill(user.username);
  await page.getByLabel('Password', { exact: true }).fill(user.password);
  await page.getByRole('button', { name: /sign in/i }).click();
  await expect(page).toHaveURL(new RegExp(`^${APP}${path === '/' ? '/(\\?.*)?$' : path.replace(/[?]/g, '\\?')}`));
  await expect(page.getByRole('button', { name: 'Sair' })).toBeVisible();
  await expect(page.getByText(user.name)).toBeVisible();
}

/**
 * Remembers the bearer token of the app's latest API request. The token is
 * held in this closure only; tests pass it straight to API calls.
 */
export function tapToken(page: Page): () => string {
  let token = '';
  page.on('request', (req) => {
    if (!req.url().startsWith(`${API}/`)) return;
    const h = req.headers()['authorization'];
    if (h?.startsWith('Bearer ')) token = h.slice('Bearer '.length);
  });
  return () => {
    expect(token, 'the app has made an authenticated API request').not.toBe('');
    return token;
  };
}

/** GET an API path with a bearer token (Playwright's request context, no browser). */
export function apiGet(request: APIRequestContext, path: string, token: string) {
  return request.get(`${API}/api/v1${path}`, { headers: { Authorization: `Bearer ${token}` } });
}

/** GET an API path with a bearer token, expecting 200, and return its JSON. */
export async function apiJSON<T>(request: APIRequestContext, path: string, token: string): Promise<T> {
  const res = await apiGet(request, path, token);
  expect(res.status(), `GET ${path}`).toBe(200);
  return (await res.json()) as T;
}

/**
 * Collects, for the whole life of the page, every CSP violation (the
 * securitypolicyviolation event, from an init script, so it is listening
 * before any app code runs), console error and uncaught page error of the
 * app's own documents (APP origin). Keycloak's pages (KEYCLOAK origin) run
 * under Keycloak's own CSP and are not watched. The test fails at the end if
 * the list is not empty.
 */
export async function watchProblems(page: Page): Promise<string[]> {
  const problems: string[] = [];
  const ours = (url: string) => url.startsWith(`${APP}/`);
  await page.exposeBinding('__e2eReportViolation', (_source, v: Record<string, string>) => {
    if (ours(v.page ?? '')) {
      problems.push(`CSP violation on ${v.page}: ${v.directive} blocked ${v.blocked || '(inline)'} ${v.sample ?? ''}`);
    }
  });
  await page.addInitScript(() => {
    document.addEventListener('securitypolicyviolation', (e) => {
      const report = (window as unknown as { __e2eReportViolation: (v: Record<string, string>) => void })
        .__e2eReportViolation;
      report({
        page: location.origin + location.pathname,
        directive: e.effectiveDirective,
        blocked: e.blockedURI,
        sample: e.sample,
      });
    });
  });
  page.on('console', (msg) => {
    // A resource the app failed to load (e.g. an API call) is located at that
    // resource's URL; only Keycloak's own pages are left out.
    const at = msg.location().url || page.url();
    if (msg.type() === 'error' && !at.startsWith(`${KEYCLOAK}/`)) problems.push(`console error on ${at}: ${msg.text()}`);
  });
  page.on('pageerror', (err) => {
    if (ours(page.url())) problems.push(`page error on ${page.url()}: ${err.message}`);
  });
  return problems;
}

/** Every key and value of localStorage and sessionStorage of the current page. */
export async function webStorage(page: Page) {
  return page.evaluate(() => {
    const dump = (s: Storage) => {
      const out: Record<string, string> = {};
      for (let i = 0; i < s.length; i++) {
        const k = s.key(i);
        if (k !== null) out[k] = s.getItem(k) ?? '';
      }
      return out;
    };
    return { local: dump(window.localStorage), session: dump(window.sessionStorage) };
  });
}
