module Shapes where

open import Agda.Builtin.Nat
open import Agda.Builtin.Bool

infixl 6 _+++_

data Color : Set where
  red   : Color
  green : Color
  blue  : Color

data List (A : Set) : Set where
  []   : List A
  _::_ : A → List A → List A

data Index : Nat → Set where
  here : Index zero

record Point : Set where
  constructor mkPoint
  field
    px : Nat
    py pz : Nat

record Pair (A B : Set) : Set where
  field
    first  : A
    second : B

not : Bool → Bool
not true  = false
not false = true

length : {A : Set} → List A → Nat
length []        = zero
length (_ :: xs) = suc (length xs)

_+++_ : Nat → Nat → Nat
zero    +++ m = m
(suc n) +++ m = suc (n +++ m)

if_then_else_ : Bool → Nat → Nat → Nat
if true  then a else b = a
if false then a else b = b

double : Nat → Nat
double n = suc (suc n)

unit : Nat
unit = suc (suc zero)

apply : (Nat → Nat) → Nat → Nat
apply f x = f x

compose : (Nat → Nat) → (Nat → Nat) → Nat → Nat
compose f g = λ x → f (g x)

postulate
  magic : Nat
  Trust : Set

private
  hidden : Nat
  hidden = zero

abstract
  secret : Nat
  secret = zero

instance
  natDefault : Nat
  natDefault = zero

module Inner where
  inner : Nat
  inner = double zero

  data Flag : Set where
    on off : Flag

  record Box : Set where
    field
      content : Nat

module Param (n : Nat) where
  scaled : Nat
  scaled = double n

module Alias = Inner

mutual
  isEven : Nat → Bool
  isEven zero    = true
  isEven (suc n) = isOdd n

  isOdd : Nat → Bool
  isOdd zero    = false
  isOdd (suc n) = isEven n

pattern one = suc zero

last : Nat
last = double one
