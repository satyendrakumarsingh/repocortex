package store;

import haxe.ds.StringMap;

@:keep
class Box<T:Dynamic> extends Base<T> implements Container {
	@:isVar public var value(get, set):T;
	@:optional public var label:String;

	public function new(v:T) {
		super();
		this.value = v;
	}

	@:keep public function get_value():T {
		return value;
	}

	@:generic public function convert<U:Dynamic>(x:U):T {
		return cast x;
	}

	public function set_value(v:T):T {
		return value = v;
	}
}

@:keep
interface Container {
	@:optional function reset():Void;
	var size:Int;
}

@:generic
class Pool<T:(Dynamic, Container)> {
	var items:Array<T> = [];

	public static function make<K:Dynamic>(k:K):Pool<K> {
		return new Pool();
	}

	public function size():Int {
		return items.length;
	}
}

typedef Handler<T:Dynamic> = T->Void;

class Last {
	public function new() {}
}
