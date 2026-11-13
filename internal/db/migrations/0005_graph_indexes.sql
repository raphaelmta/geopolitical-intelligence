-- +goose Up

-- Run with the loaders stopped: plain CREATE INDEX inside the migration
-- transaction blocks writes on relationship while the indexes build (minutes
-- on the national base).

-- Map queries (internal/graph) read the edges of an entity in relationship id
-- order: Neighbors pages with a keyset cursor (WHERE src_entity_id = $1 AND
-- id > $cursor ORDER BY id LIMIT n), once per direction. With (entity, id)
-- each page is a short ordered index range, whatever the entity's degree;
-- with the single-column indexes every page of a hub (a party with its
-- candidates and donations) reads and sorts all of the hub's edges.
-- The composite indexes also serve every equality lookup on the entity
-- column, so they replace the single-column ones of 0001.
CREATE INDEX relationship_src_entity_id_id ON relationship (src_entity_id, id);
CREATE INDEX relationship_dst_entity_id_id ON relationship (dst_entity_id, id);
DROP INDEX relationship_src_entity_id;
DROP INDEX relationship_dst_entity_id;

-- +goose Down
CREATE INDEX relationship_src_entity_id ON relationship (src_entity_id);
CREATE INDEX relationship_dst_entity_id ON relationship (dst_entity_id);
DROP INDEX relationship_src_entity_id_id;
DROP INDEX relationship_dst_entity_id_id;
