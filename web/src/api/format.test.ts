import { describe, expect, test, vi } from 'vitest';
import {
  closedLabel,
  formatBytes,
  formatCNPJBase,
  formatConfidence,
  formatDate,
  formatMoney,
  formatPercent,
  formatTimestamp,
  relationLabel,
} from './format';
import { edge } from '../test/fixtures';

// vi.stubEnv sets process.env.TZ, which Node applies to Date and Intl at
// once; unstubEnvs (vite.config.ts) restores it after each test.

describe.each(['America/Sao_Paulo', 'UTC', 'Asia/Tokyo'])('datas no fuso %s', (tz) => {
  test('formatDate não desloca o dia', () => {
    vi.stubEnv('TZ', tz);
    expect(formatDate('2026-10-01')).toBe('01/10/2026');
    expect(formatDate('2026-01-01')).toBe('01/01/2026');
    expect(formatDate('2025-12-31')).toBe('31/12/2025');
  });
});

describe('formatDate', () => {
  test('valor inválido ou ausente', () => {
    expect(formatDate(null)).toBe('');
    expect(formatDate('2026-13-01x')).toBe('2026-13-01x');
  });
});

describe('formatTimestamp', () => {
  test('mostra o instante no horário local', () => {
    vi.stubEnv('TZ', 'America/Sao_Paulo');
    expect(formatTimestamp('2026-11-01T02:30:00Z')).toBe('31/10/2026 23:30');
    vi.stubEnv('TZ', 'UTC');
    expect(formatTimestamp('2026-11-01T02:30:00Z')).toBe('01/11/2026 02:30');
  });

  test('valor ausente ou inválido', () => {
    expect(formatTimestamp(null)).toBe('');
    expect(formatTimestamp('ontem')).toBe('ontem');
  });
});

describe('formatCNPJBase', () => {
  test('formata a raiz de 8 caracteres', () => {
    expect(formatCNPJBase('12345678')).toBe('12.345.678');
    expect(formatCNPJBase('12ABC345')).toBe('12.ABC.345');
  });
  test('outros tamanhos ficam como vieram', () => {
    expect(formatCNPJBase('123')).toBe('123');
    expect(formatCNPJBase('')).toBe('');
  });
});

describe('formatMoney', () => {
  test('formata decimal em texto sem perder precisão', () => {
    expect(formatMoney('1000.00')).toBe('R$ 1.000,00');
    expect(formatMoney('12345678901234567.89')).toBe('R$ 12.345.678.901.234.567,89');
    expect(formatMoney('0.5')).toBe('R$ 0,50');
    expect(formatMoney('7')).toBe('R$ 7,00');
    expect(formatMoney('0.125')).toBe('R$ 0,125');
    expect(formatMoney('-1500.10')).toBe('-R$ 1.500,10');
  });
  test('valor que não é decimal fica como veio', () => {
    expect(formatMoney('abc')).toBe('abc');
    expect(formatMoney(null)).toBe('');
  });
});

describe('formatPercent e formatBytes', () => {
  test('percentual em pt-BR', () => {
    expect(formatPercent(12.5)).toBe('12,5%');
    expect(formatPercent('33.33')).toBe('33,33%');
    expect(formatPercent(100)).toBe('100%');
  });
  test('tamanho de arquivo', () => {
    expect(formatBytes(251000000)).toBe('251 MB');
    expect(formatBytes(1500)).toBe('1,5 kB');
  });
});

describe('closedLabel', () => {
  test('intervalo de saída', () => {
    expect(closedLabel(edge({ valid_to_after: '2026-10-01', valid_to_before: '2026-11-01' }))).toBe(
      'saiu entre 01/10/2026 e 01/11/2026',
    );
    expect(closedLabel(edge({ valid_to_after: '2026-10-01', valid_to_before: '2026-10-01' }))).toBe(
      'encerrado em 01/10/2026',
    );
    expect(closedLabel(edge({ valid_to_after: null, valid_to_before: '2026-11-01' }))).toBe('saiu antes de 01/11/2026');
    expect(closedLabel(edge({ valid_to_after: '2026-10-01', valid_to_before: null }))).toBe('saiu depois de 01/10/2026');
    expect(closedLabel(edge({ valid_to_after: null, valid_to_before: null }))).toBe('encerrado (data desconhecida)');
  });
});

describe('relationLabel', () => {
  test('rótulo depende do sentido do vínculo', () => {
    expect(relationLabel('partner_of', 'out')).toBe('é sócio de');
    expect(relationLabel('partner_of', 'in')).toBe('tem como sócio');
    expect(relationLabel('donated_to', 'out')).toBe('doou para');
  });
  test('tipo desconhecido mostra o código', () => {
    expect(relationLabel('novo_tipo', 'out')).toBe('novo_tipo');
  });
});

describe('formatConfidence', () => {
  test.each([
    [1, '100%'],
    [0.9999, '99%'],
    [0.999, '99%'],
    [0.995, '99%'],
    [0.994, '99%'],
    [0.8, '80%'],
    [0.666, '67%'],
    [0.005, '1%'],
    [0.004, '< 1%'],
    [0.0001, '< 1%'],
    [0, '0%'],
  ])('%s → %s', (v, want) => {
    expect(formatConfidence(v)).toBe(want);
  });
  test.each([null, undefined, NaN, Infinity, 1.2, -0.1, '0.5'])('%s → null', (v) => {
    expect(formatConfidence(v)).toBeNull();
  });
});
