-- 0023_return_types: the type a definition declares it returns, and the call
-- a local variable's value came from.
--
-- WHY. A receiver's class was known only when the source stated it: a typed
-- parameter, field or local, a construction (`new Foo()`, `Foo.new`,
-- `Foo::new()`), or `self`. `order = repo.find(id); order.save()` stated
-- nothing about `order`, so `save` was bound by its name alone, at medium or
-- low, or not at all when several classes declare one. When `find` declares
-- that it returns an Order, the language has already said what `order` holds;
-- the index just had nowhere to keep either half of that sentence.
--
-- symbols.return_type is the plain type name a function or method declares it
-- returns (`Order`), as internal/parse reads it from the definition's syntax.
-- '' means no plain type was read: none declared, a primitive, or a type the
-- parser deliberately does not unwrap (`Optional<Order>`, `Promise<Order>`,
-- `Result<Order, E>`, a type parameter). The empty string is the default
-- because every existing row, every SCIP-imported symbol and every language
-- with no reader genuinely is "not read", and must not claim a type.
--
-- signature was considered and not used: internal/parse never fills it for
-- code symbols (0010_arity says the same), and filling it with a bare type name
-- would make every reader of that column print a return type as if it were a
-- whole signature.
--
-- symbol_references.value_line and value_col are, on a call made on a local
-- variable whose type the source does not state, the position of the call
-- whose result initialised that variable: the start_line and start_col of
-- that call's own row in this table, in the same file. 0 means no such call
-- was read. The resolver binds the producing call first and reads the return
-- type of what it bound; see internal/resolve/infer/returned.go.
--
-- Columns, not a side table: both are read on the hot resolution path beside
-- receiver_kind and param_count, which are columns for the same reason.

ALTER TABLE symbols ADD COLUMN return_type TEXT NOT NULL DEFAULT '';

ALTER TABLE symbol_references ADD COLUMN value_line INTEGER NOT NULL DEFAULT 0;
ALTER TABLE symbol_references ADD COLUMN value_col INTEGER NOT NULL DEFAULT 0;
