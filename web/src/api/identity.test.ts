import { describe, expect, test } from 'vitest';
import { contextLabel, cpfLabel, proposalReason, REASON_OTHER, validReason } from './identity';

describe('identity formatting', () => {
  test.each([
    ['12345678', 'empresa 12.345.678'],
    ['A1B2C3D4', 'empresa A1.B2C.3D4'],
    ['cvm:90000002', 'empresa 90.000.002'],
    ['cand:2022:250001234567', 'candidatura 2022'],
    ['donor:2024:250000000001', 'doação 2024'],
    ['dep:204554', 'deputado'],
    ['sen:5012', 'senador'],
    ['outra:coisa', 'outra:coisa'],
    ['cand:abc', 'cand:abc'],
    ['', '—'],
  ])('contexto %s → %s', (raw, want) => {
    expect(contextLabel(raw)).toBe(want);
  });

  test('CPF ausente diz que não foi publicado', () => {
    expect(cpfLabel(undefined)).toBe('CPF não publicado');
    expect(cpfLabel('')).toBe('CPF não publicado');
    expect(cpfLabel('***777777**')).toBe('***777777**');
  });

  test('cada reason_code vira uma frase em português', () => {
    expect(proposalReason('common_name')).toMatch(/^Nome comum: há mais trechos do CPF diferentes com este nome/);
    expect(proposalReason('ambiguous_key')).toMatch(/^Um analista já separou fichas com este nome e este trecho do CPF/);
    expect(proposalReason('impure_target')).toMatch(/entidade montada por um analista/);
  });

  test.each(['other', undefined, '', 'codigo-novo'])('código %s → frase cautelosa, sem texto do servidor', (code) => {
    expect(proposalReason(code)).toBe(REASON_OTHER);
  });

  test('o texto em inglês do servidor não é mais interpretado', () => {
    // a reason string alone (older server, no reason_code) gets the cautious wording
    expect(proposalReason(undefined)).toBe(REASON_OTHER);
    expect(REASON_OTHER).not.toMatch(/distinct|fragment|analyst/);
  });

  test('motivo de uma decisão: 1 a 500 caracteres, sem contar espaços nas pontas', () => {
    expect(validReason('')).toBe(false);
    expect(validReason('   \n ')).toBe(false);
    expect(validReason(' a ')).toBe(true);
    expect(validReason('x'.repeat(500))).toBe(true);
    expect(validReason('x'.repeat(501))).toBe(false);
    // characters, not UTF-16 units
    expect(validReason('😀'.repeat(500))).toBe(true);
  });
});
