import { describe, expect, test } from 'vitest';
import { mayCarryCPF } from './cpf';

// Synthetic CPFs only (123.456.789-09 has valid check digits).
describe('mayCarryCPF', () => {
  test.each([
    '12345678909',
    '123.456.789-09',
    '123 456 789 09',
    '1234 5678 909',
    'maria silva 123.456.789-09',
    '123.456.789-01 maria',
    'maria12345678909',
    '***456789**',
    '***.456.789-**',
    'maria ***456789**',
    '123.456.789-09 111.222.333-96',
  ])('%s carrega um CPF', (q) => {
    expect(mayCarryCPF(q)).toBe(true);
  });

  test.each(['456789', '12345678', '12.345.678/0001-90', '12345678000190', 'maria silva', 'posto 15', 'rua 7 123', '123456789'])(
    '%s não carrega CPF',
    (q) => {
      expect(mayCarryCPF(q)).toBe(false);
    },
  );
});
