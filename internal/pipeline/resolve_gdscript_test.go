package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestGDScriptResolution indexes a small Godot project (scripts with and without a
// class_name, an inner class and a GUT test) and checks every call against what the
// language's names reach.
//
// What it is built to catch: `Weapon.new()` and `w.fire()` where other_weapon.gd also
// has a `fire` and `reload` and is not the script preloaded, and `Weapon` is a
// constant the script names it by (it declares no class_name); `Util.slug` through a
// preload of a path under another folder of the project; `extends Base`, by a
// class_name, and `extends "res://scripts/base.gd"`, by a path, both of which make the
// parent's members the script's own for a bare call; `Player.make()` and `Player.new()`
// from a test, by a class_name; `self.helper()`, `super._ready()` and a member of an
// inner class called from the script's own functions; `print` and `Input.is_action_pressed`,
// which are the engine's; `add_child`, which the engine class provides and no script
// declares; and a typed parameter (`other: Player`).
//
// What it does not bind, by design: a method the engine class a script extends
// provides, a script reached by a path the code builds as a string, and a call on a
// value whose type the source does not state where more than one script has the name.
func TestGDScriptResolution(t *testing.T) {
	got := langEdges(t, gdscriptFixture())
	high, low := model.ConfidenceHigh, model.ConfidenceLow
	wants := []want{
		// The script's own members, and an inner class's.
		{"scripts/player.gd:Player._ready", "scripts/player.gd:Player.helper", high},
		{"scripts/player.gd:Player.Inventory.add", "scripts/player.gd:Player.Inventory.clear", high},
		{"scripts/player.gd:Player.test_it", "scripts/player.gd:Player.helper", high},
		{"scripts/player.gd:Player.test_it", "scripts/player.gd:Player.make", high},
		// A script preloaded under a name, which declares no class_name.
		{"scripts/player.gd:Player._ready", "scripts/weapon.gd:weapon.fire", high},
		{"scripts/player.gd:Player._ready", "scripts/weapon.gd:weapon._init", high},
		{"scripts/player.gd:Player._ready", "lib/util.gd:util.slug", high},
		// What the parent declares, named by a class_name and by a path.
		{"scripts/player.gd:Player._ready", "scripts/base.gd:Base.base_tick", high},
		{"scripts/player.gd:Player._ready", "scripts/base.gd:Base._ready", high},
		{"scripts/enemy.gd:Enemy.attack", "scripts/base.gd:Base.shared", high},
		// A script by its class_name, from a test.
		{"test/test_player.gd:test_player.test_it", "scripts/player.gd:Player.make", high},
		{"test/test_player.gd:test_player.test_it", "scripts/player.gd:Player.helper", high},
		{"test/test_player.gd:test_player.test_it", "scripts/player.gd:Player", high},
		{"scripts/player.gd:Player.make", "scripts/player.gd:Player", high},
		{"scripts/player.gd:Player._ready", "scripts/player.gd:Player.Inventory", high},
	}
	forbid := []want{
		// other_weapon.gd is not the script preloaded.
		{Src: "scripts/player.gd", Dst: "scripts/other_weapon.gd"},
		{Src: "test/", Dst: "scripts/other_weapon.gd"},
		// The engine's names are the engine's.
		{Src: "scripts/", Dst: "scripts/debug.gd"},
	}
	_ = low
	scoreEdges(t, got, wants, forbid, nil)
}

func gdscriptFixture() map[string]string {
	return map[string]string{
		"project.godot": `config_version=5

[application]
config/name="Shop"
`,
		"lib/util.gd": `extends RefCounted

static func slug(s: String) -> String:
	return s

static func pad(s: String) -> String:
	return s
`,
		"scripts/base.gd": `class_name Base
extends Node

func _ready() -> void:
	pass

func base_tick() -> void:
	pass

func shared() -> void:
	pass
`,
		// A script with a name of its own that nothing in the project loads.
		"scripts/debug.gd": `extends Node

func print_line(s) -> void:
	pass

func fire() -> void:
	pass
`,
		"scripts/enemy.gd": `class_name Enemy
extends "res://scripts/base.gd"

func attack() -> void:
	shared()
`,
		// The same names as weapon.gd, in a script nobody preloads.
		"scripts/other_weapon.gd": `extends Node

func fire() -> void:
	pass

func reload() -> void:
	pass
`,
		"scripts/player.gd": `class_name Player
extends Base

const Weapon = preload("res://scripts/weapon.gd")
const Util = preload("res://lib/util.gd")

var health := 100

class Inventory:
	extends RefCounted

	func add() -> void:
		clear()

	func clear() -> void:
		pass

func _ready() -> void:
	var w := Weapon.new()
	w.fire()
	Util.slug("a")
	helper(1)
	base_tick()
	self.helper(2)
	print("x")
	add_child(w)
	Inventory.new().add()
	super._ready()
	if Input.is_action_pressed("jump"):
		pass

func helper(x: int) -> int:
	return x + 1

static func make() -> Player:
	return Player.new()

func test_it(other: Player) -> void:
	other.helper(1)
	Player.make()
`,
		"scripts/weapon.gd": `extends Node

func _init() -> void:
	pass

func fire() -> void:
	pass

func reload() -> void:
	pass
`,
		"test/test_player.gd": `extends GutTest

func test_it():
	var p := Player.new()
	p.helper(1)
	Player.make()
`,
	}
}
