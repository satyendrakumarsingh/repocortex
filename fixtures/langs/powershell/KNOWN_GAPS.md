# PowerShell: forms not extracted

- **Script variables and configuration tables** (`$script:Counter = 0`,
  `$Settings = @{ ... }`) are not recorded: assignments are indistinguishable
  from the many local variables of a script.
- **DSC `Configuration Name { ... }`** is recorded as a function: the grammar
  reads it as a command with a name and a script block. `Node` and resource
  blocks inside it are commands, not definitions.
- **Class properties** (`[string] $Name`) and **enum members** are not
  recorded.
- **A function after an unreadable region.** The grammar fails on some
  constructs (a tab-expansion script of a real module, from line 328, 12 error
  nodes); a function that sits after the failure can be missing from the index.
