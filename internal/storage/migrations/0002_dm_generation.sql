-- Incremented whenever a participant requests their poll DM again. It is part
-- of the DM's metadata key, so that crash recovery never mistakes the old DM
-- for the newly requested one.
ALTER TABLE participants ADD COLUMN dm_generation INTEGER NOT NULL DEFAULT 0;
