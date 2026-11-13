-- +goose Up

-- Run with the loaders stopped: plain CREATE INDEX inside the migration
-- transaction blocks writes on relationship while the index builds (minutes
-- on the national base).

-- The TSE loader looks up the OPEN donated_to of a receipt by the candidacy
-- record it targets and the receipt id (role): the receipt-conflict check
-- (sqlReceiptConflicts) and the insert guard (sqlRelInsert) in
-- internal/connector/tse/upsert.go. relationship_dst_record_id alone reads
-- every claim targeting the candidacy for each probe. The predicates of
-- those queries match this definition exactly (rel_type = 'donated_to',
-- closed_collection_id IS NULL, dst_record_id, attrs ->> 'role').
CREATE INDEX relationship_open_receipt ON relationship (dst_record_id, (attrs ->> 'role'))
    WHERE rel_type = 'donated_to' AND closed_collection_id IS NULL;

-- +goose Down
DROP INDEX relationship_open_receipt;
