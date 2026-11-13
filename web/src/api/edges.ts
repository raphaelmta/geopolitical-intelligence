// What an edge's flags mean, in one place: whether it is closed, and what the
// identity of its person ends rests on (docs/api.md "Fato e inferência").
// EdgeBadge, the graph labels and the path summaries all read these.
import type { Edge, IdentityBasis } from './types';
import { CONFIDENCE_MISSING, formatConfidence } from './format';

type IdentityFields = Pick<Edge, 'identity_basis' | 'identity_confidence'>;

const BASES: readonly string[] = ['none', 'auto', 'analyst', 'unknown'] satisfies IdentityBasis[];

/**
 * The identity basis the API sent. Without the field (an older payload) an
 * edge that cites no person record is "none" and one that does is
 * "unknown": never shown as certain. A value this screen does not know is
 * "unknown" too.
 */
export function identityBasisOf(e: IdentityFields): IdentityBasis {
  const b = e.identity_basis;
  if (b == null) return e.identity_confidence === undefined ? 'none' : 'unknown';
  return BASES.includes(b) ? (b as IdentityBasis) : 'unknown';
}

/** The edge depends on merged person records (or may: unknown). */
export function identityMerged(e: IdentityFields): boolean {
  return identityBasisOf(e) !== 'none';
}

export const IDENTITY_AUTO_HELP =
  'Identidade inferida: pessoas unidas automaticamente por nome e trecho do CPF. A porcentagem é a confiança dessa união.';
export const IDENTITY_ANALYST_HELP =
  'Registros de pessoa unidos por decisão de um analista: um julgamento humano, não um fato documentado pela fonte.';
export const IDENTITY_UNKNOWN_HELP =
  'Resultado gravado antes de a plataforma registrar em que se apoia a identidade: pode depender de registros de pessoa unidos.';

/**
 * The identity tag of an edge (text and help), or null when its identity
 * rests on nothing merged. An automatic merge shows its confidence through
 * formatConfidence, and never "100%": a value of 1 (or invalid) for an
 * automatic merge is shown as missing.
 */
export function identityTag(e: IdentityFields): { text: string; title: string } | null {
  switch (identityBasisOf(e)) {
    case 'none':
      return null;
    case 'auto': {
      const c = e.identity_confidence;
      const pct = typeof c === 'number' && c < 1 ? formatConfidence(c) : null;
      return { text: pct ? `identidade ${pct}` : `identidade · ${CONFIDENCE_MISSING}`, title: IDENTITY_AUTO_HELP };
    }
    case 'analyst':
      return { text: 'identidade: decisão de analista', title: IDENTITY_ANALYST_HELP };
    default:
      return { text: 'identidade: base não informada', title: IDENTITY_UNKNOWN_HELP };
  }
}

/**
 * Whether the claim is closed: the API's `closed` flag. Only a job result
 * stored by an older server lacks it; then, and only then, the end dates
 * decide (what the screens did before the flag existed).
 */
export function isClosed(e: Pick<Edge, 'closed' | 'valid_to_after' | 'valid_to_before'>): boolean {
  if (typeof e.closed === 'boolean') return e.closed;
  return e.valid_to_after != null || e.valid_to_before != null;
}

/** A path (or chain) with an inferred link or a link resting on merged identities. */
export function pathHasDoubt(edges: Pick<Edge, 'kind' | 'identity_basis' | 'identity_confidence'>[]): boolean {
  return edges.some((e) => e.kind === 'inference' || identityMerged(e));
}
