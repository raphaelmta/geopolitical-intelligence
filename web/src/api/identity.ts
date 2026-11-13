// Identity decisions: how a person record's context, a proposal's reason and
// a decision's reason are shown and checked (internal/identity, internal/api/identity.go).
import { useQuery } from '@tanstack/react-query';
import { apiGet } from './client';
import { formatCNPJBase, sourceName } from './format';
import type { SourceStatus } from './types';

/** Record ids of one merge or split (internal/api/identity.go maxRecordIDs). */
export const MAX_RECORD_IDS = 100;
/** Characters of a decision's reason (maxReasonRunes). */
export const REASON_MAX = 500;

/** Shown when a guarded write (409) finds the data changed under the screen. */
export const MSG_CHANGED = 'Os dados mudaram desde que esta tela foi carregada. Nada foi alterado. Confira de novo.';

/** The honesty line shown wherever two records look alike. */
export const NOT_PROOF =
  'A coincidência de nome e do trecho publicado do CPF não é prova de identidade: o trecho tem 6 dos 11 dígitos, e pessoas diferentes com o mesmo nome podem tê-lo.';

/**
 * Where a record comes from, in words: "empresa 12.345.678" (Receita, CVM),
 * "candidatura 2022", "doação 2022", "deputado", "senador"; anything else as
 * the raw key.
 */
export function contextLabel(context: string): string {
  if (!context) return '—';
  if (/^[0-9A-Z]{8}$/.test(context)) return `empresa ${formatCNPJBase(context)}`;
  let m = /^cvm:([0-9A-Z]{8})$/.exec(context);
  if (m) return `empresa ${formatCNPJBase(m[1]!)}`;
  m = /^(cand|donor):(\d{4}):/.exec(context);
  if (m) return `${m[1] === 'cand' ? 'candidatura' : 'doação'} ${m[2]}`;
  if (/^dep:./.test(context)) return 'deputado';
  if (/^sen:./.test(context)) return 'senador';
  return context;
}

/** The masked CPF as the API sends it, or that the source published none. */
export function cpfLabel(masked: string | undefined): string {
  return masked ? masked : 'CPF não publicado';
}

/** Shown for reason_code "other", a missing code (older server) or one this screen does not know. */
export const REASON_OTHER =
  'A plataforma não teve segurança para unir estas fichas sozinha e pede a decisão de um analista.';

/**
 * Why a group was proposed instead of merged, from the proposal's stable
 * reason_code (internal/api/identity.go reasonCode). The free-text reason is
 * never parsed.
 */
export function proposalReason(code: string | undefined): string {
  switch (code) {
    case 'common_name':
      return 'Nome comum: há mais trechos do CPF diferentes com este nome do que o limite até o qual a plataforma une fichas sozinha. Por isso a decisão é de um analista.';
    case 'ambiguous_key':
      return 'Um analista já separou fichas com este nome e este trecho do CPF. Desde então, a plataforma não une sozinha nada com esta combinação: propõe aqui.';
    case 'impure_target':
      return 'As fichas seriam unidas a uma entidade montada por um analista, e a plataforma nunca amplia sozinha uma entidade montada à mão.';
    default:
      return REASON_OTHER;
  }
}

/** Characters (not UTF-16 units) of a trimmed reason, as the API counts them. */
export function reasonLength(reason: string): number {
  return [...reason.trim()].length;
}

export function validReason(reason: string): boolean {
  const n = reasonLength(reason);
  return n >= 1 && n <= REASON_MAX;
}

/**
 * Source names by key, from /sources (the source list the status screen
 * uses). Until it loads, or if it fails, the static names of format.ts
 * (and an unknown key as is).
 */
export function useSourceNames(enabled = true): (key: string) => string {
  const q = useQuery({
    queryKey: ['sources'],
    queryFn: () => apiGet<SourceStatus[]>('/sources'),
    enabled,
    staleTime: 5 * 60_000,
  });
  const names = new Map((q.data ?? []).map((s) => [s.key, s.name]));
  return (key) => names.get(key) || sourceName(key);
}
