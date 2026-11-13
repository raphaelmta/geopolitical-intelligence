import { describe, expect, test } from 'vitest';
import { identityBasisOf, identityTag, isClosed, pathHasDoubt } from './edges';
import { edge } from '../test/fixtures';

describe('identityBasisOf', () => {
  test.each(['none', 'auto', 'analyst', 'unknown'] as const)('base enviada pela API (%s) é usada como está', (b) => {
    expect(identityBasisOf({ identity_basis: b, identity_confidence: 0.5 })).toBe(b);
  });

  test('sem o campo: sem registro de pessoa → none; com registro → unknown (nunca certeza)', () => {
    expect(identityBasisOf({})).toBe('none');
    expect(identityBasisOf({ identity_confidence: 1 })).toBe('unknown');
    expect(identityBasisOf({ identity_confidence: 0.9 })).toBe('unknown');
  });

  test('valor desconhecido → unknown', () => {
    expect(identityBasisOf({ identity_basis: 'algo-novo' })).toBe('unknown');
  });
});

describe('identityTag', () => {
  test('none → nada, mesmo com confiança', () => {
    expect(identityTag({ identity_basis: 'none', identity_confidence: 0.5 })).toBeNull();
  });

  test('auto → "identidade NN%" e a explicação', () => {
    const t = identityTag({ identity_basis: 'auto', identity_confidence: 0.99999 })!;
    expect(t.text).toBe('identidade 99%');
    expect(t.title).toContain('pessoas unidas automaticamente por nome e trecho do CPF');
  });

  test.each([1, null, undefined, NaN, 1.5])('auto com confiança %s nunca mostra 100%%', (c) => {
    const t = identityTag({ identity_basis: 'auto', identity_confidence: c })!;
    expect(t.text).toBe('identidade · confiança não informada');
    expect(t.text).not.toMatch(/\d%/);
  });

  test('analyst → decisão de analista, sem porcentagem', () => {
    const t = identityTag({ identity_basis: 'analyst', identity_confidence: 1 })!;
    expect(t.text).toBe('identidade: decisão de analista');
    expect(t.text).not.toContain('%');
    expect(t.title).toMatch(/analista/);
  });

  test('unknown → base não informada', () => {
    expect(identityTag({ identity_basis: 'unknown', identity_confidence: 0.9 })!.text).toBe(
      'identidade: base não informada',
    );
  });
});

describe('isClosed', () => {
  test('usa o campo closed da API, não as datas', () => {
    expect(isClosed(edge({ closed: true, valid_to_after: null, valid_to_before: null }))).toBe(true);
    expect(isClosed(edge({ closed: false, valid_to_after: '2026-01-01', valid_to_before: '2026-02-01' }))).toBe(false);
  });

  test('sem o campo (resultado de job antigo): deduz pelas datas', () => {
    expect(isClosed(edge({ closed: undefined, valid_to_before: '2026-02-01' }))).toBe(true);
    expect(isClosed(edge({ closed: undefined, valid_to_after: '2026-01-01' }))).toBe(true);
    expect(isClosed(edge({ closed: undefined }))).toBe(false);
  });
});

describe('pathHasDoubt', () => {
  test('inferência ou identidade unida em algum elo', () => {
    expect(pathHasDoubt([edge()])).toBe(false);
    expect(pathHasDoubt([edge(), edge({ kind: 'inference', confidence: 0.5 })])).toBe(true);
    expect(pathHasDoubt([edge({ identity_basis: 'auto', identity_confidence: 0.99 })])).toBe(true);
    expect(pathHasDoubt([edge({ identity_basis: 'analyst', identity_confidence: 1 })])).toBe(true);
    expect(pathHasDoubt([edge({ identity_basis: 'unknown' })])).toBe(true);
    expect(pathHasDoubt([edge({ identity_basis: 'none', identity_confidence: 1 })])).toBe(false);
  });
});
