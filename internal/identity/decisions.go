package identity

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Errors of the analyst operations.
var (
	ErrNoUser             = errors.New("identity: the deciding user is required")
	ErrTooFewRecords      = errors.New("identity: not enough distinct records")
	ErrRecordNotFound     = errors.New("identity: person record not found")
	ErrProposalNotFound   = errors.New("identity: proposal not found")
	ErrProposalNotPending = errors.New("identity: proposal is not pending")
	// ErrProposalStale: fewer than two of the proposal's records are still
	// free to merge (the others were locked by a later decision). The
	// proposal stays pending; the next Resolve refreshes or closes it.
	ErrProposalStale = errors.New("identity: fewer than two proposal records are still unlocked")
	// ErrExpectationFailed: the records are no longer as the caller saw them
	// (DecisionExpectation, MergeExpecting, SplitExpecting). Nothing was done.
	ErrExpectationFailed = errors.New("identity: the records changed since the caller saw them")
)

// DecisionExpectation is what the caller of DecideExpecting saw when it took
// its decision. Every field is checked inside the decision's transaction,
// after the identity lock, against what the decision is about to do; any
// difference refuses the decision with ErrExpectationFailed. The zero value
// checks nothing.
type DecisionExpectation struct {
	// CheckTarget: a confirmation must merge into TargetEntityID (0: the
	// caller saw no target, so no confirmation matches). Ignored by a reject,
	// which merges into nothing.
	CheckTarget    bool
	TargetEntityID int64
	// RecordIDs, when not nil, must be exactly (as a set) the proposal's
	// records the decision acts on: those still free (linked to an entity and
	// not locked by an analyst decision).
	RecordIDs []int64
}

// Merge puts the records in the entity of the lowest-id one, as an analyst
// decision: the records are locked (Resolve never moves them again), get
// confidence 1 and their relationships are re-pointed. It returns the entity.
func (r *Resolver) Merge(ctx context.Context, recordIDs []int64, user, reason string) (int64, error) {
	return r.MergeExpecting(ctx, recordIDs, nil, user, reason)
}

// MergeExpecting is Merge that, when entityIDs is not nil, first checks (in
// the merge's transaction, after the identity lock) that the records are
// exactly all the person records of those entities: each record belongs to
// one of them, each of them has at least one record, and none of their
// records is missing. Otherwise it returns ErrExpectationFailed and does nothing.
func (r *Resolver) MergeExpecting(ctx context.Context, recordIDs, entityIDs []int64, user, reason string) (int64, error) {
	ids, user, err := validate(recordIDs, 2, user)
	if err != nil {
		return 0, err
	}
	var entity int64
	err = retryTx(ctx, r.Pool, func(tx pgx.Tx) error {
		if err := lockRecords(ctx, tx, ids); err != nil {
			return err
		}
		if entityIDs != nil {
			if err := expectCover(ctx, tx, ids, sortedUnique(entityIDs)); err != nil {
				return err
			}
		}
		var err error
		entity, _, err = analystMerge(ctx, tx, ids, user, reason)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("identity: merge: %w", err)
	}
	return entity, nil
}

// Split moves the records to one new person entity (named after the
// lowest-id record), as an analyst decision: the records are locked, get
// confidence 1 and their relationships are re-pointed. It returns the new entity.
//
// A key (name and CPF fragment) that ends up on both sides of the split is
// shared by different people, so it is marked ambiguous: future records with
// it are proposed to an analyst, never merged automatically.
func (r *Resolver) Split(ctx context.Context, recordIDs []int64, user, reason string) (int64, error) {
	return r.SplitExpecting(ctx, recordIDs, 0, user, reason)
}

// SplitExpecting is Split that, when entityID is not 0, first checks (in the
// split's transaction, after the identity lock) that every record belongs to
// entityID and that the entity keeps at least one other record. Otherwise it
// returns ErrExpectationFailed and does nothing.
func (r *Resolver) SplitExpecting(ctx context.Context, recordIDs []int64, entityID int64, user, reason string) (int64, error) {
	ids, user, err := validate(recordIDs, 1, user)
	if err != nil {
		return 0, err
	}
	var entity int64
	err = retryTx(ctx, r.Pool, func(tx pgx.Tx) error {
		if err := lockRecords(ctx, tx, ids); err != nil {
			return err
		}
		if entityID != 0 {
			if err := expectSplit(ctx, tx, ids, entityID); err != nil {
				return err
			}
		}
		var err error
		if entity, err = newPersonEntity(ctx, tx, ids[0]); err != nil {
			return err
		}
		decision, err := insertDecision(ctx, tx, "split", ids, &entity, reason, user)
		if err != nil {
			return err
		}
		previous, err := place(ctx, tx, ids, entity, decision)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO identity_ambiguous_key (group_key, decision_id)
			SELECT DISTINCT s.name_norm || '|' || s.cpf_fragment, $3::bigint
			FROM person_record s
			WHERE s.id = ANY ($1) AND s.name_norm <> '' AND `+validFragment("s")+`
				AND EXISTS (SELECT 1 FROM person_record o
					WHERE o.entity_id = ANY ($2) AND o.name_norm = s.name_norm AND o.cpf_fragment = s.cpf_fragment)
			ON CONFLICT (group_key) DO NOTHING`, ids, previous, decision)
		if err != nil {
			return fmt.Errorf("mark ambiguous keys: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("identity: split: %w", err)
	}
	return entity, nil
}

// Decide confirms or rejects a pending proposal, acting on its records as
// they are now: records locked by a decision taken since the proposal was
// written are left untouched (neither moved nor re-locked).
//
// Confirming merges the remaining records as Merge does; with fewer than two
// left it returns ErrProposalStale and the proposal stays pending. Rejecting
// keeps the key apart for good (it is never merged or proposed again) and
// separates what an earlier automatic merge had already joined: each record
// sharing an entity with a lower-id proposal record gets its own new entity.
// Either way the records are locked by the decision.
func (r *Resolver) Decide(ctx context.Context, proposalID int64, confirm bool, user string) error {
	return r.DecideExpecting(ctx, proposalID, confirm, user, DecisionExpectation{})
}

// DecideExpecting is Decide that first checks exp (see DecisionExpectation)
// in the decision's transaction, after the identity lock and the row locks,
// against the target and the records the decision is about to act on.
func (r *Resolver) DecideExpecting(ctx context.Context, proposalID int64, confirm bool, user string, exp DecisionExpectation) error {
	user = strings.TrimSpace(user)
	if user == "" {
		return ErrNoUser
	}
	err := retryTx(ctx, r.Pool, func(tx pgx.Tx) error {
		if err := LockTx(ctx, tx); err != nil {
			return err
		}
		var (
			status, key string
			proposed    []int64
		)
		err := tx.QueryRow(ctx, `SELECT status, group_key, record_ids FROM identity_proposal WHERE id = $1 FOR UPDATE`,
			proposalID).Scan(&status, &key, &proposed)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrProposalNotFound
		}
		if err != nil {
			return fmt.Errorf("load proposal: %w", err)
		}
		if status != "pending" {
			return fmt.Errorf("proposal %d is %s: %w", proposalID, status, ErrProposalNotPending)
		}
		// The records still free, with their entities (all row-locked first).
		if err := lockNeighbourhood(ctx, tx, proposed); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, entity_id FROM person_record
			WHERE id = ANY ($1) AND locked_by_decision_id IS NULL AND entity_id IS NOT NULL
			ORDER BY id`, proposed)
		if err != nil {
			return fmt.Errorf("load records: %w", err)
		}
		type rec struct{ ID, Entity int64 }
		free, err := pgx.CollectRows(rows, pgx.RowToStructByPos[rec])
		if err != nil {
			return fmt.Errorf("load records: %w", err)
		}
		ids := make([]int64, len(free))
		for i, f := range free {
			ids[i] = f.ID
		}
		// The caller's view, checked under the locks: the free records, and
		// the target of a confirmation, the entity of the lowest-id free
		// record (analystMerge's target, as the proposals list shows it).
		if exp.RecordIDs != nil && !slices.Equal(sortedUnique(exp.RecordIDs), ids) {
			return fmt.Errorf("proposal %d: records %v, caller saw %v: %w", proposalID, ids, exp.RecordIDs, ErrExpectationFailed)
		}
		if confirm && exp.CheckTarget && (len(free) == 0 || free[0].Entity != exp.TargetEntityID) {
			return fmt.Errorf("proposal %d: target changed, caller saw %d: %w", proposalID, exp.TargetEntityID, ErrExpectationFailed)
		}
		reason := fmt.Sprintf("proposal %d (%s)", proposalID, key)
		var decision int64
		newStatus := "rejected"
		if confirm {
			if len(ids) < 2 {
				return fmt.Errorf("proposal %d: %w", proposalID, ErrProposalStale)
			}
			newStatus = "confirmed"
			if _, decision, err = analystMerge(ctx, tx, ids, user, "confirmed "+reason); err != nil {
				return err
			}
		} else {
			listed := ids
			if len(listed) == 0 {
				listed = sortedUnique(proposed) // nothing left to lock; the key is still rejected
			}
			if decision, err = insertDecision(ctx, tx, "reject", listed, nil, "rejected "+reason, user); err != nil {
				return err
			}
			seen := map[int64]bool{}
			var stay []int64
			for _, f := range free {
				if !seen[f.Entity] {
					seen[f.Entity] = true
					stay = append(stay, f.ID)
					continue
				}
				entity, err := newPersonEntity(ctx, tx, f.ID)
				if err != nil {
					return err
				}
				if _, err := place(ctx, tx, []int64{f.ID}, entity, decision); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE person_record SET locked_by_decision_id = $2 WHERE id = ANY ($1)`,
				stay, decision); err != nil {
				return fmt.Errorf("lock records: %w", err)
			}
			if err := repoint(ctx, tx, `SELECT unnest($1::bigint[])`, stay); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE identity_proposal
			SET status = $2, decided_by = $3, decided_at = now(), decision_id = $4 WHERE id = $1`,
			proposalID, newStatus, user, decision)
		if err != nil {
			return fmt.Errorf("record decision on proposal: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("identity: decide proposal %d: %w", proposalID, err)
	}
	return nil
}

// analystMerge merges ids (sorted, distinct, already locked) into the entity
// of the first one.
func analystMerge(ctx context.Context, tx pgx.Tx, ids []int64, user, reason string) (entity, decision int64, err error) {
	if err := tx.QueryRow(ctx, `SELECT entity_id FROM person_record WHERE id = $1`, ids[0]).Scan(&entity); err != nil {
		return 0, 0, fmt.Errorf("target entity: %w", err)
	}
	if decision, err = insertDecision(ctx, tx, "merge", ids, &entity, reason, user); err != nil {
		return 0, 0, err
	}
	_, err = place(ctx, tx, ids, entity, decision)
	return entity, decision, err
}

// expectCover checks that ids (locked by lockRecords) are exactly all the
// person records of entities (sorted, distinct), each of which has one.
func expectCover(ctx context.Context, tx pgx.Tx, ids, entities []int64) error {
	var stray, missing, empty int
	err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM person_record WHERE id = ANY ($1) AND entity_id <> ALL ($2)),
		(SELECT count(*) FROM person_record WHERE entity_id = ANY ($2) AND id <> ALL ($1)),
		(SELECT count(*) FROM unnest($2::bigint[]) e(id)
			WHERE NOT EXISTS (SELECT 1 FROM person_record p WHERE p.entity_id = e.id AND p.id = ANY ($1)))`,
		ids, entities).Scan(&stray, &missing, &empty)
	if err != nil {
		return fmt.Errorf("check entities: %w", err)
	}
	if stray+missing+empty > 0 {
		return fmt.Errorf("records outside the entities %d, records of the entities left out %d, entities without a record %d: %w",
			stray, missing, empty, ErrExpectationFailed)
	}
	return nil
}

// expectSplit checks that ids (locked by lockRecords) all belong to entity
// and that it has at least one record outside ids.
func expectSplit(ctx context.Context, tx pgx.Tx, ids []int64, entity int64) error {
	var elsewhere, rest int
	err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM person_record WHERE id = ANY ($1) AND entity_id <> $2),
		(SELECT count(*) FROM person_record WHERE entity_id = $2 AND id <> ALL ($1))`,
		ids, entity).Scan(&elsewhere, &rest)
	if err != nil {
		return fmt.Errorf("check entity: %w", err)
	}
	if elsewhere > 0 || rest == 0 {
		return fmt.Errorf("records outside entity %d: %d, records left in it: %d: %w", entity, elsewhere, rest, ErrExpectationFailed)
	}
	return nil
}

// lockNeighbourhood takes the identity lock and row-locks, in one statement
// and in id order, the records ids and every record sharing an entity with
// one of them: place may update those too (relaxLoneRecords), and id order is
// the order AssertRelationship takes its share locks in, so a concurrent
// ingest waits instead of deadlocking.
func lockNeighbourhood(ctx context.Context, tx pgx.Tx, ids []int64) error {
	if err := LockTx(ctx, tx); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT count(*) FROM (
		SELECT p.id FROM person_record p
		WHERE p.id = ANY ($1) OR p.entity_id IN (SELECT entity_id FROM person_record WHERE id = ANY ($1))
		ORDER BY p.id FOR UPDATE OF p) l`, ids)
	if err != nil {
		return fmt.Errorf("lock records: %w", err)
	}
	return nil
}

// lockRecords locks ids (sorted) and their neighbourhood and checks that
// every record exists and is linked to an entity.
func lockRecords(ctx context.Context, tx pgx.Tx, ids []int64) error {
	if err := lockNeighbourhood(ctx, tx, ids); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id FROM person_record WHERE id = ANY ($1) AND entity_id IS NOT NULL ORDER BY id`, ids)
	if err != nil {
		return fmt.Errorf("lock records: %w", err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return fmt.Errorf("lock records: %w", err)
	}
	for _, id := range ids {
		if _, ok := slices.BinarySearch(found, id); !ok {
			return fmt.Errorf("record %d: %w", id, ErrRecordNotFound)
		}
	}
	return nil
}

// newPersonEntity creates a person entity named after the record.
func newPersonEntity(ctx context.Context, tx pgx.Tx, recordID int64) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO entity (kind, name, name_norm)
		SELECT 'person', name, name_norm FROM person_record WHERE id = $1 RETURNING id`, recordID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create entity: %w", err)
	}
	return id, nil
}

func insertDecision(ctx context.Context, tx pgx.Tx, action string, ids []int64, entity *int64, reason, user string) (int64, error) {
	var confidence *float64
	if entity != nil {
		one := 1.0
		confidence = &one
	}
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO identity_decision (kind, action, record_ids, entity_id, confidence, reason, decided_by)
		VALUES ('analyst', $1, $2, $3, $4, $5, $6) RETURNING id`, action, ids, entity, confidence, reason, user).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert decision: %w", err)
	}
	return id, nil
}

// place moves ids to entity as an analyst decision (locked, confidence 1),
// re-points every relationship citing them whose pointer differs, and gives
// confidence 1 to a record left alone behind. It returns the entities the
// records were in before.
func place(ctx context.Context, tx pgx.Tx, ids []int64, entity, decision int64) ([]int64, error) {
	rows, err := tx.Query(ctx, `
		WITH old AS (SELECT id, entity_id FROM person_record WHERE id = ANY ($1))
		UPDATE person_record p SET entity_id = $2, identity_confidence = 1, locked_by_decision_id = $3
		FROM old WHERE p.id = old.id
		RETURNING old.entity_id`, ids, entity, decision)
	if err != nil {
		return nil, fmt.Errorf("place records: %w", err)
	}
	previous, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("place records: %w", err)
	}
	if err := repoint(ctx, tx, `SELECT unnest($1::bigint[])`, ids); err != nil {
		return nil, err
	}
	return previous, relaxLoneRecords(ctx, tx, `SELECT unnest($1::bigint[])`, previous)
}

func validate(recordIDs []int64, minimum int, user string) ([]int64, string, error) {
	user = strings.TrimSpace(user)
	if user == "" {
		return nil, "", ErrNoUser
	}
	ids := sortedUnique(recordIDs)
	if len(ids) < minimum {
		return nil, "", fmt.Errorf("%w: got %d, need %d", ErrTooFewRecords, len(ids), minimum)
	}
	return ids, user, nil
}

func sortedUnique(ids []int64) []int64 {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}
