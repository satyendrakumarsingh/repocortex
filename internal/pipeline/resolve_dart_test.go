package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestDartResolution indexes a small package and its test and checks every
// call against what the imports make it reach.
//
// What it is built to catch: `u.log` through `as u` where lib/other/util.dart
// has a `log` too; `format` written bare where lib/ui/text.dart, which cart.dart
// does not import, also has one; a call on a field of declared type
// (`store.save`) against a second class that declares `save`; a method called
// on a value whose type nothing states (`thing.save`); a relative import of
// 'models/item.dart' against the package-style one; and the framework's own
// `Text` widget, which the repository also happens to declare. That last one
// binds at low, wrongly, and is listed as known: cart.dart imports flutter's
// library, which the index cannot see into, and a name nothing else
// explains is bound to the one definition of it the repository has.
func TestDartResolution(t *testing.T) {
	got := langEdges(t, dartFixture())
	high := model.ConfidenceHigh
	wants := []want{
		{"lib/src/cart.dart:Cart.total", "lib/src/pricing.dart:discount", high},
		{"lib/src/cart.dart:Cart.total", "lib/src/models/item.dart:Item", high},
		{"lib/src/cart.dart:Cart.total", "lib/src/pricing.dart:Tax.rate", high},
		{"lib/src/cart.dart:Cart.total", "lib/src/cart.dart:Cart.helper", high},
		{"lib/src/cart.dart:Cart.total", "lib/src/cart.dart:Cart.recalc", high},
		{"lib/src/cart.dart:Cart.total", "lib/src/util.dart:log", high},
		{"lib/src/cart.dart:Cart.total", "lib/src/store.dart:Store.save", high},
		{"lib/src/cart.dart:Cart.total", "lib/src/cache.dart:makeCache", high},
		{"lib/src/cache.dart:makeCache", "lib/src/cache.dart:Cache", high},
		{"lib/src/models/item.dart:Item.blank", "lib/src/models/item.dart:Item", high},
		{"lib/src/pricing.dart:Tax.apply", "lib/src/pricing.dart:Tax.rate", high},
		{"test/cart_test.dart:main", "lib/src/cart.dart:Cart", high},
		{"test/cart_test.dart:main", "lib/src/cart.dart:Cart.recalc", high},
		{"test/cart_test.dart:main", "lib/src/store.dart:Store", high},
		// A barrel re-exports: app.dart imports lib/shop.dart alone, which
		// exports pricing.dart, and more.dart, which exports deep.dart (and
		// shop.dart again, a cycle).
		{"lib/app.dart:run", "lib/src/pricing.dart:discount", high},
		{"lib/app.dart:run", "lib/src/deep.dart:rebate", high},
		// A generated part is part of the library that names it.
		{"lib/src/model.dart:Model.toJson", "lib/src/model.g.dart:_$ModelToJson", high},
		{"lib/src/model.dart:Model.from", "lib/src/model.g.dart:_$ModelFromJson", high},
		{"lib/src/model.g.dart:_$ModelFromJson", "lib/src/model.dart:Model", high},
	}
	forbid := []want{
		{Src: "lib/src/", Dst: "lib/other/util.dart"},
		// util.dart is imported `as u`: its names are reached through `u.`, so
		// the unprefixed `format(x)` is not a call of its `format`.
		{Src: "lib/src/cart.dart:Cart.total", Dst: "lib/src/util.dart:format"},
		{Src: "lib/src/cart.dart", Dst: "lib/ui/text.dart:format"},
		{Src: "lib/src/cart.dart:Cart.total", Dst: "lib/src/cache.dart:Cache.save"},
		// internal.dart is exported by no barrel, and legacy.g.dart is a part of
		// another library.
		{Src: "lib/app.dart", Dst: "lib/src/internal.dart"},
		{Src: "lib/src/model.dart", Dst: "lib/src/legacy.g.dart"},
	}
	scoreEdges(t, got, wants, forbid, nil,
		want{Src: "lib/src/cart.dart:Cart.build", Dst: "lib/ui/text.dart:Text"})
}

func dartFixture() map[string]string {
	return map[string]string{
		"pubspec.yaml": "name: shop\nenvironment:\n  sdk: '>=3.0.0 <4.0.0'\n",
		"lib/src/models/item.dart": `class Item {
  final String name;
  final double cost;
  Item({required this.name, this.cost = 0});

  double get price => cost;

  static Item blank() => Item(name: '');
}
`,
		"lib/src/pricing.dart": `import 'models/item.dart';

double discount(Item item) {
  return item.price * 0.9;
}

class Tax {
  static double rate() => 0.2;
  double apply(double x) => x * (1 + rate());
}
`,
		"lib/src/util.dart": `void log(String message) {
  print(message);
}

String format(double d) => d.toString();
`,
		"lib/other/util.dart": `void log(String message) {
  print('other: ' + message);
}
`,
		"lib/src/store.dart": `import 'models/item.dart';

class Store {
  void save(Item item) {}
}
`,
		"lib/src/cache.dart": `import 'models/item.dart';

class Cache {
  void save(Item item) {}
}

Cache makeCache() => Cache();
`,
		"lib/src/cart.dart": `import 'package:flutter/material.dart';
import 'package:shop/src/util.dart' as u;
import 'cache.dart';
import 'models/item.dart';
import 'pricing.dart';
import 'store.dart';

class Cart {
  final Store store;
  Cart(this.store);

  double total(Item item) {
    var x = discount(item);
    var y = Item(name: 'a');
    var t = Tax.rate();
    helper(x);
    this.recalc();
    u.log('total');
    print('hi');
    store.save(item);
    var thing = makeCache();
    thing.save(item);
    var s = format(x);
    return x + t;
  }

  void recalc() {}

  double helper(double v) => v;

  Widget build(BuildContext context) => Text('cart');
}
`,
		"lib/shop.dart": `library shop;

export 'src/pricing.dart';
export 'src/more.dart';
`,
		"lib/src/more.dart": `export 'deep.dart';
export '../shop.dart';

double bonus() => 1;
`,
		"lib/src/deep.dart": `double rebate(double d) => d * 0.5;
`,
		"lib/src/internal.dart": `double rebate(double d) => d;

void secret() {}
`,
		"lib/app.dart": `import 'package:shop/shop.dart';

void run() {
  discount(null);
  rebate(2.0);
}
`,
		"lib/src/model.dart": `part 'model.g.dart';

class Model {
  Map toJson() {
    return _$ModelToJson(this);
  }

  static Model from(Map j) {
    return _$ModelFromJson(j);
  }
}
`,
		"lib/src/model.g.dart": `part of 'model.dart';

Map _$ModelToJson(Model m) {
  return {};
}

Model _$ModelFromJson(Map j) {
  return Model();
}
`,
		"lib/src/legacy.dart": `part 'legacy.g.dart';

class Legacy {}
`,
		"lib/src/legacy.g.dart": `part of 'legacy.dart';

Map _$ModelToJson(Object m) => {};
`,
		"lib/ui/text.dart": `class Text {
  final String data;
  Text(this.data);
}

String format(double d) => d.toStringAsFixed(2);
`,
		"test/cart_test.dart": `import 'package:test/test.dart';
import 'package:shop/src/cart.dart';
import 'package:shop/src/store.dart';

void main() {
  test('total', () {
    final cart = Cart(Store());
    cart.recalc();
  });
}
`,
	}
}
