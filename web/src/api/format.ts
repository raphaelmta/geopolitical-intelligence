// Formatting for display, pt-BR. Dates without time are handled as text so
// no time zone can shift the day; money is formatted from the decimal string
// so no precision is lost.
import type { Edge, RelType } from './types';

const DATE_ONLY = /^(\d{4})-(\d{2})-(\d{2})$/;

/** "2026-10-01" → "01/10/2026". */
export function formatDate(value: string | null | undefined): string {
  if (!value) return '';
  const m = DATE_ONLY.exec(value);
  if (!m) return value;
  return `${m[3]}/${m[2]}/${m[1]}`;
}

const timestampFormat = () =>
  new Intl.DateTimeFormat('pt-BR', {
    day: '2-digit',
    month: '2-digit',
    year: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    hourCycle: 'h23',
  });

/** RFC 3339 instant → "DD/MM/AAAA HH:MM" in the browser's local time. */
export function formatTimestamp(value: string | null | undefined): string {
  if (!value) return '';
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return value;
  const parts = Object.fromEntries(timestampFormat().formatToParts(d).map((p) => [p.type, p.value]));
  return `${parts.day}/${parts.month}/${parts.year} ${parts.hour}:${parts.minute}`;
}

/** CNPJ base (8 characters, numeric or alphanumeric) → "12.345.678". */
export function formatCNPJBase(key: string): string {
  if (!/^[0-9A-Z]{8}$/.test(key)) return key;
  return `${key.slice(0, 2)}.${key.slice(2, 5)}.${key.slice(5)}`;
}

function groupThousands(digits: string): string {
  return digits.replace(/\B(?=(\d{3})+(?!\d))/g, '.');
}

/** Decimal string "1234.5" → "R$ 1.234,50", without floating point. */
export function formatMoney(value: string | null | undefined): string {
  if (value == null) return '';
  const m = /^(-)?(\d+)(?:\.(\d+))?$/.exec(value.trim());
  if (!m) return value;
  const int = groupThousands(m[2]!.replace(/^0+(?=\d)/, ''));
  const frac = (m[3] ?? '').padEnd(2, '0');
  return `${m[1] ? '-' : ''}R$ ${int},${frac}`;
}

const decimal = new Intl.NumberFormat('pt-BR', { maximumFractionDigits: 2 });

/** 12.5 or "12.5" (already in percent) → "12,5%". */
export function formatPercent(value: number | string): string {
  const n = typeof value === 'number' ? value : Number(value);
  if (!Number.isFinite(n)) return String(value);
  return `${decimal.format(n)}%`;
}

/**
 * A confidence in [0, 1] as a whole percentage, never overstating it: only
 * exactly 1 is "100%" (0.995 is "99%", not rounded up), anything above 0 and
 * below 0.005 is "< 1%". null when the value is missing or not in [0, 1].
 */
export function formatConfidence(value: unknown): string | null {
  if (typeof value !== 'number' || !Number.isFinite(value) || value < 0 || value > 1) return null;
  if (value === 1) return '100%';
  if (value > 0 && value < 0.005) return '< 1%';
  return `${Math.min(99, Math.round(value * 100))}%`;
}

export const CONFIDENCE_MISSING = 'confiança não informada';

export function formatBytes(n: number): string {
  const units = ['B', 'kB', 'MB', 'GB', 'TB'];
  let v = n;
  let i = 0;
  while (v >= 1000 && i < units.length - 1) {
    v /= 1000;
    i++;
  }
  return `${new Intl.NumberFormat('pt-BR', { maximumFractionDigits: 1 }).format(v)} ${units[i]}`;
}

/**
 * Display names of the sources (internal/sources All, the same names
 * GET /sources sends), for screens that have only a source key. A key not
 * listed is shown as is.
 */
export const SOURCE_NAMES: Record<string, string> = {
  receita: 'Receita Federal — CNPJ',
  tse: 'TSE — candidaturas e contas',
  camara: 'Câmara dos Deputados',
  senado: 'Senado Federal',
  cvm: 'CVM — companhias abertas',
};

export function sourceName(key: string): string {
  return Object.hasOwn(SOURCE_NAMES, key) ? SOURCE_NAMES[key]! : key;
}

export const KIND_LABELS: Record<string, string> = {
  person: 'Pessoa',
  company: 'Empresa',
  party: 'Partido',
  public_body: 'Órgão público',
  foreign: 'Estrangeiro',
};

export function kindLabel(kind: string): string {
  return KIND_LABELS[kind] ?? kind;
}

/** Natural key as shown next to an entity ("" when there is none to show). */
export function naturalKeyLabel(kind: string, key: string): string {
  if (!key) return '';
  return kind === 'company' ? formatCNPJBase(key) : key;
}

/** Direction of an edge seen from one entity: out = it is the source. */
export type Direction = 'out' | 'in';

const RELATION_LABELS: Record<RelType, { out: string; in: string; name: string }> = {
  partner_of: { out: 'é sócio de', in: 'tem como sócio', name: 'Sociedade' },
  administrator_of: { out: 'é administrador de', in: 'tem como administrador', name: 'Administração' },
  legal_rep_of: { out: 'é representante legal de', in: 'tem como representante legal', name: 'Representação legal' },
  shareholder_of: { out: 'é acionista de', in: 'tem como acionista', name: 'Participação acionária' },
  controls: { out: 'controla', in: 'é controlada por', name: 'Controle' },
  candidate_for: { out: 'foi candidato por', in: 'teve como candidato', name: 'Candidatura' },
  donated_to: { out: 'doou para', in: 'recebeu doação de', name: 'Doação eleitoral' },
  holds_mandate: { out: 'exerce mandato em', in: 'tem como mandatário', name: 'Mandato' },
};

export const REL_TYPES = Object.keys(RELATION_LABELS) as RelType[];

export function relationLabel(type: string, dir: Direction): string {
  const l = RELATION_LABELS[type as RelType];
  return l ? l[dir] : type;
}

export function relationTypeName(type: string): string {
  return RELATION_LABELS[type as RelType]?.name ?? type;
}

/** When a closed edge ended, from its bounds. */
export function closedLabel(e: Pick<Edge, 'valid_to_after' | 'valid_to_before'>): string {
  const after = formatDate(e.valid_to_after);
  const before = formatDate(e.valid_to_before);
  if (after && before) return after === before ? `encerrado em ${after}` : `saiu entre ${after} e ${before}`;
  if (before) return `saiu antes de ${before}`;
  if (after) return `saiu depois de ${after}`;
  return 'encerrado (data desconhecida)';
}

/** Letters and digits in a query (what the API counts as useful). */
export function usefulLength(q: string): number {
  return (q.match(/[\p{L}\p{N}]/gu) ?? []).length;
}

/** An instant (ms or RFC 3339) → "HH:MM:SS" in the browser's local time. */
export function formatClock(value: number | string): string {
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return String(value);
  const parts = Object.fromEntries(
    new Intl.DateTimeFormat('pt-BR', { hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23' })
      .formatToParts(d)
      .map((p) => [p.type, p.value]),
  );
  return `${parts.hour}:${parts.minute}:${parts.second}`;
}

/** Today's date in the browser's local time, "YYYY-MM-DD". */
export function localToday(now: Date = new Date()): string {
  const p = (n: number) => String(n).padStart(2, '0');
  return `${now.getFullYear()}-${p(now.getMonth() + 1)}-${p(now.getDate())}`;
}
