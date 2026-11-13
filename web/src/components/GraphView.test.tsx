import { beforeEach, describe, expect, test, vi } from 'vitest';
import { act, fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { GraphPanel } from './GraphView';
import { toElements } from './graphStyle';
import { edge } from '../test/fixtures';
import { graphs, lastGraph, resetGraphs } from '../test/graphMock';

vi.mock('./graphAdapter', () => import('../test/graphMock'));

const elements = toElements([edge({ relationship_id: 1 })]);

beforeEach(() => resetGraphs());

function panel(props: Partial<Parameters<typeof GraphPanel>[0]> = {}) {
  return (
    <GraphPanel
      label="Mapa de vínculos"
      elements={elements}
      selectedId={null}
      onSelect={vi.fn()}
      {...props}
    />
  );
}

describe('GraphPanel', () => {
  test('a lista não espera o gráfico: aviso enquanto o código do gráfico carrega', async () => {
    render(panel());
    expect(screen.getByRole('status').textContent).toBe('Carregando o gráfico…');
    await screen.findByRole('application');
    expect(screen.queryByText('Carregando o gráfico…')).toBeNull();
  });

  test('monta o gráfico com os elementos, destaca o selecionado e desmonta limpo', async () => {
    const { rerender, unmount } = render(panel());
    const g = await lastGraph();
    expect(g.ids()).toEqual(['e1', 'n11', 'n12']);
    rerender(panel({ selectedId: 'e1' }));
    expect(g.handle.highlight).toHaveBeenLastCalledWith('e1');
    unmount();
    expect(g.handle.destroy).toHaveBeenCalledTimes(1);
  });

  test('o gráfico é uma aplicação com nome e instrução que remete à lista', async () => {
    render(panel());
    const app = await screen.findByRole('application', { name: /Mapa de vínculos/ });
    expect(app.tabIndex).toBe(0);
    expect(document.getElementById(app.getAttribute('aria-describedby')!)!.textContent).toMatch(
      /Use a lista abaixo para navegar pelos vínculos/,
    );
  });

  test('ocultar o gráfico desmonta o Cytoscape; mostrar de novo monta outro', async () => {
    render(panel());
    await lastGraph();
    await userEvent.click(screen.getByRole('button', { name: 'Ocultar o gráfico' }));
    expect(graphs[0]!.handle.destroy).toHaveBeenCalled();
    expect(screen.queryByRole('application')).toBeNull();
    await userEvent.click(screen.getByRole('button', { name: 'Mostrar o gráfico' }));
    await vi.waitFor(() => expect(graphs).toHaveLength(2));
  });

  test('clique e duplo clique no gráfico chegam aos callbacks; Enter abre o nó selecionado', async () => {
    const onSelect = vi.fn();
    const onActivate = vi.fn();
    render(panel({ onSelect, onActivate, selectedId: 'n11' }));
    const g = await lastGraph();
    act(() => g.callbacks.onSelect('n12'));
    expect(onSelect).toHaveBeenCalledWith('n12');
    act(() => g.callbacks.onActivate('n12'));
    expect(onActivate).toHaveBeenCalledWith('n12');
    fireEvent.keyDown(screen.getByRole('application'), { key: 'Enter' });
    expect(onActivate).toHaveBeenLastCalledWith('n11');
  });

  test('troca de tema claro/escuro refaz as cores', async () => {
    const listeners: (() => void)[] = [];
    vi.stubGlobal(
      'matchMedia',
      vi.fn((q: string) => ({
        media: q,
        matches: false,
        addEventListener: (_: string, l: () => void) => listeners.push(l),
        removeEventListener: vi.fn(),
      })),
    );
    render(panel());
    const g = await lastGraph();
    act(() => listeners.forEach((l) => l()));
    expect(g.handle.restyle).toHaveBeenCalled();
  });

  test('na tela de celular o gráfico começa oculto', async () => {
    vi.stubGlobal(
      'matchMedia',
      vi.fn((q: string) => ({
        media: q,
        matches: q.includes('max-width'),
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
      })),
    );
    render(panel());
    // Hidden means not mounted at all: not even the lazy chunk's placeholder.
    expect(screen.queryByText('Carregando o gráfico…')).toBeNull();
    expect(screen.queryByRole('application')).toBeNull();
    expect(graphs).toHaveLength(0);
    // Shown on request: the graph mounts then (and only then).
    await userEvent.click(screen.getByRole('button', { name: 'Mostrar o gráfico' }));
    await lastGraph();
    expect(graphs).toHaveLength(1);
    expect(screen.getByRole('button', { name: 'Ocultar o gráfico' })).toBeTruthy();
  });

  test('a legenda do gráfico explica a marca de identidade unida em texto', () => {
    render(panel());
    const lines = screen.getByRole('list', { name: 'Linhas do gráfico' });
    expect(lines.textContent).toContain('círculo vazado no início');
    expect(lines.textContent).toContain('identidade unida');
  });
});
