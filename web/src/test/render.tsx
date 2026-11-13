import type { ReactElement } from 'react';
import { render } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter, Route, Routes } from 'react-router';
import { vi } from 'vitest';
import { configureApi, type ApiAuth } from '../api/client';
import { SessionContext, type Session } from '../auth';

export const API = 'http://api.test';

export function fakeAuth(overrides: Partial<ApiAuth> = {}): ApiAuth {
  return {
    accessToken: vi.fn(async () => 'token-1'),
    renew: vi.fn(async () => null),
    login: vi.fn(async () => true),
    ...overrides,
  };
}

export function newQueryClient() {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false, gcTime: Infinity } },
  });
}

/**
 * Renders ui at route (matching path), with a query client, a configured API
 * and a signed-in session (an analyst unless roles say otherwise).
 */
export function renderAt(
  ui: ReactElement,
  { route = '/', path = '/', auth = fakeAuth(), roles = ['analyst'] }: { route?: string; path?: string; auth?: ApiAuth; roles?: string[] } = {},
) {
  configureApi({ baseUrl: API, auth });
  const client = newQueryClient();
  const session: Session = { name: 'Ana Analista', roles, signOut: vi.fn() };
  const result = render(
    <QueryClientProvider client={client}>
      <SessionContext.Provider value={session}>
        <MemoryRouter initialEntries={[route]}>
          <Routes>
            <Route path={path} element={ui} />
            <Route path="*" element={<p>outra rota</p>} />
          </Routes>
        </MemoryRouter>
      </SessionContext.Provider>
    </QueryClientProvider>,
  );
  return { ...result, auth, client };
}
