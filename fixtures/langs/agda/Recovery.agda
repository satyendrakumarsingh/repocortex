module Recovery where

open import Agda.Builtin.Nat

-- An indexed family: parameters and an index, constructors of three atoms.
data Vec (A : Set) : Nat → Set where
  nil  : Vec A zero
  cons : {n : Nat} → A → Vec A n → Vec A (suc n)

afterVec : Nat
afterVec = zero

-- A right-hand side of three or more atoms.
double : Nat → Nat
double n = n +++ n

unit : Nat
unit = zero

apply : Nat → Nat
apply f = f

-- A type that applies one name to two arguments.
headOf : {A : Set} {n : Nat} → Vec A n → Nat
headOf v = zero

lenOf : Vec Nat zero → Nat
lenOf v = zero

-- A where block with a signature of its own.
triple : Nat → Nat
triple n = go n
  where
    go : Nat → Nat
    go m = m +++ double m

next : Nat
next = zero

-- A let, and a right-hand side on the line after the equals sign.
letIn : Nat → Nat
letIn n = let k = n in k

spread : Nat → Nat
spread n =
  mix n n n

-- A record field on the same line as field.
record Wrapper : Set where
  field unwrap : Nat

record Holder (A : Set) : Set where
  field value : Vec A zero
  field
    extra : Nat

afterRecords : Nat
afterRecords = zero

-- The same forms inside a module.
module Local where
  data Tree (A : Set) : Nat → Set where
    leaf : Tree A zero
    node : {n : Nat} → Tree A n → A → Tree A n → Tree A (suc n)

  size : Nat → Nat
  size n = n +++ n

  walk : Nat → Nat
  walk n = step n
    where
      step : Nat → Nat
      step k = k +++ k

  tail : Nat
  tail = zero

lastOne : Nat
lastOne = zero
