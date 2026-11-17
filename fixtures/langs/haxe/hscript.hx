package hscript;

import haxe.ds.StringMap;

class Colorizer extends Parser {
	public var keywords:Array<String>;
	static var instance:Colorizer;
	var count:Int = 0;

	public function new() {
		super();
		keywords = ["var", "function"];
	}

	public static function get():Colorizer {
		return instance;
	}

	public function colorize(code:String):String {
		return code;
	}

	function helper(x:Int):Int {
		return x * 2;
	}
}

interface Visitor {
	function visit(e:Expr):Void;
	var depth:Int;
}

class Interp {
	var vars:StringMap<Dynamic>;

	public function new() {
		vars = new StringMap();
	}

	public function execute(e:Expr):Dynamic {
		try {
			return exprReturn(e);
		} catch (err:Dynamic) {
			throw err;
		}
	}

	function exprReturn(e:Expr):Dynamic {
		return switch (e) {
			case EConst(c): c;
			default: null;
		}
	}

	public function executeFile(path:String):Void {
		trace(path);
	}
}
