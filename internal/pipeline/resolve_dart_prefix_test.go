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

// TestDartShowHide checks the combinators of `import` and `export`: `show`
// limits an import to the names it lists, `hide` takes the listed names out, a
// barrel's own combinators are carried to whoever imports it, and a name an
// import keeps out is still bound through another that exposes it. a.dart and
// b.dart define the same names, so a name that binds to the wrong one shows.
func TestDartShowHide(t *testing.T) {
	atLeastLow := model.ConfidenceLow
	got := langEdges(t, map[string]string{
		"pubspec.yaml":       "name: mypkg\n",
		"lib/src/a.dart":     "void f() {}\nvoid g() {}\nvoid h() {}\nclass Box {\n  static Box make() => Box();\n}\n",
		"lib/src/b.dart":     "void f() {}\nvoid g() {}\nvoid h() {}\n",
		"lib/shop_show.dart": "export 'src/a.dart' show f;\n",
		"lib/shop_hide.dart": "export 'src/a.dart' hide g;\n",
		"lib/shop_all.dart":  "export 'src/a.dart';\n",
		// Direct imports.
		"lib/use_show.dart": "import 'src/a.dart' show f, Box;\nvoid useShow() {\n  f();\n  g();\n  Box.make();\n}\n",
		"lib/use_hide.dart": "import 'src/a.dart' hide g;\nvoid useHide() {\n  f();\n  g();\n  h();\n}\n",
		// Hidden from one import, exposed by another.
		"lib/use_both.dart":   "import 'src/a.dart' hide g;\nimport 'src/b.dart' show g;\nvoid useBoth() {\n  g();\n}\n",
		"lib/use_viabar.dart": "import 'src/a.dart' hide g;\nimport 'shop_all.dart';\nvoid useViaBarrel() {\n  g();\n}\n",
		// Barrels.
		"lib/use_bshow.dart":  "import 'shop_show.dart';\nvoid useBarrelShow() {\n  f();\n  g();\n}\n",
		"lib/use_bhide.dart":  "import 'shop_hide.dart';\nvoid useBarrelHide() {\n  f();\n  g();\n}\n",
		"lib/use_chain.dart":  "import 'shop_hide.dart' show g, h;\nvoid useChain() {\n  f();\n  g();\n  h();\n}\n",
		"lib/use_prefix.dart": "import 'src/a.dart' as a hide g;\nvoid usePrefix() {\n  a.f();\n  a.g();\n}\n",
	})
	// show keeps the listed names, and the static member of a listed type.
	requireEdge(t, got, "use_show.dart:useShow", "src/a.dart:f", atLeastLow)
	forbidEdge(t, got, "use_show.dart:useShow", "src/a.dart:g")
	requireEdge(t, got, "use_show.dart:useShow", "src/a.dart:Box.make", atLeastLow)
	// hide takes the listed names out.
	requireEdge(t, got, "use_hide.dart:useHide", "src/a.dart:f", atLeastLow)
	requireEdge(t, got, "use_hide.dart:useHide", "src/a.dart:h", atLeastLow)
	forbidEdge(t, got, "use_hide.dart:useHide", "src/a.dart:g")
	// Another import exposes what one hides.
	requireEdge(t, got, "use_both.dart:useBoth", "src/b.dart:g", atLeastLow)
	forbidEdge(t, got, "use_both.dart:useBoth", "src/a.dart:g")
	requireEdge(t, got, "use_viabar.dart:useViaBarrel", "src/a.dart:g", atLeastLow)
	// An export's combinators reach the importer of the barrel.
	requireEdge(t, got, "use_bshow.dart:useBarrelShow", "src/a.dart:f", atLeastLow)
	forbidEdge(t, got, "use_bshow.dart:useBarrelShow", "src/a.dart:g")
	requireEdge(t, got, "use_bhide.dart:useBarrelHide", "src/a.dart:f", atLeastLow)
	forbidEdge(t, got, "use_bhide.dart:useBarrelHide", "src/a.dart:g")
	// The import's own `show` narrows the barrel's further.
	requireEdge(t, got, "use_chain.dart:useChain", "src/a.dart:h", atLeastLow)
	forbidEdge(t, got, "use_chain.dart:useChain", "src/a.dart:g")
	forbidEdge(t, got, "use_chain.dart:useChain", "src/a.dart:f")
	// A prefixed import takes combinators too.
	requireEdge(t, got, "use_prefix.dart:usePrefix", "src/a.dart:f", atLeastLow)
	forbidEdge(t, got, "use_prefix.dart:usePrefix", "src/a.dart:g")
	// Nothing here imports b.dart except use_both.
	forbidEdge(t, got, "use_show.dart", "src/b.dart")
	forbidEdge(t, got, "use_hide.dart", "src/b.dart")
	forbidEdge(t, got, "use_bshow.dart", "src/b.dart")
	forbidEdge(t, got, "use_chain.dart", "src/b.dart")
}
