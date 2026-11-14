import { describe, expect, test, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { Edge } from '../api/types';
import { GraphDetails } from './GraphDetails';
import { entitiesOf } from './graphStyle';
import { edge } from '../test/fixtures';

const person = { id: 11, kind: 'person', name: 'PESSOA FICTICIA UM', natural_key: '' };
const companyA = { id: 12, kind: 'company', name: 'EMPRESA FICTICIA A LTDA', natural_key: '90000002' };
const companyB = { id: 13, kind: 'company', name: 'EMPRESA FICTICIA B LTDA', natural_key: '90000003' };

const edges: Edge[] = [
  edge({ relationship_id: 1, src: person, dst: companyA }),
  edge({ relationship_id: 2, type: 'administrator_of', src: person, dst: companyA }),
  edge({ relationship_id: 3, type: 'controls', src: companyA, dst: person, kind: 'inference', confidence: 0.7 }),
  edge({ relationship_id: 4, src: person, dst: companyB, stale: true }),
];

function show(props: Partial<Parameters<typeof GraphDetails>[0]> = {}) {
  const onSelect = vi.fn();
  const result = render(
    <QueryClientProvider client={new QueryClient()}>
      <MemoryRouter>
        <GraphDetails edges={edges} entities={entitiesOf(edges)} selected={null} onSelect={onSelect} {...props} />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { onSelect, ...result };
}

describe('GraphDetails', () => {
  test('sem seleção: explica como ler o gráfico', () => {
    show();
    expect(screen.getByRole('heading', { name: 'Como ler o gráfico' })).toBeTruthy();
    expect(screen.getByText(/Clique num círculo ou numa linha/)).toBeTruthy();
  });

  test('sem seleção, o texto de "idle" substitui o padrão', () => {
    show({ idle: <p>escolha algo</p> });
    expect(screen.getByText('escolha algo')).toBeTruthy();
    expect(screen.queryByRole('heading', { name: 'Como ler o gráfico' })).toBeNull();
  });

  test('seleção desconhecida cai no texto padrão', () => {
    show({ selected: 'n404' });
    expect(screen.getByRole('heading', { name: 'Como ler o gráfico' })).toBeTruthy();
  });

  test('entidade selecionada: nome, tipo, chave, ações e seus vínculos com selos', () => {
    const actions = vi.fn((n: { name: string }) => <button type="button">Expandir {n.name}</button>);
    show({ selected: 'n12', actions });
    expect(screen.getByRole('heading', { level: 3, name: 'EMPRESA FICTICIA A LTDA' })).toBeTruthy();
    expect(screen.getByText('Empresa')).toBeTruthy();
    expect(screen.getByText('90.000.002')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Expandir EMPRESA FICTICIA A LTDA' })).toBeTruthy();
    expect(actions).toHaveBeenCalledWith(expect.objectContaining({ id: 12 }));
    // three links touch the company (1, 2 in; 3 out), each with its badge
    expect(screen.getByRole('heading', { level: 4, name: '3 vínculos no gráfico' })).toBeTruthy();
    const items = screen.getAllByRole('listitem');
    expect(items).toHaveLength(3);
    expect(items[0]!.textContent).toContain('tem como sócio');
    expect(items[0]!.textContent).toContain('PESSOA FICTICIA UM');
    expect(items[0]!.textContent).toContain('fato');
    expect(items[1]!.textContent).toContain('tem como administrador');
    expect(items[2]!.textContent).toContain('controla');
    expect(items[2]!.textContent).toContain('inferência · 70%');
  });

  test('um só vínculo usa o singular; vínculo vencido mostra o selo', () => {
    show({ selected: 'n13' });
    expect(screen.getByRole('heading', { level: 4, name: '1 vínculo no gráfico' })).toBeTruthy();
    expect(screen.getByRole('listitem').textContent).toContain('vencido');
  });

  test('clicar em outra entidade da lista chama onSelect com o id do nó', async () => {
    const { onSelect } = show({ selected: 'n12' });
    await userEvent.click(screen.getAllByRole('button', { name: 'PESSOA FICTICIA UM' })[0]!);
    expect(onSelect).toHaveBeenCalledWith('n11');
  });

  test('laço: "a própria entidade", sem botão', () => {
    const loop = [edge({ relationship_id: 9, src: companyA, dst: companyA, type: 'shareholder_of' })];
    show({ edges: loop, entities: entitiesOf(loop), selected: 'n12' });
    expect(screen.getByText('a própria entidade')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'EMPRESA FICTICIA A LTDA' })).toBeNull();
  });

  test('mais de 12 vínculos: lista os primeiros e remete às listas', () => {
    const many = Array.from({ length: 15 }, (_, i) =>
      edge({ relationship_id: 100 + i, src: companyA, dst: { ...companyB, id: 200 + i, name: `FILHA ${i}` } }),
    );
    show({ edges: many, entities: entitiesOf(many), selected: 'n12' });
    expect(screen.getAllByRole('listitem')).toHaveLength(12);
    expect(screen.getByText('E mais 3; todos estão nas listas abaixo.')).toBeTruthy();
  });

  test('linha selecionada: todos os vínculos entre o par, nos dois sentidos, cada um com "fonte"', () => {
    show({ selected: 'e1' });
    expect(screen.getByText('3 vínculos entre')).toBeTruthy();
    const pair = screen.getByRole('heading', { level: 3 });
    expect(within(pair).getByRole('link', { name: 'PESSOA FICTICIA UM' }).getAttribute('href')).toBe('/entidade/11');
    expect(within(pair).getByRole('link', { name: 'EMPRESA FICTICIA A LTDA' }).getAttribute('href')).toBe('/entidade/12');
    const cards = screen.getAllByRole('listitem');
    expect(cards).toHaveLength(3); // 4 is with another company
    expect(cards[0]!.textContent).toContain('PESSOA FICTICIA UM é sócio de EMPRESA FICTICIA A LTDA');
    expect(cards[1]!.textContent).toContain('PESSOA FICTICIA UM é administrador de EMPRESA FICTICIA A LTDA');
    expect(cards[2]!.textContent).toContain('EMPRESA FICTICIA A LTDA controla PESSOA FICTICIA UM');
    expect(cards[2]!.textContent).toContain('inferência · 70%');
    expect(cards[0]!.className).toBe('is-selected');
    expect(cards[1]!.className).toBe('');
    // one "fonte" button per relationship, each naming it
    for (const id of [1, 2, 3]) {
      expect(screen.getByRole('button', { name: `fonte do vínculo nº ${id}, no painel` }).textContent).toBe('fonte');
    }
    expect(screen.queryByRole('button', { name: /vínculo nº 4/ })).toBeNull();
    expect(cards[0]!.textContent).toContain('receita');
  });

  test('a linha escolhida pelo id de outro membro marca o cartão certo', () => {
    show({ selected: 'e3' });
    expect(screen.getByText('3 vínculos entre')).toBeTruthy();
    const cards = screen.getAllByRole('listitem');
    expect(cards.map((c) => c.className)).toEqual(['', '', 'is-selected']);
  });

  test('um vínculo só: "1 vínculo entre"', () => {
    show({ selected: 'e4' });
    expect(screen.getByText('1 vínculo entre')).toBeTruthy();
    expect(screen.getAllByRole('listitem')).toHaveLength(1);
    expect(screen.getByText('vencido')).toBeTruthy();
  });
});
