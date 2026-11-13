import type { Edge, EntityResponse, Provenance } from '../api/types';

export function edge(overrides: Partial<Edge> = {}): Edge {
  return {
    relationship_id: 501,
    type: 'partner_of',
    src: { id: 11, kind: 'person', name: 'PESSOA FICTICIA UM', natural_key: '' },
    dst: { id: 12, kind: 'company', name: 'EMPRESA FICTICIA A LTDA', natural_key: '90000002' },
    attrs: { role: '22' },
    kind: 'fact',
    confidence: null,
    valid_from: '2020-01-01',
    valid_to_after: null,
    valid_to_before: null,
    source_key: 'receita',
    collection_id: 7,
    stale: false,
    closed: false,
    identity_basis: 'none',
    ...overrides,
  };
}

export function companyEntity(overrides: Partial<EntityResponse> = {}): EntityResponse {
  return {
    entity: { id: 12, kind: 'company', name: 'EMPRESA FICTICIA A LTDA', natural_key: '90000002' },
    versions: [],
    records: [],
    records_truncated: false,
    possible_same: [],
    ...overrides,
  };
}

export function personEntity(overrides: Partial<EntityResponse> = {}): EntityResponse {
  return {
    entity: { id: 11, kind: 'person', name: 'PESSOA FICTICIA UM', natural_key: '' },
    versions: [],
    records: [],
    records_truncated: false,
    possible_same: [],
    ...overrides,
  };
}

export function provenance(overrides: Partial<Provenance> = {}): Provenance {
  return {
    relationship_id: 501,
    kind: 'fact',
    confidence: null,
    record_ref: 'K3241.K03200Y0.D61010.SOCIOCSV:42',
    recorded_at: '2026-11-01T10:00:00Z',
    last_seen_collection_id: 7,
    closed_collection_id: null,
    source: { key: 'receita', name: 'Receita Federal — CNPJ' },
    collection: {
      id: 7,
      reference_date: '2026-10-01',
      observed_date: '2026-10-13',
      collected_at: '2026-11-01T10:30:00Z',
      status: 'done',
      full_scope: true,
      stale: false,
    },
    raw_key: 'raw/receita/2026-10-01/Socios0.zip',
    raw_sha256: '9f2c0d1e5b7a4c3d2e1f0a9b8c7d6e5f4a3b2c1d0e9f8a7b6c5d4e3f2a1b0c9d',
    raw_size: 251000000,
    last_seen_collection: {
      id: 9,
      reference_date: '2026-11-01',
      observed_date: '2026-11-10',
      collected_at: '2026-11-12T08:00:00Z',
    },
    source_url: 'https://arquivos.receitafederal.gov.br/public.php/webdav/Dados/Cadastros/CNPJ/2026-10/Socios0.zip',
    source_note: 'Arquivo do compartilhamento público da Receita Federal (Nextcloud): abra o link e entre com o código público.',
    ...overrides,
  };
}
