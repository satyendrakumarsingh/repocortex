package hscript;

enum Const {
	CInt( v : Int );
	CFloat( f : Float );
	CString( s : String );
}

#if hscriptPos
typedef Expr = {
	var e : ExprDef;
	var pmin : Int;
	var pmax : Int;
}
enum ExprDef
#else
enum Expr
#end
{
	EConst( c : Const );
	EIdent( v : String );
	EVar( n : String, ?t : CType, ?e : Expr );
	EParent( e : Expr );
	EBlock( e : Array<Expr> );
}

typedef Argument = { name : String, ?t : CType, ?opt : Bool };

enum CType {
	CTPath( path : Array<String>, ?params : Array<CType> );
	CTFun( args : Array<CType>, ret : CType );
}

typedef Point = {> Base, var z : Int }

class Tools {
	public static function expr( e : Expr ) : ExprDef {
		return e;
	}

	public static function iter( e : Expr, f : Expr -> Void ) {
		switch( expr(e) ) {
		case EConst(_), EIdent(_):
		case EBlock(el): for( e in el ) f(e);
		default:
		}
	}
}

abstract Meters(Float) from Float to Float {
	public inline function new(v:Float) this = v;
	public function toFeet():Float return this * 3.28;
}

enum abstract Level(Int) {
	var Low = 0;
	var High = 1;
}

@:autoBuild(macros.Live.build())
extern interface Live {
	function reload():Void;
}

class Last {
	public function new() {}
	public inline function twice(x:Int):Int return x * 2;
}
