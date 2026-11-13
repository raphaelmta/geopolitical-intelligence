import { expect, test } from 'vitest';
import { render, screen } from '@testing-library/react';
import { StaleTag } from './StaleTag';

test('StaleTag mostra "vencido" com explicação', () => {
  render(<StaleTag />);
  const tag = screen.getByText('vencido');
  expect(tag.getAttribute('title')).toMatch(/desatualizada/);
});
