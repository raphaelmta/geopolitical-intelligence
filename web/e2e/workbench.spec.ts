// End-to-end test of the whole platform (`make e2e`): the real Keycloak
// login, the API, the worker and the web app over the fixtures of
// testdata/, loaded by scripts/e2e-seed.sh through the source simulator.
//
// Fixture facts used below (testdata/*/README.md):
//   - ALFA PARTICIPAÇÕES S.A. (CNPJ 98990001, Receita) has partners JOSÉ
//     CONCEIÇÃO BRANDÃO and NORTHWIND HOLDINGS LIMITED, is a partner of ALFA
//     INVESTIMENTOS LTDA (98990002), which is a partner of ALFA COMÉRCIO DE
//     ALIMENTOS LTDA (98990003); JOSÉ is also a partner of 98990003 and of
//     BETA SERVIÇOS DE ENGENHARIA LTDA (98990005).
//   - TSE 2022, receipt 50018: JOSÉ CONCEIÇÃO BRANDÃO (CPF fragment 123456,
//     the fragment of the Receita partner of the same name) donated to the
//     governor candidate CARLOS EDUARDO EXEMPLO, the politician of the paths
//     test. Identity resolution joins the donor and the partner (same name,
//     one fragment), so CARLOS ← JOSÉ → ALFA COMÉRCIO is a 2-hop path.
//   - Identity queue: ROBERTO DOADOR FICTÍCIO and LÚCIA DOADORA EXEMPLO are
//     TSE donors (fragments 222333 and 666777, several records each) with
//     Receita namesakes of other fragments (partners of 98990026). The e2e
//     worker merges automatically only names with 1 fragment
//     (IDENTITY_COMMON_NAME_THRESHOLD=1), so each becomes a pending proposal.
//   - CVM 2026: ALFA HOLDING S.A. holds 51.37 % of ALFA PARTICIPAÇÕES and
//     controls it; ALFA CONTROLE LTDA holds 70 % of the holding (controller)
//     and FERNANDA FICTÍCIA NETA 100 % of ALFA CONTROLE (controller); ANA
//     FICTÍCIA CONTROLADORA is a declared co-controller (10 %).
import { createHash } from 'node:crypto';
import { expect, test, type Locator, type Page } from '@playwright/test';
import {
  API,
  APP,
  USERS,
  apiGet,
  apiJSON,
  expectKeycloakLogin,
  login,
  tapToken,
  watchProblems,
  webStorage,
} from './support';

const ALFA = 'ALFA PARTICIPAÇÕES S.A.';
const ALFA_COMERCIO = 'ALFA COMÉRCIO DE ALIMENTOS LTDA';
const JOSE = 'JOSÉ CONCEIÇÃO BRANDÃO';
const CARLOS = 'CARLOS EDUARDO EXEMPLO';
const ROBERTO = 'ROBERTO DOADOR FICTÍCIO';
const LUCIA = 'LÚCIA DOADORA EXEMPLO';
const SOURCE_NAMES = [
  'Receita Federal — CNPJ',
  'TSE — candidaturas e contas',
  'Câmara dos Deputados',
  'Senado Federal',
  'CVM — companhias abertas',
];
/** The simulator, reachable from the playwright container only (compose network). */
const SIMULATOR = 'http://sourcesim:8080';

/** dd/mm/aaaa, as the app formats dates. */
const DATE = /\b\d{2}\/\d{2}\/\d{4}\b/;

/** The provenance dialog opened by a "fonte" button. */
function provenanceDialog(page: Page): Locator {
  return page.getByRole('dialog', { name: /^Fonte do vínculo nº \d+$/ });
}

/** Rows of a table, header row excluded. */
function bodyRows(table: Locator): Locator {
  return table.getByRole('row').filter({ has: table.page().getByRole('cell') });
}

test.describe('sem sessão', () => {
  test('sem login, qualquer rota redireciona para o Keycloak', async ({ page }) => {
    // The app's documents load (config, then the login redirect) before
    // Keycloak's page: nothing may be reported on them either.
    const problems = await watchProblems(page);
    for (const path of ['/', '/entidade/1', '/bases']) {
      await page.goto(path);
      await expectKeycloakLogin(page);
      // The production client and flow: authorization code + PKCE S256,
      // back to the app's origin.
      const auth = new URL(page.url());
      expect(auth.searchParams.get('code_challenge_method')).toBe('S256');
      expect(auth.searchParams.get('redirect_uri')).toBe(`${APP}/`);
    }
    expect(problems).toEqual([]);
  });

  // The "zero CSP violations" checks below are only as good as the watcher:
  // an inline style on a page of the app must be reported.
  test('o vigia de CSP enxerga uma violação', async ({ page }) => {
    const problems = await watchProblems(page);
    await page.goto('/config.json');
    await page.evaluate(() => {
      const s = document.createElement('style');
      s.textContent = 'body { color: red; }';
      document.head.appendChild(s);
    });
    await expect.poll(() => problems.some((p) => p.startsWith(`CSP violation on ${APP}/config.json: style-src`))).toBe(true);
  });

  // Plain HTTP checks (no browser document is loaded here, so there is
  // nothing for the CSP watcher to observe).
  test('servidor web e API: cabeçalhos de segurança, 401 sem token, CORS só da aplicação', async ({ request }) => {
    const index = await request.get(`${APP}/entidade/1`);
    expect(index.status()).toBe(200);
    const h = index.headers();
    const csp = h['content-security-policy'] ?? '';
    for (const directive of [
      "default-src 'self'",
      "script-src 'self'",
      "style-src 'self'",
      `connect-src 'self' ${API} http://localhost:8081`,
      "frame-ancestors 'none'",
      "object-src 'none'",
      "base-uri 'self'",
      "form-action 'self'",
    ]) {
      expect(csp, `CSP has ${directive}`).toContain(directive);
    }
    expect(csp).not.toContain('unsafe-inline');
    expect(csp).not.toContain('unsafe-eval');
    expect(h['x-content-type-options']).toBe('nosniff');
    expect(h['referrer-policy']).toBe('no-referrer');
    expect(h['x-frame-options']).toBe('DENY');
    expect(h['cross-origin-opener-policy']).toBe('same-origin');
    expect(h['cross-origin-resource-policy']).toBe('same-origin');
    expect(h['permissions-policy']).toContain('camera=()');

    const config = await request.get(`${APP}/config.json`);
    expect(await config.json()).toEqual({
      apiBaseUrl: API,
      oidcAuthority: 'http://localhost:8081/realms/geoint-e2e',
      oidcClientId: 'geoint-web',
    });

    const anonymous = await request.get(`${API}/api/v1/sources`);
    expect(anonymous.status()).toBe(401);
    expect(anonymous.headers()['content-type']).toContain('application/json');
    expect(await anonymous.json()).toEqual({ error: 'unauthorized' });

    const preflight = (origin: string) =>
      request.fetch(`${API}/api/v1/sources`, {
        method: 'OPTIONS',
        headers: { Origin: origin, 'Access-Control-Request-Method': 'GET', 'Access-Control-Request-Headers': 'authorization' },
      });
    const ours = await preflight(APP);
    expect(ours.status()).toBe(204);
    expect(ours.headers()['access-control-allow-origin']).toBe(APP);
    expect(ours.headers()['access-control-allow-headers']?.toLowerCase()).toContain('authorization');
    const theirs = await preflight('http://evil.example');
    expect(theirs.headers()['access-control-allow-origin']).toBeUndefined();
    const simple = await request.get(`${API}/api/v1/sources`, { headers: { Origin: 'http://localhost:5174' } });
    expect(simple.headers()['access-control-allow-origin']).toBeUndefined();
  });
});

test('analista busca empresa, navega, acha caminho e vê a fonte', async ({ page, request }) => {
  test.setTimeout(180_000);
  const problems = await watchProblems(page);
  const token = tapToken(page);

  // Login through the real Keycloak form, then the session is in memory only.
  await login(page, USERS.analista);
  await expect(page.getByRole('heading', { level: 1, name: 'Busca' })).toBeVisible();

  // Search by CNPJ.
  await page.getByLabel('Nome, CNPJ ou CPF').fill('98990001');
  await page.keyboard.press('Enter');
  const results = page.getByRole('table', { name: /Resultados para “98990001”/ });
  await expect(results).toBeVisible();
  // The app has made authenticated API calls by now (the search).
  const accessToken = token();
  const stored = await webStorage(page);
  for (const [area, items] of Object.entries(stored)) {
    for (const [k, v] of Object.entries(items)) {
      expect(v.includes(accessToken), `${area}Storage[${k}] holds the access token`).toBe(false);
      expect(/access_token|refresh_token|id_token/.test(v), `${area}Storage[${k}] holds a token`).toBe(false);
    }
  }
  expect(stored.local).toEqual({});
  // The redirect state (PKCE verifier) is removed on the way back.
  expect(stored.session).toEqual({});
  expect(await page.evaluate(() => document.cookie)).not.toContain(accessToken);

  await results.getByRole('link', { name: ALFA }).click();
  await expect(page.getByRole('heading', { level: 1, name: ALFA })).toBeVisible();
  await expect(page.getByText('CNPJ básico')).toBeVisible();

  // The entity page draws its connections right away: the lazy Cytoscape
  // chunk, under the CSP, with the map's legend and a link to the full map.
  const connections = page.getByRole('region', { name: 'Mapa de conexões' });
  await expect(connections).toBeVisible();
  await expect(
    connections.getByRole('application', { name: new RegExp(`^Mapa de conexões de ${ALFA.replace('.', '\\.')}`) }).locator('canvas').first(),
  ).toBeVisible();
  await expect(connections.getByRole('list', { name: 'Linhas do gráfico' })).toContainText('tracejada: inferência');
  await expect(connections.getByRole('link', { name: 'Abrir mapa completo' })).toHaveAttribute('href', /^\/mapa\/\d+$/);

  // Partners, each a "fato".
  const current = page.getByRole('table', { name: /^Vínculos atuais: \d+ carregados$/ });
  await expect(current).toBeVisible();
  const partners = bodyRows(current).filter({ hasText: 'tem como sócio' });
  await expect(partners.filter({ has: page.getByRole('link', { name: JOSE }) })).toHaveCount(1);
  await expect(partners.filter({ has: page.getByRole('link', { name: 'NORTHWIND HOLDINGS LIMITED' }) })).toHaveCount(1);
  for (const row of await partners.all()) {
    await expect(row.getByText('fato', { exact: true })).toBeVisible();
  }
  // JOSÉ is a person merged automatically from several records (see below):
  // his link shows the identity inference, never as certain; the company
  // partner's link has no identity tag.
  const IDENTITY_AUTO = /^identidade (\d{1,2}|< 1)%$/;
  await expect(partners.filter({ has: page.getByRole('link', { name: JOSE }) }).getByText(IDENTITY_AUTO)).toBeVisible();
  await expect(
    partners.filter({ has: page.getByRole('link', { name: 'NORTHWIND HOLDINGS LIMITED' }) }).getByText(/^identidade/),
  ).toHaveCount(0);

  // Where the partnership with JOSÉ came from.
  const joseRow = partners.filter({ has: page.getByRole('link', { name: JOSE }) });
  await joseRow.getByRole('button', { name: `fonte do vínculo com ${JOSE}` }).click();
  const source = provenanceDialog(page);
  await expect(source).toBeVisible();
  await expect(source.getByText('Receita Federal — CNPJ')).toBeVisible();
  await expect(source.getByText(/^Socios\.csv:\d+$/)).toBeVisible();
  // What the dialog must show, from the API it shows (not from this test's
  // clock: the edition month is the one the seed collected).
  const relId = Number((await source.getByRole('heading').first().innerText()).replace(/\D/g, ''));
  const prov = await apiJSON<{
    collection: { reference_date: string };
    raw_key: string;
    raw_sha256: string;
    source_url: string | null;
  }>(request, `/relationships/${relId}/provenance`, accessToken);
  const ref = prov.collection.reference_date; // YYYY-MM-01: the Receita edition
  expect(ref).toMatch(/^\d{4}-\d{2}-01$/);
  const [y, m] = ref.split('-');
  // The two collections: the first that recorded the link and the last that
  // confirmed it (the seed collected once, so both name the same edition).
  const firstSeen = source.getByRole('group', { name: 'Primeira coleta que registrou este vínculo' });
  const lastSeen = source.getByRole('group', { name: 'Última coleta que o confirmou' });
  for (const block of [firstSeen, lastSeen]) {
    await expect(block.getByRole('definition').filter({ hasText: `01/${m}/${y}` }).first()).toBeVisible();
    await expect(block).toContainText(/Coletada em\s*\d{2}\/\d{2}\/\d{4} \d{2}:\d{2}/);
  }
  await expect(source.getByText(IDENTITY_AUTO)).toBeVisible();
  // The link to the file at the source: the simulator's WebDAV URL, without
  // the share token, opened in a new tab without opener or referrer.
  const sourceUrl = `${SIMULATOR}/receita/${y}-${m}/Socios0.zip`;
  expect(prov.source_url).toBe(sourceUrl);
  const open = source.getByRole('link', { name: 'Abrir na fonte' });
  await expect(open).toHaveAttribute('href', sourceUrl);
  await expect(open).toHaveAttribute('target', '_blank');
  await expect(open).toHaveAttribute('rel', 'noopener noreferrer');
  await expect(source.getByText(sourceUrl, { exact: true })).toBeVisible();
  const rawKey = `raw/receita/${ref}/Socios0.zip`;
  expect(prov.raw_key).toBe(rawKey);
  await expect(source.getByText(rawKey, { exact: true })).toBeVisible();
  const shaCode = source.getByTitle(/^[0-9a-f]{64}$/);
  await expect(shaCode).toBeVisible();
  expect(await shaCode.getAttribute('title')).toBe(prov.raw_sha256);
  await expect(source.getByRole('button', { name: 'Copiar SHA-256 completo' })).toBeVisible();
  // The SHA-256 shown is the one of the file the source published.
  const published = await request.get(`${SIMULATOR}/receita/${y}-${m}/Socios0.zip`, {
    headers: { Authorization: `Basic ${btoa('e2e-share-token:')}` },
  });
  expect(published.status()).toBe(200);
  expect(await shaCode.getAttribute('title')).toBe(createHash('sha256').update(await published.body()).digest('hex'));
  await source.getByRole('button', { name: 'Fechar' }).click();
  await expect(source).toBeHidden();

  // The counterparts listed on the entity page...
  const listed = (await current.getByRole('row').count()) - 1;
  const others = (
    await Promise.all((await bodyRows(current).all()).map((r) => r.getByRole('cell').nth(1).getByRole('link').innerText()))
  ).sort();

  // ...are the links listed beside the graph on the map.
  await page.getByRole('link', { name: 'Ver no mapa' }).click();
  await expect(page.getByRole('heading', { level: 1, name: `Mapa: ${ALFA}` })).toBeVisible();
  const graph = page.getByRole('application', { name: new RegExp(`^Mapa de vínculos de ${ALFA.replace('.', '\\.')}`) });
  await expect(graph).toBeVisible();
  // The lazy Cytoscape chunk loaded and drew under the CSP.
  await expect(graph.locator('canvas').first()).toBeVisible();
  const mapEdges = page.getByRole('table', { name: /^Vínculos no mapa \(\d+\)$/ });
  await expect(mapEdges).toHaveAccessibleName(`Vínculos no mapa (${listed})`);
  const mapOthers = (
    await Promise.all(
      (await bodyRows(mapEdges).all()).map(async (r) => {
        const from = await r.getByRole('cell').nth(0).getByRole('link').innerText();
        const to = await r.getByRole('cell').nth(2).getByRole('link').innerText();
        return from === ALFA ? to : from;
      }),
    )
  ).sort();
  expect(mapOthers).toEqual(others);

  // Expand JOSÉ: his other companies join the map.
  const nodes = page.getByRole('table', { name: /^Entidades no mapa/ });
  await nodes.getByRole('button', { name: `Expandir ${JOSE}` }).click();
  await expect(bodyRows(nodes).filter({ hasText: JOSE })).toContainText('vínculos carregados');
  await expect(mapEdges.getByRole('link', { name: 'BETA SERVIÇOS DE ENGENHARIA LTDA' }).first()).toBeVisible();
  await expect(mapEdges.getByRole('link', { name: ALFA_COMERCIO }).first()).toBeVisible();

  // JOSÉ is one person in two sources: the identity resolution joined his
  // Receita partner records and his TSE donor record (same name, same CPF
  // fragment, the only fragment of that name).
  await nodes.getByRole('link', { name: JOSE, exact: true }).click();
  await expect(page.getByRole('heading', { level: 1, name: JOSE })).toBeVisible();
  const records = page.getByRole('table', { name: /^\d+ registros?/ });
  await expect(
    bodyRows(records).filter({ has: page.getByRole('cell', { name: 'Receita Federal — CNPJ', exact: true }) }).first(),
  ).toBeVisible();
  const tseRecord = bodyRows(records).filter({
    has: page.getByRole('cell', { name: 'TSE — candidaturas e contas', exact: true }),
  });
  await expect(tseRecord).toHaveCount(1);
  await expect(tseRecord).toContainText('donor:2022:10001600003');
  await expect(bodyRows(records).filter({ hasText: '***123456**' })).toHaveCount(await bodyRows(records).count());

  // Paths between the politician (the candidate JOSÉ donated to) and a
  // company JOSÉ is a partner of: politician ← person donation ← person →
  // partner → company.
  const donation = bodyRows(page.getByRole('table', { name: /^Vínculos atuais/ })).filter({ hasText: 'doou para' });
  await donation.getByRole('link', { name: CARLOS }).click();
  await expect(page.getByRole('heading', { level: 1, name: CARLOS })).toBeVisible();
  await page.getByRole('link', { name: 'Caminhos a partir daqui' }).click();
  await expect(page.getByRole('heading', { level: 1, name: 'Caminhos entre duas entidades' })).toBeVisible();
  await expect(page.getByRole('group', { name: 'De', exact: true })).toContainText(CARLOS);
  await page.getByRole('combobox', { name: 'Até' }).fill('98990003');
  await page.getByRole('listbox', { name: 'Resultados para Até' }).getByRole('option', { name: new RegExp(ALFA_COMERCIO) }).click();
  await expect(page.getByRole('group', { name: 'Até', exact: true })).toContainText(ALFA_COMERCIO);
  await page.getByRole('button', { name: 'Buscar caminhos' }).click();
  await expect(page.getByRole('status').filter({ hasText: /^Tarefa: / })).toHaveText('Tarefa: concluída', { timeout: 60_000 });
  const firstPath = page.getByRole('region', { name: `Caminho 1: ${CARLOS} → ${JOSE} → ${ALFA_COMERCIO}` });
  await expect(firstPath).toBeVisible();
  await expect(page.getByText(/^1 caminho de 2 elos \(os mais curtos\)\./)).toBeVisible();
  // Both links rest on JOSÉ, a person merged automatically: facts, but the
  // path says it depends on a merged identity.
  await expect(firstPath.getByText('2 elos — 2 fatos, 0 inferências, 2 com identidade unida')).toBeVisible();
  await expect(firstPath.getByText('contém inferência ou identidade unida', { exact: true })).toBeVisible();
  const hops = firstPath.getByRole('listitem');
  await expect(hops).toHaveCount(2);
  const expected = [
    { relation: 'recebeu doação de', base: 'TSE — candidaturas e contas' },
    { relation: 'é sócio de', base: 'Receita Federal — CNPJ' },
  ];
  for (const [i, want] of expected.entries()) {
    const hop = hops.nth(i);
    await expect(hop.getByText(want.relation, { exact: true })).toBeVisible();
    await expect(hop.getByText('fato', { exact: true })).toBeVisible();
    await expect(hop.getByText(IDENTITY_AUTO)).toBeVisible();
    await hop.getByRole('button', { name: /^fonte do vínculo entre / }).click();
    await expect(provenanceDialog(page).getByText(want.base, { exact: true })).toBeVisible();
    await provenanceDialog(page).getByRole('button', { name: 'Fechar' }).click();
    await expect(provenanceDialog(page)).toBeHidden();
  }
  await expect(page.getByRole('application', { name: 'Gráfico dos caminhos encontrados' }).locator('canvas').first()).toBeVisible();

  // Control group of ALFA COMÉRCIO following every corporate link: JOSÉ,
  // its direct partner and partner of the chain's top, is a final controller.
  await page.goto('/');
  await page.getByLabel('Nome, CNPJ ou CPF').fill('98990003');
  await page.keyboard.press('Enter');
  await page.getByRole('table', { name: /Resultados para/ }).getByRole('link', { name: ALFA_COMERCIO }).click();
  await page.getByRole('link', { name: 'Grupo de controle' }).click();
  await expect(page.getByRole('heading', { level: 1, name: `Grupo de controle: ${ALFA_COMERCIO}` })).toBeVisible();
  // Documented control first (the default): two partners, no declared
  // control or percentages in the Receita data, so no controller.
  await expect(page.getByRole('status').filter({ hasText: /^Tarefa: / })).toHaveText('Tarefa: concluída', { timeout: 60_000 });
  await expect(page.getByText('Nenhum controlador final identificado.')).toBeVisible();
  await page.getByRole('radio', { name: 'Vínculos societários' }).check();
  await expect(page.getByText('Vínculo societário não é prova de controle.')).toBeVisible();
  await expect(page.getByRole('status').filter({ hasText: /^Tarefa: / })).toHaveText('Tarefa: concluída', { timeout: 60_000 });
  const controllers = page.getByRole('region', { name: 'Controladores finais' });
  await expect(controllers.getByRole('link', { name: JOSE })).toBeVisible();

  // Documented control of the CVM company ALFA PARTICIPAÇÕES: the chain of
  // declared controllers ends at FERNANDA; ANA is a declared co-controller.
  await page.goto('/');
  await page.getByLabel('Nome, CNPJ ou CPF').fill('98990001');
  await page.keyboard.press('Enter');
  await page.getByRole('table', { name: /Resultados para/ }).getByRole('link', { name: ALFA }).click();
  await page.getByRole('link', { name: 'Grupo de controle' }).click();
  await expect(page.getByRole('radio', { name: 'Controle documentado' })).toBeChecked();
  await expect(page.getByRole('status').filter({ hasText: /^Tarefa: / })).toHaveText('Tarefa: concluída', { timeout: 60_000 });
  await expect(controllers.getByRole('link', { name: 'FERNANDA FICTÍCIA NETA' })).toBeVisible();
  await expect(controllers.getByRole('link', { name: 'ANA FICTÍCIA CONTROLADORA' })).toBeVisible();
  const groupEdges = page.getByRole('table', { name: /^Vínculos do grupo/ });
  const holdingControls = bodyRows(groupEdges).filter({ hasText: 'ALFA HOLDING S.A.' }).filter({ hasText: 'controla' });
  await expect(holdingControls.filter({ has: page.getByRole('link', { name: ALFA }) })).toHaveCount(1);

  // Every source up to date, each with its reference date.
  await page.getByRole('link', { name: 'Bases' }).click();
  await expect(page.getByRole('heading', { level: 1, name: 'Situação das bases' })).toBeVisible();
  await expect(
    page.getByText(
      'A primeira coleta de cada base é sempre pedida por alguém (aqui ou pela linha de comando). Depois disso a plataforma mantém a base em dia sozinha.',
      { exact: true },
    ),
  ).toBeVisible();
  await expect(page.getByRole('region', { name: SOURCE_NAMES[0] })).toContainText(/edição publicada em \d{2}\/\d{2}\/\d{4}/);
  for (const name of SOURCE_NAMES) {
    const card = page.getByRole('region', { name });
    await expect(card.getByText('em dia', { exact: true })).toBeVisible();
    await expect(card.getByRole('definition').first()).toContainText(DATE);
  }

  // Search by CPF: JOSÉ's records carry the fragment 123456. The CPF typed
  // (synthetic, valid check digits, middle digits 123456) lists candidates,
  // with the caveat, and never reaches the URL, the history or storage.
  const CPF = '987.123.456-28';
  await page.goto('/');
  await page.getByLabel('Nome, CNPJ ou CPF').fill(`jose ${CPF}`);
  await page.keyboard.press('Enter');
  const candidates = page.getByRole('table', { name: /^Pessoas candidatas/ });
  await expect(candidates).toBeVisible();
  await expect(page.getByRole('note', { name: 'Sobre a busca por CPF' })).toContainText('6 dígitos do meio');
  const joseHit = bodyRows(candidates).filter({ has: page.getByRole('link', { name: JOSE }) });
  await expect(joseHit).toHaveCount(1);
  await expect(joseHit.getByText('coincide com o trecho do CPF')).toBeVisible();
  const here = new URL(page.url());
  expect(here.pathname + here.search + here.hash).toBe('/');
  const cpfDigits = /987|123\.?456/;
  expect(JSON.stringify(await page.evaluate(() => window.history.state))).not.toMatch(cpfDigits);
  expect(JSON.stringify(await webStorage(page))).not.toMatch(cpfDigits);
  // The masked form alone lists every person of that fragment, JOSÉ included.
  await page.getByLabel('Nome, CNPJ ou CPF').fill('***123456**');
  await page.keyboard.press('Enter');
  await expect(bodyRows(candidates).filter({ has: page.getByRole('link', { name: JOSE }) })).toHaveCount(1);
  // Back from his page brings the CPF search back, from memory.
  await candidates.getByRole('link', { name: JOSE }).click();
  await expect(page.getByRole('heading', { level: 1, name: JOSE })).toBeVisible();
  await page.goBack();
  await expect(candidates).toBeVisible();
  expect(new URL(page.url()).search).toBe('');

  expect(problems, 'CSP violations, console or page errors during the flow').toEqual([]);

  // A CPF with wrong check digits is refused in plain Portuguese (the 400
  // shows up as a console error, hence after the check above).
  await page.getByLabel('Nome, CNPJ ou CPF').fill('987.123.456-29');
  await page.keyboard.press('Enter');
  await expect(page.getByRole('alert')).toContainText('CPF inválido: confira os dígitos.');
});

test('papéis: analista não vê "Coletar agora" e recebe 403; administrador vê o botão; Sair encerra a sessão', async ({
  browser,
  request,
}) => {
  // analista
  const analystContext = await browser.newContext();
  const analyst = await analystContext.newPage();
  const analystProblems = await watchProblems(analyst);
  const analystToken = tapToken(analyst);
  await login(analyst, USERS.analista, '/bases');
  await expect(analyst.getByRole('heading', { level: 1, name: 'Situação das bases' })).toBeVisible();
  await expect(analyst.getByRole('region', { name: SOURCE_NAMES[0] })).toBeVisible();
  await expect(analyst.getByRole('button', { name: 'Coletar agora' })).toHaveCount(0);
  const denied = await request.post(`${API}/api/v1/sources/receita/collect`, {
    headers: { Authorization: `Bearer ${analystToken()}` },
    data: {},
  });
  expect(denied.status()).toBe(403);
  expect(await denied.json()).toEqual({ error: 'forbidden' });
  // The same token reads fine: the 403 is the role, not the token.
  expect((await apiGet(request, '/sources', analystToken())).status()).toBe(200);
  expect(analystProblems).toEqual([]);
  await analystContext.close();

  // admin-e2e (admin includes analyst)
  const adminContext = await browser.newContext();
  const admin = await adminContext.newPage();
  const adminProblems = await watchProblems(admin);
  await login(admin, USERS.admin, '/bases');
  // Both roles (admin includes analyst); the order follows the token's claim.
  await expect(admin.getByText(/^(analista · administrador|administrador · analista)$/)).toBeVisible();
  await expect(admin.getByRole('button', { name: 'Coletar agora' })).toHaveCount(SOURCE_NAMES.length);
  // Open the request dialog (the <dialog> under the CSP) and cancel: no
  // collection is requested.
  await admin.getByRole('region', { name: SOURCE_NAMES[0] }).getByRole('button', { name: 'Coletar agora' }).click();
  const dialog = admin.getByRole('dialog', { name: `Coletar ${SOURCE_NAMES[0]} agora` });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole('button', { name: 'Pedir a coleta completa' })).toBeEnabled();
  await dialog.getByRole('button', { name: 'Cancelar' }).click();
  await expect(dialog).toBeHidden();

  // Sair: back to an unauthenticated state.
  await admin.getByRole('button', { name: 'Sair' }).click();
  await expect(admin.getByRole('heading', { level: 1, name: 'Você saiu' })).toBeVisible();
  await admin.goto('/');
  await expectKeycloakLogin(admin);
  expect(adminProblems).toEqual([]);
  await adminContext.close();
});

test('fila de identidades: proposta pendente, confirmar (cancelado) e rejeitar', async ({ page }) => {
  const problems = await watchProblems(page);
  await login(page, USERS.analista, '/identidades');
  await expect(page.getByRole('heading', { level: 1, name: 'Fila de identidades' })).toBeVisible();

  // ROBERTO: the proposal as listed, then the confirm dialog, cancelled.
  const roberto = page.getByRole('article', { name: ROBERTO, exact: true });
  await expect(roberto).toBeVisible();
  // From the proposal's reason_code (common_name), never from the English text.
  await expect(roberto.getByText(/Nome comum: há mais trechos do CPF diferentes com este nome/)).toBeVisible();
  const robertoRecords = roberto.getByRole('table', { name: /^Fichas propostas \(\d+\)$/ });
  const tseRows = bodyRows(robertoRecords).filter({ hasText: 'TSE — candidaturas e contas' });
  // Base names come from /sources: wait for them before counting.
  await expect(tseRows.nth(1)).toBeVisible();
  for (const row of await tseRows.all()) {
    await expect(row).toContainText(/doação \d{4}/);
    await expect(row).toContainText('***222333**');
    await expect(row).toContainText(ROBERTO);
  }
  // The namesake with another fragment (Receita) is not part of it.
  await expect(robertoRecords).not.toContainText('***555666**');
  const target = roberto.getByRole('region', { name: 'Entidade de destino' });
  await expect(target.getByText(/^Confirmar une as fichas a/)).toBeVisible();
  await expect(target.getByRole('link', { name: new RegExp(`^${ROBERTO} \\(nº \\d+\\)$`) })).toBeVisible();

  await roberto.getByRole('button', { name: 'Confirmar — são a mesma pessoa' }).click();
  const confirm = page.getByRole('dialog', { name: 'Confirmar que são a mesma pessoa' });
  await expect(confirm).toBeVisible();
  await expect(confirm.getByText(/Dados carregados às \d{2}:\d{2}:\d{2}\./)).toBeVisible();
  const toMerge = confirm.getByRole('table', { name: /^Fichas que serão unidas \(\d+\)$/ });
  await expect(bodyRows(toMerge)).toHaveCount(await tseRows.count());
  await expect(confirm.getByText(/serão unidas na entidade de destino/)).toBeVisible();
  await expect(confirm.getByRole('button', { name: 'Confirmar: são a mesma pessoa' })).toBeEnabled();
  await confirm.getByRole('button', { name: 'Cancelar' }).click();
  await expect(confirm).toBeHidden();
  await expect(roberto).toBeVisible();

  // LÚCIA: one real decision. Rejected, it leaves the pending list and shows
  // under "Rejeitadas".
  const lucia = page.getByRole('article', { name: LUCIA, exact: true });
  await lucia.getByRole('button', { name: 'Rejeitar — são pessoas diferentes' }).click();
  const reject = page.getByRole('dialog', { name: 'Rejeitar: são pessoas diferentes' });
  await expect(reject.getByText(/Dados carregados às \d{2}:\d{2}:\d{2}\./)).toBeVisible();
  await expect(reject.getByText(/nunca mais será unida automaticamente/)).toBeVisible();
  await reject.getByRole('button', { name: 'Rejeitar: são pessoas diferentes' }).click();
  await expect(reject).toBeHidden();
  await expect(page.getByRole('status').filter({ hasText: `Proposta rejeitada: as fichas de ${LUCIA}` })).toBeVisible();
  await expect(lucia).toHaveCount(0);
  await expect(roberto).toBeVisible();
  await page.getByLabel('Situação').selectOption({ label: 'Rejeitadas' });
  const rejected = page.getByRole('article', { name: LUCIA, exact: true });
  await expect(rejected.getByText('rejeitada — as fichas ficam como pessoas diferentes.')).toBeVisible();

  expect(problems).toEqual([]);
});
