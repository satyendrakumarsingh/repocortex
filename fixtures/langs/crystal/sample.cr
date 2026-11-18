require "json"
require "./money"

MAX = 10
alias Id = Int32

module Shop::Util
  VERSION = "1"

  def self.slug(s)
    s
  end

  def pad(s)
    s
  end
end

abstract class Base
  abstract def area : Float64

  def name
    "base"
  end
end

class Shop::Cart < Base
  include Comparable(Cart)

  LIMIT = 5

  @items : Array(Int32)
  @total = 0

  def initialize(@items : Array(Int32))
  end

  def total(x : Int32) : Int32
    helper = x
    helper
  end

  def self.build
    new([1])
  end

  protected def inner
  end

  private def helper(x)
    x
  end

  def ==(other)
    true
  end

  def empty?
    @items.empty?
  end

  enum Kind
    Small
    Large

    def small?
      self == Small
    end
  end
end

struct Point
  def initialize(@x : Int32, @y : Int32)
  end

  def sum
    @x + @y
  end
end

enum Color
  Red
  Green
end

def top_level(a)
  a
end

private def hidden
end

top_level(1)
