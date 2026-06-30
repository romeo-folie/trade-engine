-- 034_orb_exit_source.sql
--
-- Records the *provenance* of an ORB trade's exit so a reader can always tell a
-- broker-confirmed fill from a bar-replay reconstruction.
--
-- Until now every recorded exit was implicitly broker-confirmed. The bar-replay
-- reconciler (previously paper-only, now the universal backstop) resolves an exit
-- from the market-data price path rather than an actual broker fill — accurate for
-- fixed-price TP/SL legs, but it is a *model* of the exit, not a confirmed one. The
-- distinction matters when evaluating strategy performance, so we tag it explicitly
-- instead of overloading exit_type.
--
--   'broker' — exit came from an IBKR fill (order-status poll or trades feed)
--   'replay' — exit reconstructed by replaying market-data bars over [fill, now]
--
-- Single-file (no .down) by convention: the migration runner globs every *.sql
-- file, so a .down.sql would be applied forward. The statement is idempotent.

ALTER TABLE orb_trades ADD COLUMN IF NOT EXISTS exit_source TEXT;
