import { describe, expect, test, vi } from 'vitest';
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { SearchPage } from './SearchPage';
import { renderAt } from '../test/render';
import { json, mockFetch } from '../test/fetchMock';

async function search(text: string) {
  await userEvent.type(screen.getByRole('searchbox', { name: /Nome ou CNPJ/ }), text);
  await userEvent.click(screen.getByRole('button', { name: 'Buscar' }));
}

describe('SearchPage', () => {
  test('busca com menos de 3 caracteres não chama a API e mostra aviso', async () => {
    const { fn } = mockFetch({});
    renderAt(<SearchPage />);
    await search('a .b');
    expect(screen.getByText('Digite pelo menos 3 caracteres')).toBeTruthy();
    expect(fn).not.toHaveBeenCalled();
  });

  test('busca lista resultados com tipo e CNPJ formatado', async () => {
    const { calls } = mockFetch({
      '/api/v1/search': json({
        items: [
          { id: 12, kind: 'company', name: 'EMPRESA FICTICIA A LTDA', natural_key: '12345678' },
          { id: 11, kind: 'person', name: 'PESSOA ***456789** FICTICIA', natural_key: '' },
        ],
      }),
    });
    renderAt(<SearchPage />);
    await search('ficticia');
    const table = await screen.findByRole('table', { name: /Resultados/ });
    const rows = within(table).getAllByRole('row').slice(1);
    expect(rows).toHaveLength(2);
    expect(within(rows[0]!).getByText('Empresa')).toBeTruthy();
    expect(within(rows[0]!).getByText('12.345.678')).toBeTruthy();
    expect(within(rows[0]!).getByRole('link', { name: 'EMPRESA FICTICIA A LTDA' }).getAttribute('href')).toBe(
      '/entidade/12',
    );
    expect(within(rows[1]!).getByText('Pessoa')).toBeTruthy();
    expect(within(rows[1]!).getByText('PESSOA ***456789** FICTICIA')).toBeTruthy();
    expect(calls[0]!.url.searchParams.get('q')).toBe('ficticia');
    expect(calls[0]!.url.searchParams.has('kind')).toBe(false);
  });

  test('filtro de tipo vai para a API', async () => {
    const { calls } = mockFetch({ '/api/v1/search': json({ items: [] }) });
    renderAt(<SearchPage />);
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Tipo' }), 'company');
    await search('ficticia');
    expect(await screen.findByText(/Nenhum resultado/)).toBeTruthy();
    expect(calls[0]!.url.searchParams.get('kind')).toBe('company');
  });

  test('um nome com <script> aparece como texto', async () => {
    mockFetch({
      '/api/v1/search': json({
        items: [{ id: 5, kind: 'company', name: '<script>alert(1)</script><img src=x onerror=alert(2)>', natural_key: '' }],
      }),
    });
    const { container } = renderAt(<SearchPage />);
    await search('script');
    expect(await screen.findByText('<script>alert(1)</script><img src=x onerror=alert(2)>')).toBeTruthy();
    expect(container.querySelector('script')).toBeNull();
    expect(container.querySelector('img')).toBeNull();
  });

  test('query_too_short da API vira mensagem em português', async () => {
    mockFetch({ '/api/v1/search': json({ error: 'query_too_short' }, 400) });
    renderAt(<SearchPage />);
    await search('ltda');
    expect(await screen.findByRole('alert')).toBeTruthy();
    expect(screen.getByRole('alert').textContent).toMatch(/3 caracteres/);
  });

  test('403 mostra a mensagem de permissão, sem tentar de novo', async () => {
    mockFetch({ '/api/v1/search': json({ error: 'forbidden' }, 403) });
    renderAt(<SearchPage />);
    await search('ficticia');
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Você não tem permissão para esta ação');
    expect(within(alert).queryByRole('button')).toBeNull();
  });

  test('503 auth_unavailable mostra a mensagem do serviço de autenticação', async () => {
    mockFetch({ '/api/v1/search': json({ error: 'auth_unavailable' }, 503) });
    renderAt(<SearchPage />);
    await search('ficticia');
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Serviço de autenticação indisponível — tente novamente em instantes');
  });

  test('código desconhecido mostra mensagem genérica e nunca o texto do servidor', async () => {
    mockFetch({ '/api/v1/search': json({ error: 'pq: relation "segredo" does not exist' }, 500) });
    const { container } = renderAt(<SearchPage />);
    await search('ficticia');
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Algo deu errado. Tente novamente.');
    expect(container.textContent).not.toContain('segredo');
  });

  test('falha de rede oferece tentar de novo', async () => {
    let fail = true;
    const fetchFn = vi.fn(async () => {
      if (fail) throw new TypeError('Failed to fetch');
      return json({ items: [{ id: 12, kind: 'company', name: 'EMPRESA X', natural_key: '12345678' }] });
    });
    vi.stubGlobal('fetch', fetchFn);
    renderAt(<SearchPage />);
    await search('empresa');
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toMatch(/Não foi possível falar com o servidor/);
    fail = false;
    await userEvent.click(within(alert).getByRole('button', { name: 'Tentar novamente' }));
    expect(await screen.findByRole('link', { name: 'EMPRESA X' })).toBeTruthy();
    expect(fetchFn).toHaveBeenCalledTimes(2);
  });

  test('explica como a busca funciona', () => {
    mockFetch({});
    renderAt(<SearchPage />);
    const summary = screen.getByText('Como a busca funciona');
    const details = summary.closest('details')!;
    expect(details.textContent).toMatch(/palavras inteiras/);
    expect(details.textContent).toMatch(/erros de digitação/);
    expect(details.textContent).toMatch(/CNPJ/);
  });
});
