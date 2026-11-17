module Shapes
  ( Shape(..)
  , area
  , class Container
  ) where

import Prelude
import Data.Array (length)
import Data.Maybe (Maybe(..))

foreign import data Handle :: Type

foreign import sqrtImpl :: Number -> Number

foreign import now :: Effect Number

data Shape
  = Circle Number
  | Rect Number Number

data Color = Red | Green | Blue

newtype Wrapper a = Wrapper { unwrap :: a }

newtype Name = Name String

type Point = { px :: Number, py :: Number }

type Pair a = Tuple a a

class Container f where
  empty :: forall a. f a
  insert :: forall a. a -> f a -> f a
  size :: forall a. f a -> Int

class Show a <= Pretty a where
  pretty :: a -> String

instance prettyShape :: Pretty Shape where
  pretty (Circle r) = "circle"
  pretty (Rect w h) = "rect"

derive instance eqShape :: Eq Shape

area :: Shape -> Number
area (Circle r) = pi * r * r
area (Rect w h) = w * h

perimeter :: Shape -> Number
perimeter s = go s
  where
  go (Circle r) = 2.0 * pi * r
  go (Rect w h) = 2.0 * (w + h)

scale :: Number -> Shape -> Shape
scale k (Circle r) = Circle (k * r)
scale k (Rect w h) = Rect (k * w) (k * h)

unsignedHelper x = x + 1

addPoints :: Point -> Point -> Point
addPoints a b = { px: a.px + b.px, py: a.py + b.py }

infixl 6 addPoints as |+|

total :: Array Shape -> Number
total = sum <<< map area

origin :: Point
origin = { px: 0.0, py: 0.0 }

withDefault :: forall a. a -> Maybe a -> a
withDefault d Nothing = d
withDefault _ (Just x) = x

main :: Effect Unit
main = do
  let s = Circle 1.0
      t = scale 2.0 s
  log (show (area t))
