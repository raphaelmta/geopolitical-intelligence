import { expect, test } from 'vitest';
import { render, screen } from '@testing-library/react';
import { Timeline } from './Timeline';

test('Timeline lista os itens na ordem dada e marca o atual', () => {
  render(
    <Timeline
      label="Versões"
      items={[
        { key: 'b', title: 'Versão atual', current: true, children: <p>nova</p> },
        { key: 'a', title: 'Versão encerrada', children: <p>antiga</p> },
      ]}
    />,
  );
  const list = screen.getByRole('list', { name: 'Versões' });
  const items = list.querySelectorAll('li');
  expect(items).toHaveLength(2);
  expect(items[0]!.textContent).toContain('nova');
  expect(items[0]!.textContent).toContain('atual');
  expect(items[1]!.textContent).toContain('antiga');
});
