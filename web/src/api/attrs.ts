// The one place that turns an edge's attrs into Portuguese text. Known keys
// get a label; any other key is ignored. Decimals (amount, percent*) arrive
// as strings and are formatted as text, never through a float.
import type { Edge } from './types';
import { formatCNPJBase, formatDate, formatMoney, formatPercent } from './format';

type Attrs = Edge['attrs'];
type Scalar = string | number;

function scalar(v: unknown): Scalar | null {
  if (typeof v === 'string') return v.trim() === '' ? null : v;
  if (typeof v === 'number' && Number.isFinite(v)) return v;
  return null;
}

/** "51.50" → "51,5%", digit by digit; a number (old data) via Intl. */
export function formatDecimalPercent(value: Scalar): string {
  if (typeof value === 'number') return formatPercent(value);
  const m = /^(-)?(\d+)(?:\.(\d+))?$/.exec(value.trim());
  if (!m) return value;
  const int = m[2]!.replace(/^0+(?=\d)/, '').replace(/\B(?=(\d{3})+(?!\d))/g, '.');
  const frac = (m[3] ?? '').replace(/0+$/, '');
  return `${m[1] ? '-' : ''}${int}${frac ? `,${frac}` : ''}%`;
}

const SHARE_NOUN: Record<string, string> = {
  partner_of: 'sócio',
  shareholder_of: 'acionista',
  controls: 'controle declarado',
};

/** Types whose role is the Receita qualification code; elsewhere role is an internal key. */
const QUALIFIED = new Set(['partner_of', 'administrator_of', 'legal_rep_of']);

/**
 * Who holds what in a corporate link: "sócio, participação 12,5%", or
 * "…, participação não informada" when the source gives no percentage
 * (Receita never does for partners). null for other relationship types.
 */
export function participationLabel(type: string, attrs: Attrs): string | null {
  const noun = SHARE_NOUN[type];
  if (!noun) return null;
  const p = scalar(attrs.percent);
  // controls carries no percentage: the control itself is what was declared
  if (p == null) return type === 'controls' ? noun : `${noun}, participação não informada`;
  return `${noun}, participação ${formatDecimalPercent(p)}`;
}

const MANDATE_STATUS: Record<string, string> = {
  exercicio: 'em exercício',
  licenciado: 'licenciado',
  suplente_exercicio: 'suplente em exercício',
};

const s = (v: Scalar) => String(v);

/** Display order and label of each known key (percent is handled above). */
const KEYS: [string, (v: Scalar, attrs: Attrs) => string | null][] = [
  ['role', () => null], // handled below: only Receita roles are shown
  ['year', (v) => `eleição ${s(v)}`],
  ['legislature', (v) => `legislatura ${s(v)}`],
  ['office', (v) => `cargo ${s(v)}`],
  ['office_code', (v, a) => (scalar(a.office) == null ? `cargo (código ${s(v)})` : null)],
  ['uf', (v) => `UF ${s(v)}`],
  // names when there are; codes only when the name is missing
  ['city', (v, a) => `município ${scalar(a.city_name) != null ? s(scalar(a.city_name)!) : `(código ${s(v)})`}`],
  ['city_name', (v, a) => (scalar(a.city) == null ? `município ${s(v)}` : null)],
  ['party', (v) => `partido ${s(v)}`],
  ['status', (v) => MANDATE_STATUS[s(v)] ?? `situação ${s(v)}`],
  ['result', (v) => `resultado ${s(v)}`],
  ['ballot_name', (v) => `nome na urna ${s(v)}`],
  ['amount', (v) => (typeof v === 'number' ? formatMoney(String(v)) : formatMoney(v))],
  ['date', (v) => `em ${formatDate(s(v))}`],
  ['origin', (v) => `origem ${s(v)}`],
  ['origin_code', (v, a) => (scalar(a.origin) == null ? `origem (código ${s(v)})` : null)],
  ['receipt_id', (v) => `recibo ${s(v)}`],
  ['items', (v) => (s(v) === '1' ? '1 item' : `${s(v)} itens`)],
  ['percent_on', (v) => `ON ${formatDecimalPercent(v)}`],
  ['percent_pn', (v) => `PN ${formatDecimalPercent(v)}`],
  ['reference_date', (v) => `referência ${formatDate(s(v))}`],
  ['form_version', (v) => `formulário versão ${s(v)}`],
  ['described_by', (v) => `participação indireta, descrita no formulário da companhia ${formatCNPJBase(s(v))}`],
];

/** Attribute texts for an edge of this type, in a stable order. */
export function attrLabels(type: string, attrs: Attrs | null | undefined): string[] {
  const a: Attrs = attrs ?? {};
  const out: string[] = [];
  const share = participationLabel(type, a);
  if (share) out.push(share);
  else {
    const p = scalar(a.percent);
    if (p != null) out.push(`participação ${formatDecimalPercent(p)}`);
  }
  const role = scalar(a.role);
  if (role != null && QUALIFIED.has(type)) out.push(`qualificação ${s(role)}`);
  for (const [key, label] of KEYS) {
    const v = scalar(a[key]);
    if (v == null) continue;
    const text = label(v, a);
    if (text) out.push(text);
  }
  // an empty party on a mandate means the holder had none
  if (type === 'holds_mandate' && a.party === '') out.push('sem partido');
  return out;
}
