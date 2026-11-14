import { beforeEach, describe, expect, test, vi } from 'vitest';
import { act, fireEvent, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { GraphPanel } from './GraphView';
import { toElements } from './graphStyle';
import { edge } from '../test/fixtures';
import { graphs, lastGraph, layouts, resetGraphs } from '../test/graphMock';

vi.mock('./graphAdapter', () => import('../test/graphMock'));

const elements = toElements([edge({ relationship_id: 1 })]);

beforeEach(() => {
  resetGraphs();
  document.documentElement.classList.remove('no-scroll');
});

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
      /Clique numa entidade ou numa linha para ver os detalhes.*As listas abaixo têm tudo o que o gráfico mostra/,
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
    expect(lines.textContent).toContain('linha tracejada: inferência');
    expect(lines.textContent).toContain('círculo vazado no início');
    expect(lines.textContent).toContain('identidade unida');
  });

  test('a dica muda quando o gráfico aceita duplo clique', async () => {
    render(panel({ onActivate: vi.fn() }));
    const app = await screen.findByRole('application');
    expect(document.getElementById(app.getAttribute('aria-describedby')!)!.textContent).toMatch(/duplo clique \(ou Enter\)/);
  });

  test('a legenda mostra um ícone por tipo de entidade e a marca "+" só quando expansível', () => {
    const { rerender } = render(panel());
    const kinds = screen.getByRole('list', { name: 'Tipos de entidade' });
    expect(kinds.querySelectorAll('li')).toHaveLength(5);
    expect(kinds.textContent).toContain('Órgão público');
    expect(screen.getByRole('list', { name: 'Linhas do gráfico' }).textContent).not.toContain('ainda não trazidos');
    rerender(panel({ expandable: true }));
    expect(screen.getByRole('list', { name: 'Linhas do gráfico' }).textContent).toContain('vínculos ainda não trazidos');
  });

  test('o painel de detalhes aparece ao lado do gráfico, com nome acessível', async () => {
    render(panel({ details: <p>conteúdo do painel</p> }));
    await lastGraph();
    const aside = screen.getByRole('complementary', { name: 'Detalhes do que está selecionado' });
    expect(aside.textContent).toBe('conteúdo do painel');
    expect(aside.getAttribute('aria-live')).toBe('polite');
  });

  test('o layout pedido segue até o adaptador', async () => {
    render(panel({ layout: 'flow' }));
    await lastGraph();
    expect(layouts).toEqual(['flow']);
  });

  test('um clique no fundo chega ao onClear', async () => {
    const onClear = vi.fn();
    render(panel({ onClear }));
    const g = await lastGraph();
    act(() => g.callbacks.onClear!());
    expect(onClear).toHaveBeenCalledTimes(1);
  });

  describe('barra de ferramentas', () => {
    test('Aproximar e Afastar dão zoom pelos fatores combinados; Ajustar e Reorganizar chamam o gráfico', async () => {
      render(panel());
      const g = await lastGraph();
      const tools = screen.getByRole('group', { name: 'Controles do gráfico' });
      await userEvent.click(within(tools).getByRole('button', { name: 'Aproximar' }));
      expect(g.handle.zoom).toHaveBeenLastCalledWith(1.25);
      await userEvent.click(within(tools).getByRole('button', { name: 'Afastar' }));
      expect(g.handle.zoom).toHaveBeenLastCalledWith(0.8);
      await userEvent.click(within(tools).getByRole('button', { name: 'Ajustar à tela' }));
      expect(g.handle.fit).toHaveBeenCalledTimes(1);
      await userEvent.click(within(tools).getByRole('button', { name: 'Reorganizar' }));
      expect(g.handle.arrange).toHaveBeenCalledTimes(1);
    });

    test('as teclas + e - também dão zoom', async () => {
      render(panel());
      const g = await lastGraph();
      const app = screen.getByRole('application');
      fireEvent.keyDown(app, { key: '+' });
      expect(g.handle.zoom).toHaveBeenLastCalledWith(1.25);
      fireEvent.keyDown(app, { key: '-' });
      expect(g.handle.zoom).toHaveBeenLastCalledWith(0.8);
    });

    test('"Nomes em todas as linhas" liga e desliga o modo "all" das etiquetas', async () => {
      render(panel());
      const g = await lastGraph();
      expect(g.handle.setEdgeLabels).toHaveBeenLastCalledWith('auto');
      const box = screen.getByRole('checkbox', { name: 'Nomes em todas as linhas' });
      await userEvent.click(box);
      expect(g.handle.setEdgeLabels).toHaveBeenLastCalledWith('all');
      await userEvent.click(box);
      expect(g.handle.setEdgeLabels).toHaveBeenLastCalledWith('auto');
    });

    test('Tela cheia alterna a classe do painel, o aria-pressed e o tamanho do canvas; Esc sai', async () => {
      const { container } = render(panel());
      const g = await lastGraph();
      const full = screen.getByRole('button', { name: 'Tela cheia' });
      expect(full.getAttribute('aria-pressed')).toBe('false');
      expect(container.querySelector('.graph-panel--full')).toBeNull();
      expect(g.handle.resize).not.toHaveBeenCalled();
      await userEvent.click(full);
      expect(container.querySelector('.graph-panel--full')).not.toBeNull();
      expect(document.documentElement.classList.contains('no-scroll')).toBe(true);
      const exit = screen.getByRole('button', { name: 'Sair da tela cheia' });
      expect(exit.getAttribute('aria-pressed')).toBe('true');
      expect(g.handle.resize).toHaveBeenCalledTimes(1);
      fireEvent.keyDown(screen.getByRole('application'), { key: 'Escape' });
      expect(container.querySelector('.graph-panel--full')).toBeNull();
      expect(document.documentElement.classList.contains('no-scroll')).toBe(false);
      expect(screen.getByRole('button', { name: 'Tela cheia' }).getAttribute('aria-pressed')).toBe('false');
      expect(g.handle.resize).toHaveBeenCalledTimes(2);
    });

    test('ocultar o gráfico sai da tela cheia', async () => {
      const { container } = render(panel());
      await lastGraph();
      await userEvent.click(screen.getByRole('button', { name: 'Tela cheia' }));
      await userEvent.click(screen.getByRole('button', { name: 'Ocultar o gráfico' }));
      expect(container.querySelector('.graph-panel--full')).toBeNull();
      expect(document.documentElement.classList.contains('no-scroll')).toBe(false);
    });

    test('a dica do ponteiro aparece com onHover e some ao sair', async () => {
      const { container } = render(panel());
      const g = await lastGraph();
      act(() => g.callbacks.onHover!({ text: 'EMPRESA · Empresa', x: 10, y: 20 }));
      const tip = container.querySelector('.graph-tip')!;
      expect(tip.textContent).toBe('EMPRESA · Empresa');
      expect(tip.getAttribute('aria-hidden')).toBe('true');
      act(() => g.callbacks.onHover!(null));
      expect(container.querySelector('.graph-tip')).toBeNull();
    });
  });
});
