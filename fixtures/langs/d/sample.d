module shop.sample;

import std.stdio;
import shop.money : zero;

// Module-level declarations.
int limit = 10;
auto greeting = "hello";
private int hidden = 1;

alias Id = int;

int add(int a, int b) {
    int local = a + b;
    return local;
}

private int helper(int x) { return x; }

T identity(T)(T value) { return value; }

enum Color { red, green, blue }

struct Point {
    int x;
    int y;

    this(int x, int y) {
        this.x = x;
        this.y = y;
    }

    int sum() { return x + y; }
}

interface Shape {
    double area();
}

abstract class Base {
    protected int id;
    abstract void run();
}

class Cart : Base, Shape {
    private int total;
    static int count;

    this(int total) {
        this.total = total;
    }

    ~this() {}

    override void run() {
        auto z = zero();
        writeln(z);
    }

    double area() { return 0.0; }

    static Cart make() {
        return new Cart(1);
    }

    class Item {
        int price;
        int cost() { return price; }
    }
}

union Number {
    int i;
    float f;
}

unittest {
    assert(add(1, 2) == 3);
}
