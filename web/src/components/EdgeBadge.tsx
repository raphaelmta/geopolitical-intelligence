import type { Edge } from '../api/types';
import { CONFIDENCE_MISSING, formatConfidence } from '../api/format';
import { identityTag } from '../api/edges';
import { StaleTag } from './StaleTag';

type BadgeEdge = Pick<Edge, 'kind' | 'confidence' | 'stale'> & Partial<Pick<Edge, 'identity_basis' | 'identity_confidence'>>;

/**
 * Fact or inference, never by color alone: a text label plus a line sample
 * (solid for fact, dashed for inference). Adds what the identity of the
 * person ends rests on whenever it is not a single record ("identidade NN%"
 * for an automatic merge, "decisão de analista", "base não informada"), and
 * "vencido" for stale sources.
 */
export function EdgeBadge({ edge }: { edge: BadgeEdge }) {
  const inference = edge.kind === 'inference';
  const confidence = formatConfidence(edge.confidence);
  const identity = identityTag(edge);
  return (
    <span className="edge-badge">
      <span className={`tag ${inference ? 'tag--inference' : 'tag--fact'}`}>
        <span className={`edge-line ${inference ? 'edge-line--dashed' : 'edge-line--solid'}`} aria-hidden="true" />
        <span>{inference ? `inferência · ${confidence ?? CONFIDENCE_MISSING}` : 'fato'}</span>
      </span>
      {identity && (
        <span className="tag tag--identity" title={identity.title}>
          {identity.text}
        </span>
      )}
      {edge.stale && <StaleTag />}
    </span>
  );
}
