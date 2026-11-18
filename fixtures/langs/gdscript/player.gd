@tool
class_name Player
extends CharacterBody2D

signal health_changed(old_value, new_value)
signal died

enum State { IDLE, RUNNING, JUMPING }
enum { SMALL, LARGE }

const MAX_SPEED = 300.0
const GRAVITY: float = 980.0

@export var speed: float = 200.0
@export_range(0, 100) var health: int = 100
@onready var sprite = $Sprite2D
var velocity_scale = 1.0
var state: State = State.IDLE
static var instances = 0

func _ready():
	var local_total = 0
	print("ready")

func _physics_process(delta: float) -> void:
	var direction = Input.get_axis("left", "right")
	velocity.x = direction * speed
	move_and_slide()

func take_damage(amount: int) -> bool:
	health -= amount
	health_changed.emit(health + amount, health)
	if health <= 0:
		died.emit()
		return true
	return false

static func create(name: String) -> Player:
	var p = Player.new()
	return p

func _helper(
		a: int,
		b: int
) -> int:
	return a + b

class Inventory:
	var items = []
	const LIMIT = 10

	func add(item):
		items.append(item)

	class Slot:
		var index = 0

		func clear():
			index = 0

class Weapon extends Node:
	signal fired

	func fire():
		fired.emit()

var health_setter: int = 0:
	set(value):
		health_setter = value
	get:
		return health_setter

export(int) var legacy_export = 1
onready var legacy_node = $Child
export var legacy_plain = 2

var callback = func(x):
	return x * 2

func uses_lambda():
	var f = func(a): return a + 1
	return f.call(1)

func _init(start: int = 0):
	health = start

class Bag:
	var size: int

	func _init(n: int):
		size = n
