# Make: forms not extracted

- **Targets spelled with a variable reference.** `$(NAME): main.go` and
  `$(BIN_DIR)/%.gz:` have a `variable_reference` or `concatenation` node in
  `targets` instead of a `word`. The name is only known after expansion, so no
  symbol is recorded.
- **Pattern rules** (`%.o: %.c`, `check-%:`) and **suffix rules** (`.c.o:`) are
  not definitions: there is no one name to look up. Special targets (`.PHONY`,
  `.SUFFIXES`, `.DEFAULT_GOAL`) are make's own and are skipped by their leading
  dot.
- **Variables defined only by `+=`.** An append is never recorded, so a
  variable that is first created with `+=` (legal, and rare) is missing.
- **Variables created by `$(eval ...)`, `$(foreach ...)` or `define`d
  macros** are not seen; the grammar parses these as function calls.
- **Make's own settings** (`SHELL`, `MAKEFLAGS`, `MAKEFILES`, `VPATH`, and
  names beginning with a dot) are not recorded.
- **Target-specific variables** (`app: CFLAGS = -O0`) are scoped to a target
  and are not recorded; the grammar gives them a list of targets before the
  variable name.
- **Non-GNU makefiles with space-indented recipes** (Watcom `wmake`, as in
  `spaces.mk`) are misread: the grammar takes a recipe line such as
  `  wlib -q -n libttf.lib` for a rule and records `wlib`, and the real target
  above it (`all` in `spaces.mk`, which has a `.symbolic` prerequisite and a
  `&` continuation before it) can be lost. Names that begin with `-`, `@` or
  `+` or contain a backslash cannot be targets and are filtered out; a plain
  word like `wlib` cannot be told from a real target.
- **Repeated names.** `all`, `clean`, `test` and `install` appear in nearly
  every Makefile; each is recorded once per file.
- **BSD make** (`.if`, `.else`, `.endif`, `.include`) is not understood: the grammar reads
  some directive lines as variable assignments, so every name that begins with
  a dot is dropped (`bsd.mk`).
- **A substitution reference with an empty replacement** (`$(OBJ:.c=)`,
  `$(BOARD_DIR:/=)`, `$(X:$(Y)=)`) is not in the grammar. `mask_make.go` now
  overwrites the text from the `:` to the `=` with spaces before a second
  parse, so the reference reads as `$(BOARD_DIR   )` and the file is read
  whole (`Makefile.stm32`, `Makefile.mimxrt`); what follows is what is left when
  the guard refuses the mask. Unmasked, it does not only fail
  on that line: the parser stops building `rule` and `variable_assignment`
  nodes for the rest of the file and returns a flat run of words, with each
  operator (`=`, `:=`, `:`) an ERROR node between two words (`recover.mk`).
  The query reads that run (`makeRecovered` in `lang_make.go`) and keeps the
  variables, targets and `define` macros it can tell apart. What is lost there:
  the assignment on the bad line itself when its operator is a plain `=`
  (`OBJ = $(SRC:.c=)`; an `+=` has the same tokens, so it cannot be told
  apart), the first of several targets in one rule, a target after a variable
  reference (`$(BUILD)/x:`), and every line the grammar then drops whole: once
  a `'"quoted"'` string appears in the flat run, the tokens up to the next
  variable reference are gone (a measured 34 of 75 definitions in
  micropython's `ports/stm32/Makefile`). In the flat run a recipe line such as
  `scp host:path .` reads as the target `host`, and `FOO=1 cmd` as the variable
  `FOO`; in a file that parses, neither is recorded.
