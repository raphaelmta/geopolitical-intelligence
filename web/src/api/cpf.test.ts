import { describe, expect, test } from 'vitest';
import { mayCarryCPF, routeWithoutCPF } from './cpf';

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
    '１２３.４５６.７８９-０９',
    'maria １２３４５６７８９０９',
    '＊＊＊４５６７８９＊＊',
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

describe('routeWithoutCPF', () => {
  test('tira um q com CPF e mantém o resto', () => {
    expect(routeWithoutCPF('/', '?q=maria+123.456.789-09&tipo=person', '#x')).toBe('/?tipo=person#x');
    expect(routeWithoutCPF('/', '?q=%EF%BC%91%EF%BC%92%EF%BC%93.456.789-09', '')).toBe('/');
  });
  test('mantém um q sem CPF e outras rotas', () => {
    expect(routeWithoutCPF('/', '?q=maria', '')).toBe('/?q=maria');
    expect(routeWithoutCPF('/entidade/12', '', '')).toBe('/entidade/12');
    expect(routeWithoutCPF('/caminhos', '?a=12&b=13', '')).toBe('/caminhos?a=12&b=13');
  });
});
