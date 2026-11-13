import { describe, expect, test, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter } from 'react-router';
import { AppRoutes } from './App';
import { SessionContext, type Session } from './auth';
import { configureApi } from './api/client';
import { API, fakeAuth, newQueryClient } from './test/render';
import { json, mockFetch } from './test/fetchMock';
import { companyEntity } from './test/fixtures';

vi.mock('./components/graphAdapter', () => import('./test/graphMock'));

function renderApp(route: string, session: Partial<Session> = {}) {
  configureApi({ baseUrl: API, auth: fakeAuth() });
  const s: Session = { name: 'Ana Analista', roles: ['analyst', 'admin'], signOut: vi.fn(), ...session };
  render(
    <QueryClientProvider client={newQueryClient()}>
      <SessionContext.Provider value={s}>
        <MemoryRouter initialEntries={[route]}>
          <AppRoutes />
        </MemoryRouter>
      </SessionContext.Provider>
    </QueryClientProvider>,
  );
  return s;
}

describe('App', () => {
  test('cabeçalho mostra a Bancada, a busca e o usuário com papéis', () => {
    mockFetch({});
    renderApp('/');
    const banner = screen.getByRole('banner');
    expect(banner.textContent).toContain('Bancada');
    expect(screen.getByRole('navigation', { name: 'Principal' })).toBeTruthy();
    expect(screen.getByRole('link', { name: 'Busca' }).getAttribute('href')).toBe('/');
    expect(screen.getByRole('link', { name: 'Caminhos' }).getAttribute('href')).toBe('/caminhos');
    expect(banner.textContent).toContain('Ana Analista');
    expect(banner.textContent).toContain('analista');
    expect(banner.textContent).toContain('administrador');
    expect(screen.getByRole('main')).toBeTruthy();
    expect(document.body.textContent?.toLowerCase()).not.toContain('geoint');
  });

  test('Sair chama o logout', async () => {
    mockFetch({});
    const s = renderApp('/');
    await userEvent.click(screen.getByRole('button', { name: 'Sair' }));
    expect(s.signOut).toHaveBeenCalledTimes(1);
  });

  test('rota desconhecida mostra página não encontrada', () => {
    mockFetch({});
    renderApp('/nada/aqui');
    expect(screen.getByRole('heading', { name: 'Página não encontrada' })).toBeTruthy();
  });

  test('ao mudar de rota, o foco vai para o título da página e o título do documento muda', async () => {
    mockFetch({
      '/api/v1/search': json({ items: [{ id: 12, kind: 'company', name: 'EMPRESA FICTICIA A LTDA', natural_key: '90000002' }] }),
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships': json({ items: [], next_cursor: '' }),
    });
    renderApp('/?q=ficticia');
    expect(document.title).toBe('Busca — Bancada');
    await userEvent.click(await screen.findByRole('link', { name: 'EMPRESA FICTICIA A LTDA' }));
    const h1 = await screen.findByRole('heading', { level: 1, name: 'EMPRESA FICTICIA A LTDA' });
    await vi.waitFor(() => expect(document.activeElement).toBe(h1));
    expect(document.title).toBe('EMPRESA FICTICIA A LTDA — Bancada');
  });

  test('rotas do mapa, dos caminhos e do grupo de controle existem', async () => {
    mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships': json({ items: [], next_cursor: '' }),
      '/api/v1/jobs': json({ id: '0193b6c2-7f00-7000-8000-000000000001' }, 202),
      '/api/v1/jobs/0193b6c2-7f00-7000-8000-000000000001': json({
        id: '0193b6c2-7f00-7000-8000-000000000001', type: 'control_group', params: {}, status: 'queued',
        progress: 0, result: null, error: '', created_at: '2026-11-11T12:00:00Z', finished_at: null,
      }),
    });
    renderApp('/caminhos');
    expect(await screen.findByRole('heading', { level: 1, name: 'Caminhos entre duas entidades' })).toBeTruthy();
    cleanup();
    renderApp('/mapa/12');
    expect(await screen.findByRole('heading', { level: 1, name: 'Mapa: EMPRESA FICTICIA A LTDA' })).toBeTruthy();
    cleanup();
    renderApp('/grupo/12');
    expect(await screen.findByRole('heading', { level: 1, name: 'Grupo de controle: EMPRESA FICTICIA A LTDA' })).toBeTruthy();
  });

  test('menu leva à fila de identidades e à situação das bases', async () => {
    mockFetch({
      '/api/v1/identity/proposals': json({ items: [], next_cursor: '' }),
      '/api/v1/sources': json([]),
    });
    renderApp('/');
    expect(screen.getByRole('link', { name: 'Identidades' }).getAttribute('href')).toBe('/identidades');
    expect(screen.getByRole('link', { name: 'Bases' }).getAttribute('href')).toBe('/bases');
    await userEvent.click(screen.getByRole('link', { name: 'Identidades' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Fila de identidades' })).toBeTruthy();
    await userEvent.click(screen.getByRole('link', { name: 'Bases' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Situação das bases' })).toBeTruthy();
  });

  test('página não encontrada tem título próprio', () => {
    mockFetch({});
    renderApp('/xyz');
    expect(document.title).toBe('Página não encontrada — Bancada');
    expect(document.activeElement).toBe(screen.getByRole('heading', { level: 1 }));
  });
});
