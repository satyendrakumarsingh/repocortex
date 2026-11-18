package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestDartPrefixedImportAndPrivateNames checks two rules of a Dart library:
// a name an `import ... as p` brings in is reachable only through `p.`, and a
// `_private` name is private to its library, so no other library's call binds
// to it, unless the target is a `part` of the caller's own library.
func TestDartPrefixedImportAndPrivateNames(t *testing.T) {
	atLeastLow := model.ConfidenceLow
	got := langEdges(t, map[string]string{
		"pubspec.yaml":          "name: mypkg\n",
		"lib/mypkg.dart":        "export 'src/a.dart';\nexport 'src/cyc.dart';\n",
		"lib/src/a.dart":        "void _private() {}\nvoid pub() {}\nclass K {\n  void _hidden() {}\n  void shown() {}\n}\n",
		"lib/src/b.dart":        "void _private() {}\nvoid pub() {}\nvoid onlyb() {}\n",
		"lib/src/cyc.dart":      "export '../mypkg.dart';\nvoid cyc() {}\n",
		"lib/src/user.dart":     "import 'b.dart';\nimport 'a.dart' as a;\nvoid use() {\n  pub();\n  _private();\n  onlyb();\n}\nvoid prefixed() {\n  a.pub();\n}\n",
		"lib/src/user2.dart":    "import '../mypkg.dart';\nimport 'package:mypkg/src/b.dart' hide pub;\nvoid use2() {\n  _private();\n  pub();\n  onlyb();\n  cyc();\n}\n",
		"lib/src/user3.dart":    "void use3() {\n  _private();\n  pub();\n}\n",
		"lib/src/lib.dart":      "library lib;\npart 'lib_part.dart';\nvoid _own() {}\nvoid entry() { _fromPart(); }\n",
		"lib/src/lib_part.dart": "part of 'lib.dart';\nvoid _fromPart() { _own(); }\n",
	})
	// An unprefixed name is the one the unprefixed import brings in.
	requireEdge(t, got, "user.dart:use", "src/b.dart:pub", atLeastLow)
	forbidEdge(t, got, "user.dart:use", "src/a.dart:pub")
	// Through its prefix, the other one.
	requireEdge(t, got, "user.dart:prefixed", "src/a.dart:pub", atLeastLow)
	forbidEdge(t, got, "user.dart:prefixed", "src/b.dart:pub")
	requireEdge(t, got, "user.dart:use", "src/b.dart:onlyb", atLeastLow)
	// A private name of another library is nobody's.
	forbidEdge(t, got, "user.dart:use", "a.dart:_private")
	forbidEdge(t, got, "user.dart:use", "b.dart:_private")
	forbidEdge(t, got, "user2.dart:use2", "a.dart:_private")
	forbidEdge(t, got, "user2.dart:use2", "b.dart:_private")
	forbidEdge(t, got, "user3.dart:use3", "_private")
	// `hide pub` leaves the name the barrel exports.
	requireEdge(t, got, "user2.dart:use2", "src/a.dart:pub", atLeastLow)
	forbidEdge(t, got, "user2.dart:use2", "src/b.dart:pub")
	requireEdge(t, got, "user2.dart:use2", "src/cyc.dart:cyc", atLeastLow)
	// A part is the same library as the file it belongs to.
	requireEdge(t, got, "lib.dart:entry", "lib_part.dart:_fromPart", atLeastLow)
	requireEdge(t, got, "lib_part.dart:_fromPart", "lib.dart:_own", atLeastLow)
}
