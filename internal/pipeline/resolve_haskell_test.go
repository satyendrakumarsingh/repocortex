package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestHaskellResolution indexes a small cabal library, its executable and an
// Hspec suite and checks every call against what `module` and `import` make it
// reach.
//
// What it is built to catch: `Money.add` where Shop.Other also defines `add` and
// `zero` and is not the module aliased; `import Shop.Util (slug, pad, clamp)`
// (the listed names) against `import Shop.Tax hiding (apply)` (all but one) and
// `import Shop.Cart` (everything); `import qualified Shop.Other as O`; a bare name
// that the file defines itself (`format`) while another module has one that is
// not imported; `sort`, `filter`, `lookup` and `trace`, which Prelude and base
// own and a module nobody imports defines too; a function in backquotes and one
// after `$`; a parameter called `slug` that the function applies, which is not
// Shop.Util's; a `where` binding; and a spec that imports the module under test.
//
// What it does not bind, by design: a function passed as an argument (`map
// total xs`), which without scopes cannot be told from a parameter of the same
// name, and an instance method.
func TestHaskellResolution(t *testing.T) {
	got := langEdges(t, haskellFixture())
	high := model.ConfidenceHigh
	wants := []want{
		// The module's own functions.
		{"src/Shop/Cart.hs:checkout", "src/Shop/Cart.hs:total", high},
		{"src/Shop/Cart.hs:explain", "src/Shop/Cart.hs:format", high},
		{"src/Shop/Tax.hs:apply", "src/Shop/Tax.hs:rate", high},
		// A name an import brings in: all of a module, or the listed ones.
		{"app/Main.hs:main", "src/Shop/Cart.hs:checkout", high},
		{"app/Main.hs:main", "src/Shop/Cart.hs:label", high},
		{"app/Main.hs:main", "src/Shop/Cart.hs:total", high},
		{"app/Main.hs:main", "src/Shop/Tax.hs:apply", high},
		{"src/Shop/Cart.hs:double", "src/Shop/Money.hs:scale", high},
		{"src/Shop/Cart.hs:explain", "src/Shop/Util.hs:clamp", high},
		{"src/Shop/Cart.hs:label", "src/Shop/Money.hs:scale", high},
		{"src/Shop/Cart.hs:label", "src/Shop/Util.hs:pad", high},
		{"src/Shop/Cart.hs:label", "src/Shop/Util.hs:slug", high},
		{"src/Shop/Cart.hs:total", "src/Shop/Tax.hs:rate", high},
		{"test/Shop/CartSpec.hs:spec", "src/Shop/Cart.hs:checkout", high},
		{"test/Shop/CartSpec.hs:spec", "src/Shop/Cart.hs:total", high},
		// A qualified name: written in full or through the alias of an import.
		{"app/Main.hs:main", "src/Shop/Util.hs:format", high},
		{"app/Main.hs:main", "src/Shop/Util.hs:slug", high},
		{"src/Shop/Cart.hs:checkout", "src/Shop/Money.hs:add", high},
		{"src/Shop/Cart.hs:checkout", "src/Shop/Money.hs:zero", high},
		{"src/Shop/Cart.hs:other", "src/Shop/Other.hs:add", high},
		{"src/Shop/Cart.hs:other", "src/Shop/Other.hs:zero", high},
		{"src/Shop/Cart.hs:total", "src/Shop/Money.hs:add", high},
		{"test/Shop/CartSpec.hs:spec", "src/Shop/Money.hs:add", high},
	}
	forbid := []want{
		// Shop.Other is aliased O and is not the module Money names.
		{Src: "src/Shop/Cart.hs:total", Dst: "src/Shop/Other.hs"},
		{Src: "src/Shop/Cart.hs:checkout", Dst: "src/Shop/Other.hs"},
		{Src: "test/", Dst: "src/Shop/Other.hs"},
		// apply is hidden by the import of Shop.Tax in the cart.
		{Src: "src/Shop/Cart.hs", Dst: "src/Shop/Tax.hs:apply"},
		// format is Shop.Util's but the cart imports a list that leaves it out.
		{Src: "src/Shop/Cart.hs", Dst: "src/Shop/Util.hs:format"},
		// A parameter named slug is applied by twice.
		{Src: "src/Shop/Cart.hs:twice", Dst: "src/Shop/Util.hs:slug"},
		// Nothing imports the module whose names are Prelude's.
		{Src: "src/", Dst: "src/Shop/Debug.hs"},
		{Src: "app/", Dst: "src/Shop/Debug.hs"},
		{Src: "test/", Dst: "src/Shop/Debug.hs"},
	}
	scoreEdges(t, got, wants, forbid, nil)
}

func haskellFixture() map[string]string {
	return map[string]string{
		"app/Main.hs": `module Main (main) where

import Shop.Cart
import qualified Shop.Util as U
import Shop.Tax (apply)

main :: IO ()
main = do
  let t = total [1, 2, 3]
  print (checkout [t])
  putStrLn (label t)
  putStrLn $ U.format (apply t)
  putStrLn (U.slug "x")
`,
		"shop.cabal": `name: shop
library
  hs-source-dirs: src
  exposed-modules: Shop.Money, Shop.Cart
`,
		"src/Shop/Cart.hs": `module Shop.Cart (total, checkout, label, explain, twice) where

import Data.List (sortBy)
import qualified Data.Map as M
import qualified Shop.Money as Money
import Shop.Util (slug, pad, clamp)
import Shop.Tax hiding (apply)
import qualified Shop.Other as O
import Shop.Money (scale)

total :: [Int] -> Int
total xs = Money.add (sum xs) (rate (length xs)) + helper xs
  where helper = length

checkout :: [Int] -> Int
checkout c = Money.add z t
  where
    z = Money.zero
    t = total c

label :: Int -> String
label n = pad 3 (slug (show n)) ++ show (scale 2 n)

double :: [Int] -> [Int]
double = map (scale 2) . filter (> 0)

other :: Double
other = O.add 1.0 O.zero

counts :: M.Map Int Int -> Int
counts m = M.size m

-- apply is hidden by the import of Shop.Tax, format is Shop.Util's but not imported:
-- the module's own format is the one called.
format :: Int -> String
format n = show n

explain :: Int -> String
explain n = format (apply n) ++ format (2 ` + "`" + `clamp` + "`" + ` n)

-- slug is a parameter here, and the function applies it: Shop.Util.slug is not called.
twice :: (a -> a) -> a -> a
twice slug n = slug (slug n)
`,
		// Names that Prelude and base own, in a module nobody imports.
		"src/Shop/Debug.hs": `module Shop.Debug (trace, log, sort, filter) where

trace :: a -> a
trace = id

log :: String -> String
log = id

sort :: [a] -> [a]
sort = id

filter :: (a -> Bool) -> [a] -> [a]
filter _ xs = xs
`,
		"src/Shop/Money.hs": `module Shop.Money (zero, add, scale) where

zero :: Int
zero = 0

add :: Int -> Int -> Int
add a b = a + b

scale :: Int -> Int -> Int
scale k n = k * n
`,
		// The same names as Money, in a module the cart aliases and nobody else imports.
		"src/Shop/Other.hs": `module Shop.Other (zero, add, lookup) where

zero :: Double
zero = 0.0

add :: Double -> Double -> Double
add a b = a + b

lookup :: Int -> Int
lookup = id
`,
		"src/Shop/Tax.hs": `module Shop.Tax (rate, apply) where

rate :: Int -> Int
rate c = c ` + "`" + `div` + "`" + ` 5

apply :: Int -> Int
apply c = c + rate c
`,
		"src/Shop/Util.hs": `module Shop.Util (slug, pad, format, clamp) where

slug :: String -> String
slug = id

pad :: Int -> String -> String
pad n s = s

format :: Int -> String
format = show

clamp :: Int -> Int -> Int
clamp lo n = max lo n
`,
		"test/Shop/CartSpec.hs": `module Shop.CartSpec (spec) where

import Test.Hspec
import Shop.Cart
import qualified Shop.Money as Money

spec :: Spec
spec = describe "total" $ do
  it "sums" $ total [1, 2] ` + "`" + `shouldBe` + "`" + ` 3
  it "adds" $ Money.add 1 2 ` + "`" + `shouldBe` + "`" + ` 3
  it "checks out" $ checkout [1] ` + "`" + `shouldBe` + "`" + ` 1
`,
		"test/Spec.hs": `{-# OPTIONS_GHC -F -pgmF hspec-discover #-}
`,
	}
}
