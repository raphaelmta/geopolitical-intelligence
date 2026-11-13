import { describe, expect, test } from 'vitest';
import { attrLabels, formatDecimalPercent, participationLabel } from './attrs';

describe('formatDecimalPercent', () => {
  test('decimal em texto, sem perder precisão', () => {
    expect(formatDecimalPercent('51.5')).toBe('51,5%');
    expect(formatDecimalPercent('33.333333333333333')).toBe('33,333333333333333%');
    expect(formatDecimalPercent('50.000000')).toBe('50%');
    expect(formatDecimalPercent('0.10')).toBe('0,1%');
    expect(formatDecimalPercent('1000')).toBe('1.000%');
  });
  test('número (dado antigo) e valor estranho', () => {
    expect(formatDecimalPercent(12.5)).toBe('12,5%');
    expect(formatDecimalPercent('abc')).toBe('abc');
  });
});

describe('attrLabels', () => {
  test.each([
    ['partner_of', { role: '22' }, ['sócio, participação não informada', 'qualificação 22']],
    ['partner_of', { role: '49', percent: '12.5' }, ['sócio, participação 12,5%', 'qualificação 49']],
    ['administrator_of', { role: '05' }, ['qualificação 05']],
    ['legal_rep_of', { role: '' }, []],
    [
      'shareholder_of',
      { percent: '51.25', percent_on: '60', percent_pn: '0.5', reference_date: '2026-01-31', form_version: 3, described_by: '12345678', role: 'shareholder' },
      [
        'acionista, participação 51,25%',
        'ON 60%',
        'PN 0,5%',
        'referência 31/01/2026',
        'formulário versão 3',
        'participação indireta, descrita no formulário da companhia 12.345.678',
      ],
    ],
    ['shareholder_of', { percent_on: '10' }, ['acionista, participação não informada', 'ON 10%']],
    ['controls', { percent: '100' }, ['controle declarado, participação 100%']],
    ['controls', { role: 'controller', reference_date: '2026-01-31' }, ['controle declarado', 'referência 31/01/2026']],
    [
      'holds_mandate',
      { legislature: 57, uf: 'SP', party: 'PFIC', status: 'exercicio', role: '57:PFIC:SP:exercicio' },
      ['legislatura 57', 'UF SP', 'partido PFIC', 'em exercício'],
    ],
    ['holds_mandate', { status: 'licenciado', party: '' }, ['licenciado', 'sem partido']],
    ['holds_mandate', { status: 'suplente_exercicio' }, ['suplente em exercício']],
    ['holds_mandate', { status: 'outro' }, ['situação outro']],
    [
      'candidate_for',
      { year: 2022, office: 'DEPUTADO FEDERAL', office_code: '6', uf: 'RJ', city: '60011', city_name: 'NITEROI', result: 'ELEITO', ballot_name: 'FULANO', role: '2022:6:RJ:60011' },
      ['eleição 2022', 'cargo DEPUTADO FEDERAL', 'UF RJ', 'município NITEROI', 'resultado ELEITO', 'nome na urna FULANO'],
    ],
    ['candidate_for', { city: '60011', office_code: '6' }, ['cargo (código 6)', 'município (código 60011)']],
    ['candidate_for', { office: 'PREFEITO', office_code: '11', city_name: 'NITEROI' }, ['cargo PREFEITO', 'município NITEROI']],
    ['donated_to', { origin_code: 'RPF' }, ['origem (código RPF)']],
    ['donated_to', { origin: 'RECURSOS PROPRIOS', origin_code: 'RP' }, ['origem RECURSOS PROPRIOS']],
    [
      'donated_to',
      { year: 2022, amount: '12345678901234.56', date: '2022-09-01', origin: 'RECURSOS DE PESSOAS FISICAS', origin_code: 'RPF', receipt_id: 'R1', items: 2, role: 'R1' },
      ['eleição 2022', 'R$ 12.345.678.901.234,56', 'em 01/09/2022', 'origem RECURSOS DE PESSOAS FISICAS', 'recibo R1', '2 itens'],
    ],
    ['donated_to', { items: 1 }, ['1 item']],
  ])('%s %j', (type, attrs, want) => {
    expect(attrLabels(type, attrs)).toEqual(want);
  });

  test('chave desconhecida, nula ou não escalar é ignorada', () => {
    expect(attrLabels('partner_of', { role: '22', percent: '1', segredo: 'x', outra: null, ok: true })).toEqual([
      'sócio, participação 1%',
      'qualificação 22',
    ]);
    // role is the Receita qualification only on Receita links; elsewhere an internal key
    expect(attrLabels('novo_tipo', { role: '1', qualquer: 'x' })).toEqual([]);
  });

  test('percentual nunca passa por número de ponto flutuante', () => {
    expect(attrLabels('shareholder_of', { percent: '0.30000000000000004' })).toEqual([
      'acionista, participação 0,30000000000000004%',
    ]);
  });
});

describe('participationLabel', () => {
  test('só para vínculos societários', () => {
    expect(participationLabel('partner_of', {})).toBe('sócio, participação não informada');
    expect(participationLabel('donated_to', {})).toBeNull();
    expect(participationLabel('partner_of', { percent: '' })).toBe('sócio, participação não informada');
  });
});
