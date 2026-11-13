import { describe, expect, test } from 'vitest';
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ProvenancePopover } from './ProvenancePopover';
import { renderAt, API } from '../test/render';
import { json, mockFetch } from '../test/fetchMock';
import { edge, provenance } from '../test/fixtures';

describe('ProvenancePopover', () => {
  test('clicar na fonte abre base, registro, data de coleta', async () => {
    const { calls } = mockFetch({ '/api/v1/relationships/501/provenance': json(provenance()) });
    renderAt(<ProvenancePopover relationshipId={501} />);
    expect(calls).toHaveLength(0); // fetched only when opened

    await userEvent.click(screen.getByRole('button', { name: /fonte/ }));
    const dialog = await screen.findByRole('dialog', { name: /Fonte do vínculo/ });
    await within(dialog).findByText('Receita Federal — CNPJ');
    expect(calls).toHaveLength(1);
    expect(calls[0]!.url.href).toBe(`${API}/api/v1/relationships/501/provenance`);
    expect(within(dialog).getByText('K3241.K03200Y0.D61010.SOCIOCSV:42')).toBeTruthy();
    expect(within(dialog).getByText('raw/receita/2026-10-01/Socios0.zip')).toBeTruthy();
    expect(within(dialog).getByText(/9f2c0d1e5b7a…/)).toBeTruthy();
    expect(within(dialog).getByRole('button', { name: /Copiar SHA-256/ })).toBeTruthy();
    expect(within(dialog).queryByText('vencido')).toBeNull();
    expect(dialog.textContent).toContain(
      'O arquivo original fica guardado pela plataforma; a impressão digital (SHA-256) permite conferir um novo download da fonte.',
    );
  });

  test('as duas coletas: a primeira que registrou o vínculo e a última que o confirmou, com as três datas', async () => {
    mockFetch({ '/api/v1/relationships/501/provenance': json(provenance()) });
    renderAt(<ProvenancePopover relationshipId={501} />);
    await userEvent.click(screen.getByRole('button', { name: /fonte/ }));
    const dialog = await screen.findByRole('dialog');
    const first = await within(dialog).findByRole('group', { name: 'Primeira coleta que registrou este vínculo' });
    expect(first.textContent).toContain('nº 7');
    expect(first.textContent).toContain('completa');
    expect(within(first).getByText('Data de referência')).toBeTruthy();
    expect(within(first).getByText('01/10/2026')).toBeTruthy();
    expect(within(first).getByText('Publicada ou observada em')).toBeTruthy();
    expect(within(first).getByText('13/10/2026')).toBeTruthy();
    expect(within(first).getByText('Coletada em')).toBeTruthy();
    expect(within(first).getByText(/01\/11\/2026/)).toBeTruthy();
    const last = within(dialog).getByRole('group', { name: 'Última coleta que o confirmou' });
    expect(last.textContent).toContain('nº 9');
    expect(within(last).getByText('01/11/2026')).toBeTruthy();
    expect(within(last).getByText('10/11/2026')).toBeTruthy();
    expect(within(last).getByText(/12\/11\/2026/)).toBeTruthy();
  });

  test('data observada igual à de referência não se repete; última coleta ausente → "não informado"', async () => {
    const p = provenance({ last_seen_collection: null });
    p.collection.observed_date = p.collection.reference_date;
    p.collection.collected_at = null;
    mockFetch({ '/api/v1/relationships/501/provenance': json(p) });
    renderAt(<ProvenancePopover relationshipId={501} />);
    await userEvent.click(screen.getByRole('button', { name: /fonte/ }));
    const dialog = await screen.findByRole('dialog');
    const first = await within(dialog).findByRole('group', { name: 'Primeira coleta que registrou este vínculo' });
    expect(within(first).queryByText('Publicada ou observada em')).toBeNull();
    expect(within(first).getByText('não registrada')).toBeTruthy();
    const last = within(dialog).getByRole('group', { name: 'Última coleta que o confirmou' });
    expect(last.textContent).toContain('não informado');
  });

  test('link "Abrir na fonte" em nova aba, sem referrer, com a URL também como texto e a nota da fonte', async () => {
    const p = provenance();
    mockFetch({ '/api/v1/relationships/501/provenance': json(p) });
    renderAt(<ProvenancePopover relationshipId={501} />);
    await userEvent.click(screen.getByRole('button', { name: /fonte/ }));
    const dialog = await screen.findByRole('dialog');
    const link = await within(dialog).findByRole('link', { name: 'Abrir na fonte' });
    expect(link.getAttribute('href')).toBe(p.source_url);
    expect(link.getAttribute('target')).toBe('_blank');
    expect(link.getAttribute('rel')).toBe('noopener noreferrer');
    expect(within(dialog).getByText(p.source_url!)).toBeTruthy();
    expect(within(dialog).getByText(p.source_note!)).toBeTruthy();
  });

  test.each(['javascript:alert(1)', 'ftp://exemplo.org/arquivo.zip', 'data:text/html,oi', '/relativo/arquivo.zip', 'http//quebrado'])(
    'URL que não é http/https (%s) aparece só como texto, sem link',
    async (url) => {
      mockFetch({ '/api/v1/relationships/501/provenance': json(provenance({ source_url: url, source_note: undefined })) });
      renderAt(<ProvenancePopover relationshipId={501} />);
      await userEvent.click(screen.getByRole('button', { name: /fonte/ }));
      const dialog = await screen.findByRole('dialog');
      expect(await within(dialog).findByText(url)).toBeTruthy();
      expect(within(dialog).queryByRole('link')).toBeNull();
      expect(dialog.querySelector('a')).toBeNull();
    },
  );

  test('sem URL registrada: diz que não há link, sem nota', async () => {
    mockFetch({ '/api/v1/relationships/501/provenance': json(provenance({ source_url: null, source_note: undefined })) });
    renderAt(<ProvenancePopover relationshipId={501} />);
    await userEvent.click(screen.getByRole('button', { name: /fonte/ }));
    const dialog = await screen.findByRole('dialog');
    expect(await within(dialog).findByText(/endereço de origem não registrado/)).toBeTruthy();
    expect(within(dialog).queryByRole('link')).toBeNull();
  });

  test('mostra "vencido" quando a fonte está desatualizada e omite o arquivo ausente', async () => {
    const p = provenance({ raw_key: undefined, raw_sha256: undefined, raw_size: undefined });
    p.collection.stale = true;
    mockFetch({ '/api/v1/relationships/501/provenance': json(p) });
    renderAt(<ProvenancePopover relationshipId={501} />);
    await userEvent.click(screen.getByRole('button', { name: /fonte/ }));
    const dialog = await screen.findByRole('dialog');
    expect(await within(dialog).findByText('vencido')).toBeTruthy();
    expect(within(dialog).queryByText(/SHA-256/)).toBeNull();
    expect(within(dialog).getByText(/arquivo bruto não registrado/)).toBeTruthy();
  });

  test('abre como diálogo modal nativo, fora da tabela', async () => {
    mockFetch({ '/api/v1/relationships/501/provenance': json(provenance()) });
    renderAt(<ProvenancePopover relationshipId={501} />);
    const trigger = screen.getByRole('button', { name: /fonte/ });
    expect(trigger.getAttribute('aria-expanded')).toBe('false');
    await userEvent.click(trigger);
    const dialog = await screen.findByRole('dialog');
    expect(dialog.tagName).toBe('DIALOG');
    expect((dialog as HTMLDialogElement).open).toBe(true);
    expect(trigger.getAttribute('aria-expanded')).toBe('true');
  });

  test('Esc fecha a fonte e devolve o foco ao botão', async () => {
    mockFetch({ '/api/v1/relationships/501/provenance': json(provenance()) });
    renderAt(<ProvenancePopover relationshipId={501} />);
    const trigger = screen.getByRole('button', { name: /fonte/ });
    await userEvent.click(trigger);
    const dialog = await screen.findByRole('dialog');
    await within(dialog).findByText('Receita Federal — CNPJ');
    expect(dialog.contains(document.activeElement)).toBe(true);

    await userEvent.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(document.activeElement).toBe(trigger);
  });

  test('"Fechar" e o clique no fundo também fecham e devolvem o foco', async () => {
    mockFetch({ '/api/v1/relationships/501/provenance': json(provenance()) });
    renderAt(<ProvenancePopover relationshipId={501} />);
    const trigger = screen.getByRole('button', { name: /fonte/ });

    await userEvent.click(trigger);
    let dialog = await screen.findByRole('dialog');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Fechar' }));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(document.activeElement).toBe(trigger);

    await userEvent.click(trigger);
    dialog = await screen.findByRole('dialog');
    await within(dialog).findByText('Receita Federal — CNPJ');
    await userEvent.click(dialog); // the backdrop is the dialog box itself
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(document.activeElement).toBe(trigger);
  });

  test('o foco fica preso dentro da fonte aberta', async () => {
    mockFetch({ '/api/v1/relationships/501/provenance': json(provenance()) });
    renderAt(<ProvenancePopover relationshipId={501} />);
    await userEvent.click(screen.getByRole('button', { name: /fonte/ }));
    const dialog = await screen.findByRole('dialog');
    await within(dialog).findByText('Receita Federal — CNPJ');
    for (let i = 0; i < 5; i++) {
      await userEvent.tab();
      expect(dialog.contains(document.activeElement)).toBe(true);
    }
  });

  test.each([
    ['auto', 0.99999, 'identidade 99%'],
    ['analyst', 1, 'identidade: decisão de analista'],
    ['unknown', 0.9, 'identidade: base não informada'],
  ] as const)('a natureza mostra a identidade do vínculo (%s, %s → "%s")', async (basis, c, text) => {
    mockFetch({ '/api/v1/relationships/501/provenance': json(provenance()) });
    renderAt(<ProvenancePopover relationshipId={501} edge={edge({ identity_basis: basis, identity_confidence: c })} />);
    await userEvent.click(screen.getByRole('button', { name: /fonte/ }));
    const dialog = await screen.findByRole('dialog');
    expect(await within(dialog).findByText(text)).toBeTruthy();
    expect(within(dialog).getByText('fato')).toBeTruthy();
  });

  test('identidade "none": a natureza não fala de identidade', async () => {
    mockFetch({ '/api/v1/relationships/501/provenance': json(provenance()) });
    renderAt(<ProvenancePopover relationshipId={501} edge={edge({ identity_basis: 'none', identity_confidence: 1 })} />);
    await userEvent.click(screen.getByRole('button', { name: /fonte/ }));
    const dialog = await screen.findByRole('dialog');
    await within(dialog).findByText('Receita Federal — CNPJ');
    expect(dialog.textContent).not.toContain('identidade');
  });
});
