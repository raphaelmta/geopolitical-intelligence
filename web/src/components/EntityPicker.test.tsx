import { describe, expect, test, vi } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { EntityPicker } from './EntityPicker';
import type { EntityRef } from '../api/types';
import { renderAt } from '../test/render';
import { json, mockFetch } from '../test/fetchMock';

function Harness({ onChange }: { onChange: (v: EntityRef | null) => void }) {
  const [value, setValue] = useState<EntityRef | null>(null);
  return (
    <EntityPicker
      label="De"
      value={value}
      onChange={(v) => {
        setValue(v);
        onChange(v);
      }}
    />
  );
}

const items = [
  { id: 12, kind: 'company', name: 'EMPRESA FICTICIA A LTDA', natural_key: '12345678' },
  { id: 11, kind: 'person', name: 'PESSOA FICTICIA UM', natural_key: '' },
];

describe('EntityPicker', () => {
  test('com menos de 3 caracteres úteis não chama a API', async () => {
    const { fn } = mockFetch({});
    renderAt(<Harness onChange={vi.fn()} />);
    await userEvent.type(screen.getByRole('combobox', { name: 'De' }), 'a .b');
    await new Promise((r) => setTimeout(r, 600));
    expect(fn).not.toHaveBeenCalled();
    expect(screen.getByText(/pelo menos 3 caracteres/)).toBeTruthy();
  });

  test('busca enquanto digita, mostra tipo e CNPJ formatado e escolhe pelo teclado', async () => {
    const onChange = vi.fn();
    const { calls } = mockFetch({ '/api/v1/search': json({ items }) });
    renderAt(<Harness onChange={onChange} />);
    const input = screen.getByRole('combobox', { name: 'De' });
    await userEvent.type(input, 'ficticia');
    const options = await screen.findAllByRole('option');
    expect(options).toHaveLength(2);
    expect(options[0]!.textContent).toContain('Empresa');
    expect(options[0]!.textContent).toContain('12.345.678');
    expect(calls.at(-1)!.url.searchParams.get('q')).toBe('ficticia');
    expect(input.getAttribute('aria-expanded')).toBe('true');
    expect(input.getAttribute('aria-controls')).toBe(screen.getByRole('listbox').id);
    expect(input.hasAttribute('aria-activedescendant')).toBe(false);

    await userEvent.keyboard('{ArrowDown}{ArrowDown}');
    expect(input.getAttribute('aria-activedescendant')).toBe(options[1]!.id);
    await userEvent.keyboard('{Enter}');
    expect(onChange).toHaveBeenLastCalledWith(items[1]);
    expect(screen.queryByRole('listbox')).toBeNull();
    expect(screen.getByText('PESSOA FICTICIA UM')).toBeTruthy();

    await userEvent.click(screen.getByRole('button', { name: /Trocar/ }));
    expect(onChange).toHaveBeenLastCalledWith(null);
    expect(document.activeElement).toBe(screen.getByRole('combobox', { name: 'De' }));
  });

  test('Esc fecha a lista e tira aria-controls, aria-expanded e aria-activedescendant', async () => {
    mockFetch({ '/api/v1/search': json({ items }) });
    renderAt(<Harness onChange={vi.fn()} />);
    const input = screen.getByRole('combobox', { name: 'De' });
    await userEvent.type(input, 'ficticia');
    await screen.findAllByRole('option');
    await userEvent.keyboard('{ArrowDown}');
    expect(input.getAttribute('aria-activedescendant')).toBeTruthy();
    await userEvent.keyboard('{Escape}');
    expect(screen.queryByRole('listbox')).toBeNull();
    expect(input.getAttribute('aria-expanded')).toBe('false');
    expect(input.hasAttribute('aria-controls')).toBe(false);
    expect(input.hasAttribute('aria-activedescendant')).toBe(false);
  });

  test('aceita CPF: candidatos pelo trecho do CPF, com o aviso', async () => {
    const onChange = vi.fn();
    // Synthetic CPF with valid check digits.
    const { calls } = mockFetch({
      '/api/v1/search': json({
        cpf_fragment_search: true,
        items: [{ id: 31, kind: 'person', name: 'MARIA FICTICIA', natural_key: '', match: 'cpf_fragment' }],
      }),
    });
    renderAt(<Harness onChange={onChange} />);
    await userEvent.type(screen.getByRole('combobox', { name: 'De' }), 'maria 123.456.789-09');
    const options = await screen.findAllByRole('option');
    expect(options).toHaveLength(1);
    expect(options[0]!.textContent).toContain('coincide com o trecho do CPF');
    expect(calls.at(-1)!.url.searchParams.get('q')).toBe('maria 123.456.789-09');
    expect(screen.getByText(/não uma confirmação/)).toBeTruthy();
    await userEvent.click(options[0]!);
    expect(onChange).toHaveBeenLastCalledWith({ id: 31, kind: 'person', name: 'MARIA FICTICIA', natural_key: '' });
  });
});
