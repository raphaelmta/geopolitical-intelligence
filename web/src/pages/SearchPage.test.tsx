import { afterEach, describe, expect, test, vi } from 'vitest';
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Link, Route, Routes, useLocation, useNavigate } from 'react-router';
import { SearchPage, forgetCPFSearch } from './SearchPage';
import { renderAt } from '../test/render';
import { json, mockFetch } from '../test/fetchMock';

async function search(text: string) {
  const box = screen.getByRole('searchbox', { name: /Nome, CNPJ ou CPF/ });
  await userEvent.clear(box);
  await userEvent.type(box, text);
  await userEvent.click(screen.getByRole('button', { name: 'Buscar' }));
}

/** Shows the router's current location, to check what reaches the URL. */
function Where() {
  const l = useLocation();
  return <output data-testid="where">{l.pathname + l.search + l.hash}</output>;
}

const where = () => screen.getByTestId('where').textContent ?? '';

afterEach(() => forgetCPFSearch());

// Synthetic CPFs only: 123.456.789-09 has valid check digits.
const CPF_HITS = {
  cpf_fragment_search: true,
  items: [
    { id: 31, kind: 'person', name: 'MARIA FICTICIA', natural_key: '', match: 'cpf_fragment' },
    { id: 32, kind: 'person', name: 'MARIA OUTRA FICTICIA', natural_key: '', match: 'cpf_fragment' },
  ],
};

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
    // the kind has an icon beside its name, hidden from screen readers (the word is there)
    expect(rows[0]!.querySelector('.kind-icon--company')!.getAttribute('aria-hidden')).toBe('true');
    expect(rows[1]!.querySelector('.kind-icon--person')).not.toBeNull();
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

  test('busca por CPF: aviso de candidatos, marca em cada resultado e nada do CPF na URL', async () => {
    const { calls } = mockFetch({ '/api/v1/search': json(CPF_HITS) });
    renderAt(
      <>
        <SearchPage />
        <Where />
      </>,
    );
    expect(screen.getByRole('searchbox', { name: 'Nome, CNPJ ou CPF' }).getAttribute('placeholder')).toBe(
      'nome, CNPJ ou CPF',
    );
    await search('maria 123.456.789-09');
    const table = await screen.findByRole('table', { name: /Pessoas candidatas/ });
    const note = screen.getByRole('note', { name: 'Sobre a busca por CPF' });
    expect(note.textContent).toMatch(/6 dígitos do meio/);
    expect(note.textContent).toMatch(/não é a confirmação/);
    expect(note.textContent).toMatch(/acrescente o nome/);
    // the note comes before the results and has no close button
    expect(note.compareDocumentPosition(table) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(within(note).queryByRole('button')).toBeNull();
    const rows = within(table).getAllByRole('row').slice(1);
    expect(rows).toHaveLength(2);
    for (const r of rows) expect(within(r).getByText('coincide com o trecho do CPF')).toBeTruthy();
    // the API got the query; the URL, the history and storage did not
    expect(calls[0]!.url.searchParams.get('q')).toBe('maria 123.456.789-09');
    expect(where()).toBe('/');
    expect(window.history.state?.usr ?? null).toBeNull();
    expect(JSON.stringify({ ...window.localStorage })).not.toMatch(/456/);
    expect(JSON.stringify({ ...window.sessionStorage })).not.toMatch(/456/);
    // the text stays in the box
    expect((screen.getByRole('searchbox') as HTMLInputElement).value).toBe('maria 123.456.789-09');
  });

  test('busca por CPF depois de uma por nome tira o nome da URL; outra por nome volta à URL', async () => {
    mockFetch({
      '/api/v1/search?q=maria': json({ items: [{ id: 31, kind: 'person', name: 'MARIA FICTICIA', natural_key: '' }] }),
      '/api/v1/search?q=***456789**': json(CPF_HITS),
      '/api/v1/search?q=joana': json({ items: [] }),
    });
    renderAt(
      <>
        <SearchPage />
        <Where />
      </>,
      { route: '/?q=maria' },
    );
    expect(await screen.findByRole('table', { name: /Resultados para “maria”/ })).toBeTruthy();
    await search('***456789**');
    expect(await screen.findByRole('table', { name: /Pessoas candidatas/ })).toBeTruthy();
    expect(where()).toBe('/');
    await search('joana');
    expect(await screen.findByText(/Nenhum resultado para “joana”/)).toBeTruthy();
    expect(where()).toBe('/?q=joana');
    expect(screen.queryByRole('note', { name: 'Sobre a busca por CPF' })).toBeNull();
  });

  test('CPF que chega na URL vai para a memória e sai da URL', async () => {
    const { calls } = mockFetch({ '/api/v1/search': json(CPF_HITS) });
    renderAt(
      <>
        <SearchPage />
        <Where />
      </>,
      { route: '/?q=12345678909&tipo=person' },
    );
    expect(await screen.findByRole('table', { name: /Pessoas candidatas/ })).toBeTruthy();
    expect(where()).toBe('/?tipo=person');
    expect(calls[0]!.url.searchParams.get('q')).toBe('12345678909');
    expect(calls[0]!.url.searchParams.get('kind')).toBe('person');
  });

  test('voltar para a busca traz de volta a busca por CPF, só da memória', async () => {
    const { calls } = mockFetch({ '/api/v1/search': json(CPF_HITS) });
    function Back() {
      const navigate = useNavigate();
      return (
        <button type="button" onClick={() => void navigate(-1)}>
          voltar
        </button>
      );
    }
    renderAt(
      <Routes>
        <Route
          path="/"
          element={
            <>
              <SearchPage />
              <Link to="/entidade/31">ir</Link>
              <Where />
            </>
          }
        />
        <Route path="/entidade/:id" element={<Back />} />
      </Routes>,
      { path: '*' },
    );
    await search('123.456.789-09');
    expect(await screen.findByRole('table', { name: /Pessoas candidatas/ })).toBeTruthy();
    await userEvent.click(screen.getByRole('link', { name: 'ir' }));
    await userEvent.click(await screen.findByRole('button', { name: 'voltar' }));
    expect(await screen.findByRole('table', { name: /Pessoas candidatas/ })).toBeTruthy();
    expect((screen.getByRole('searchbox') as HTMLInputElement).value).toBe('123.456.789-09');
    expect(where()).toBe('/');
    expect(calls.every((c) => c.url.pathname === '/api/v1/search')).toBe(true);
  });

  test('CPF inválido e dois CPFs viram mensagens em português', async () => {
    mockFetch({
      '/api/v1/search?q=123.456.789-01': json({ error: 'invalid_cpf' }, 400),
      '/api/v1/search?q=123.456.789-09 111.222.333-96': json({ error: 'multiple_cpfs' }, 400),
    });
    renderAt(<SearchPage />);
    await search('123.456.789-01');
    expect((await screen.findByRole('alert')).textContent).toContain('CPF inválido: confira os dígitos.');
    await search('123.456.789-09 111.222.333-96');
    expect((await screen.findByRole('alert')).textContent).toContain('mais de um CPF');
  });

  test('busca por CPF sem resultado e com tipo empresa explica que só acha pessoas', async () => {
    mockFetch({ '/api/v1/search': json({ cpf_fragment_search: true, items: [] }) });
    renderAt(<SearchPage />);
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Tipo' }), 'company');
    await search('123.456.789-09');
    expect(await screen.findByText(/A busca por CPF só encontra pessoas/)).toBeTruthy();
    expect(screen.getByRole('note', { name: 'Sobre a busca por CPF' })).toBeTruthy();
  });

  test('a ajuda explica a busca por CPF', () => {
    mockFetch({});
    renderAt(<SearchPage />);
    const details = screen.getByText('Como a busca funciona').closest('details')!;
    expect(details.textContent).toMatch(/6 dígitos do meio/);
    expect(details.textContent).toMatch(/candidatas, não uma confirmação/);
    expect(details.textContent).toMatch(/não vai para o endereço da página/);
  });

  test('trecho de CPF genérico demais: pede o nome, sem lista', async () => {
    mockFetch({ '/api/v1/search': json({ cpf_fragment_search: true, cpf_fragment_too_common: true, items: [] }) });
    renderAt(<SearchPage />);
    await search('***000001**');
    expect(await screen.findByText(/genérico demais nas fontes/)).toBeTruthy();
    expect(screen.getByText(/Acrescente o nome para buscar/)).toBeTruthy();
    expect(screen.queryByRole('table')).toBeNull();
  });

  test('CPF com palavras: pessoas candidatas marcadas e empresas pelas palavras, sem marca', async () => {
    mockFetch({
      '/api/v1/search': json({
        cpf_fragment_search: true,
        items: [
          { id: 31, kind: 'person', name: 'JOAO FICTICIO', natural_key: '', match: 'cpf_fragment' },
          { id: 40, kind: 'company', name: 'JOAO FICTICIO ***456789**', natural_key: '12345678' },
        ],
      }),
    });
    renderAt(<SearchPage />);
    await search('JOAO FICTICIO ***456789**');
    const table = await screen.findByRole('table', { name: /Resultados para/ });
    const rows = within(table).getAllByRole('row').slice(1);
    expect(within(rows[0]!).getByText('coincide com o trecho do CPF')).toBeTruthy();
    expect(within(rows[1]!).queryByText('coincide com o trecho do CPF')).toBeNull();
    expect(screen.getByRole('note', { name: 'Sobre a busca por CPF' }).textContent).toMatch(/empresas da lista foram achadas só pelas palavras/);
  });
});
