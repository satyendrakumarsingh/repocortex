# INI known gaps

## A key written with a colon separator

```
port: 5432
```

The grammar reads `port: 5432` as an error when it follows another setting
(`(ERROR (setting_name))` at the top level, with the setting lost). When it
does parse, the name comes out as `port: 5432`. The query does not extract
the key in either case, and the name correction keeps a colon-separated name
from being recorded as the key with its value. The fixture uses `=`.

## An indented line inside a section

```
[database]
  indented_key = yes
```

The grammar reads the indented line as a setting with an `ERROR` child. It is
not extracted. Some INI readers (Python's configparser, for one) treat an
indented line as a continuation of the previous value, not as a key, so the
omission is the correct answer for those files.

## Keys before the first section

A key before any `[section]` belongs to no section, so it is not extracted
(decided, not a gap). The fixture has one, `global_key`, to hold that rule.
