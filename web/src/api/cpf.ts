// CPFs typed in a search. The API answers a query carrying a CPF by the
// 6 middle digits (docs/search.md, "Busca por CPF"); the web app keeps such a
// query in memory only: never in the URL, the history or storage.

const NUMERIC_SEPARATORS = /[.\-/\s]/g;
/** A full CPF spelling: a run of 11 digits, or ddd S ddd S ddd S dd. */
const FULL = /(?<!\d)\d{3}[.\-/ ]?\d{3}[.\-/ ]?\d{3}[.\-/ ]?\d{2}(?!\d)/;
/** The masked form the sources publish: ***NNNNNN**, ***.NNN.NNN-**. */
const MASKED = /\*{3}[.\- ]?\d{3}[.\- ]?\d{3}[.\- ]?\*{2}/;

/**
 * Whether text may carry a CPF. Deliberately broader than the API's rule
 * (it also catches a CPF glued to a word): what it flags stays out of the
 * URL. A bare 6-digit number or a CNPJ (8 or 14 digits) is not flagged.
 */
export function mayCarryCPF(raw: string): boolean {
  // Fullwidth and other compatibility digits, asterisks and punctuation
  // count as their ASCII forms (the API folds them the same way).
  const text = raw.normalize('NFKC');
  const digits = text.replace(NUMERIC_SEPARATORS, '');
  if (/^\d{11}$/.test(digits)) return true;
  return FULL.test(text) || MASKED.test(text);
}

/**
 * A route (path, query, hash) with a CPF-bearing ?q= taken out, for anything
 * that keeps a route: the login's return address (stored in sessionStorage
 * during the redirect) never holds a CPF.
 */
export function routeWithoutCPF(pathname: string, search: string, hash: string): string {
  const params = new URLSearchParams(search);
  const q = params.get('q');
  if (q !== null && mayCarryCPF(q)) params.delete('q');
  const rest = params.toString();
  return pathname + (rest ? `?${rest}` : '') + hash;
}
