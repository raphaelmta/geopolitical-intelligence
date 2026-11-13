// Shapes of the API's JSON (docs/api.md), snake_case as sent.

export type EntityKind = 'person' | 'company' | 'party' | 'public_body' | 'foreign';

export type RelType =
  | 'partner_of'
  | 'administrator_of'
  | 'legal_rep_of'
  | 'donated_to'
  | 'candidate_for'
  | 'holds_mandate'
  | 'controls'
  | 'shareholder_of';

/** Date without time, "YYYY-MM-DD". */
export type DateOnly = string;
/** Instant, RFC 3339 in UTC. */
export type Timestamp = string;

export interface EntityRef {
  id: number;
  /** One of EntityKind; kept open so a new kind still renders. */
  kind: EntityKind | (string & {});
  /** Already CPF-masked by the API. */
  name: string;
  natural_key: string;
}

/**
 * none: no person record, or each person involved has a single record;
 * auto: some person joins several records merged automatically (inference);
 * analyst: some person joins several records and an analyst decision placed
 * at least one; unknown: legacy job result citing person records.
 */
export type IdentityBasis = 'none' | 'auto' | 'analyst' | 'unknown';

export interface Edge {
  relationship_id: number;
  type: RelType | (string & {});
  src: EntityRef;
  dst: EntityRef;
  /**
   * Simple values only; which keys depends on the type (api/attrs.ts knows
   * them). Decimals (amount, percent*) come as strings.
   */
  attrs: Record<string, string | number | boolean | null>;
  kind: 'fact' | 'inference';
  /** null for facts; 0..1 for inferences. */
  confidence: number | null;
  /**
   * Present when the edge cites person records: the lowest confidence that
   * those records are the entity's person (automatic merge: always < 1;
   * analyst merge: 1). Read it together with identity_basis.
   */
  identity_confidence?: number | null;
  /**
   * What the identity of the person ends rests on (docs/api.md "Fato e
   * inferência"). Always sent by the API; "unknown" only in job results
   * stored before the field existed. Optional so an older payload still
   * renders (api/edges.ts identityBasisOf decides then).
   */
  identity_basis?: IdentityBasis | (string & {});
  /**
   * The claim was closed. Always sent by the API; absent only in job results
   * stored by an older server (api/edges.ts isClosed falls back to the dates).
   */
  closed?: boolean;
  valid_from: DateOnly | null;
  valid_to_after: DateOnly | null;
  valid_to_before: DateOnly | null;
  source_key: string;
  collection_id: number;
  stale: boolean;
}

export interface CompanyVersion {
  id: number;
  name: string;
  legal_nature: string;
  size: string;
  uf: string;
  city: string;
  cnae: string;
  status: string;
  /** Decimal string. */
  capital: string | null;
  open: boolean;
  first_collection_id: number;
  last_seen_collection_id: number | null;
  closed_collection_id: number | null;
  /** Reference dates of the collections that first/last saw it, and closed it. */
  first_seen_reference_date?: DateOnly | null;
  last_seen_reference_date?: DateOnly | null;
  closed_reference_date?: DateOnly | null;
  record_ref: string;
  recorded_at: Timestamp;
}

export interface PersonRecord {
  id: number;
  entity_id: number;
  source_key: string;
  context_key: string;
  name: string;
  cpf_masked?: string;
  identity_confidence: number;
  locked: boolean;
  /** Where the record was first read: source file and line (CPF-masked). Absent on older servers. */
  record_ref?: string;
  /** The collection that first read it. Absent on older servers. */
  first_collection_id?: number;
  /** Reference date of that collection; null only if the collection does not exist. */
  first_seen_reference_date?: DateOnly | null;
  /**
   * Proposals list only: unlocked and linked to an entity — a decision on
   * the proposal moves it. Absent elsewhere (and on older servers).
   */
  free?: boolean;
}

export interface PossibleSame {
  entity_id: number;
  name: string;
  records: number;
  shared_fragment: boolean;
}

export interface EntityResponse {
  entity: EntityRef;
  versions: CompanyVersion[];
  records: PersonRecord[];
  records_truncated: boolean;
  possible_same: PossibleSame[];
}

export interface SearchItem extends EntityRef {
  /**
   * How the hit was found, when not by name or CNPJ: "cpf_fragment" is a
   * person whose CPF fragment (the 6 middle digits) matches the CPF typed —
   * a candidate, never a confirmation.
   */
  match?: 'cpf_fragment' | (string & {});
}

export interface SearchResponse {
  items: SearchItem[];
  /** Present (true) when the query carried a CPF: every hit is a candidate. */
  cpf_fragment_search?: boolean;
}

export interface RelationshipsPage {
  items: Edge[];
  /** "" on the last page. */
  next_cursor: string;
}

export interface Provenance {
  relationship_id: number;
  kind: 'fact' | 'inference';
  confidence: number | null;
  record_ref: string;
  recorded_at: Timestamp;
  last_seen_collection_id: number | null;
  closed_collection_id: number | null;
  source: { key: string; name: string };
  collection: {
    id: number;
    reference_date: DateOnly;
    /** Until when the source's state was observed (Receita: the edition's publication). Absent on older servers. */
    observed_date?: DateOnly;
    collected_at: Timestamp | null;
    status: string;
    full_scope: boolean;
    stale: boolean;
  };
  /** Absent for collections loaded before raw files were recorded. */
  raw_key?: string;
  raw_sha256?: string;
  raw_size?: number;
  /** The collection that saw the relationship last; null when none is recorded (absent on older servers). */
  last_seen_collection?: CollectionDates | null;
  /** Where the file was downloaded from (no credentials); null when not recorded. */
  source_url?: string | null;
  /** Receita only, with a URL: how to reach the file in a browser (fixed text). */
  source_note?: string;
}

export interface CollectionDates {
  id: number;
  reference_date: DateOnly;
  observed_date: DateOnly;
  collected_at: Timestamp | null;
}

export interface Job {
  id: string;
  type: 'paths' | 'control_group' | (string & {});
  params: Record<string, unknown>;
  status: 'queued' | 'running' | 'done' | 'failed';
  progress: number;
  result: unknown;
  error: string;
  created_at: Timestamp;
  finished_at: Timestamp | null;
}

export interface SourceStatus {
  key: string;
  name: string;
  collectable: boolean;
  /**
   * Published as dated editions (Receita): the reference date names the
   * edition and staleness counts from last_observed_date. Absent on older
   * servers (treated as false).
   */
  editions?: boolean;
  status: 'fresh' | 'stale' | 'never' | 'failed';
  last_collected_at: Timestamp | null;
  next_expected_at: Timestamp | null;
  last_reference_date: DateOnly | null;
  /**
   * Of the last done full collection: for an edition source (Receita) the
   * edition's publication date (staleness counts from it), otherwise the day
   * the collection started. null when never; absent on older servers.
   */
  last_observed_date?: DateOnly | null;
  last_checked_at: Timestamp | null;
  /** Only for admins (absent for analysts); masked, at most 300 characters. */
  last_error?: string;
  last_stats: Record<string, number> | null;
  /** A collection request still waiting to start (absent on older servers). */
  pending_request_at?: Timestamp | null;
}

export type ReasonCode = 'common_name' | 'impure_target' | 'ambiguous_key' | 'other';

export type ProposalStatus = 'pending' | 'confirmed' | 'rejected' | 'obsolete';

export interface ProposalsPage {
  items: Proposal[];
  /** "" on the last page. */
  next_cursor: string;
}

export interface Proposal {
  id: number;
  status: ProposalStatus;
  name: string;
  records: PersonRecord[];
  connections: number;
  /** Free text (English, may change); never parsed: use reason_code. */
  reason: string;
  /** Stable form of reason (absent on older servers → treated as "other"). */
  reason_code?: ReasonCode | (string & {});
  /** The entity a confirmation would merge the records into, with its other records. */
  target_entity: {
    /** null when no record of the proposal is linked to an entity any more. */
    entity: EntityRef | null;
    other_records: { id: number; source_key: string; context_key: string }[];
    other_records_truncated: boolean;
  };
}

/** Params of a "paths" job (internal/jobs/graphjobs.go). */
export interface PathsParams {
  a: number;
  b: number;
  /** 1..6; 0 means the default (6). */
  max_depth: number;
  rel_types?: string[];
  /** YYYY-MM-DD */
  at?: string;
}

export type ControlMode = 'documented' | 'corporate_links';

export interface ControlGroupParams {
  company_id: number;
  mode: ControlMode;
}

export type JobRequest = { type: 'paths'; params: PathsParams } | { type: 'control_group'; params: ControlGroupParams };

/** One step of a path. forward: walked from src to dst. */
export interface Hop {
  edge: Edge;
  forward: boolean;
}

export interface HubStop {
  entity: EntityRef;
  degree: number;
}

/** Result of a "paths" job; lists may come as null. */
export interface PathsResult {
  paths: Hop[][] | null;
  stopped_hubs: HubStop[] | null;
  truncated: boolean;
}

export interface Member {
  entity: EntityRef;
  /** Control hops from the nearest final controller. */
  depth: number;
  /** Relationship ids of that chain, from the controller down. */
  via: number[] | null;
}

export interface ControlStop {
  entity: EntityRef;
  /** fund | foreign | too_many_partners | too_large (kept open). */
  reason: string;
}

/** Result of a "control_group" job; lists may come as null. */
export interface ControlResult {
  root: EntityRef;
  controllers: EntityRef[] | null;
  members: Member[] | null;
  edges: Edge[] | null;
  stops: ControlStop[] | null;
}
