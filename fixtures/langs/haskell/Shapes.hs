{-# LANGUAGE ScopedTypeVariables #-}
module Shapes
  ( Shape(..)
  , area
  , Container(..)
  ) where

import Data.List (sortBy)
import qualified Data.Map as Map

-- | A simple sum type.
data Shape
  = Circle Double
  | Rect Double Double
  deriving (Show, Eq)

data Point = Point { px :: Double, py :: Double }

newtype Wrapper a = Wrapper { unwrap :: a }

type Name = String
type Pair a = (a, a)

class Container f where
  empty :: f a
  insert :: a -> f a -> f a
  toL :: f a -> [a]
  size :: f a -> Int
  size = length . toL

class (Show a) => Pretty a where
  pretty :: a -> String

instance Pretty Shape where
  pretty (Circle r) = "circle " ++ show r
  pretty (Rect w h) = "rect"

instance Container [] where
  empty = []
  insert = (:)
  toL = id

area :: Shape -> Double
area (Circle r) = pi * r * r
area (Rect w h) = w * h

perimeter :: Shape -> Double
perimeter s = go s
  where
    go (Circle r) = 2 * pi * r
    go (Rect w h) = 2 * (w + h)

scale :: Double -> Shape -> Shape
scale k (Circle r) = Circle (k * r)
scale k (Rect w h) = Rect (k * w) (k * h)

unsignedHelper x = x + 1

(|+|) :: Double -> Double -> Double
a |+| b = a + b

infixl 6 |+|

total :: [Shape] -> Double
total = sum . map area

origin = Point 0 0

main :: IO ()
main = do
  let s = Circle 1
      t = scale 2 s
  print (area t)

pair, other :: Int
pair = 1
other = 2

safeDiv
  :: Int
  -> Int
  -> Maybe Int
safeDiv _ 0 = Nothing
safeDiv a b = Just (a `div` b)

data Expr a where
  Lit :: Int -> Expr Int
  Add :: Expr Int -> Expr Int -> Expr Int

type family Elem c
type instance Elem [e] = e

data family Vec a

newtype Age = Age Int deriving Show

data Tree a = Leaf | Node (Tree a) a (Tree a)

x `plus` y = x + y

foreign import ccall "math.h sin" c_sin :: Double -> Double

pattern Zero :: Int
pattern Zero = 0
