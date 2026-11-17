# Verilog known gaps

- A task called as a bare name (`reset_all;` in an `always` block, alu.v line
  46) makes the grammar report one missing token. The file is read as
  `partial`, and the definitions around it are still extracted, so the
  fixture keeps the line to prove that.
- `function new()` of a class has no name node of its own
  (`class_constructor_declaration`), so constructors are not held.
- Out-of-class definitions (`function int Packet::size();`) and `extern` /
  `pure virtual` method prototypes are not covered by the fixture and are not
  extracted.
- Type parameters (`parameter type T = int`) are not held.
