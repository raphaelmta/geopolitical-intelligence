-- +goose Up

-- Party numbers in the TSE staging tables: a party donor published without
-- its sigla (SG_PARTIDO_DOADOR blank) but with NR_PARTIDO_DOADOR is mapped to
-- the sigla the candidacies of the same year give that number, when only one
-- sigla has it (docs/sources/tse.md).
ALTER TABLE tse_stg_cand ADD COLUMN party_number text NOT NULL DEFAULT '';
ALTER TABLE tse_stg_receipt ADD COLUMN donor_party_number text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tse_stg_receipt DROP COLUMN donor_party_number;
ALTER TABLE tse_stg_cand DROP COLUMN party_number;
