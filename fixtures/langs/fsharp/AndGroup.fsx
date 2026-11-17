module Toolkit.Recursive

open System

let rec isEven n =
    if n = 0 then true else isOdd (n - 1)

and isOdd n =
    if n = 0 then false else isEven (n - 1)

and describe (n: int) : string =
    if isEven n then "even" else "odd"

type Shape =
    | Circle of radius: float
    | Rect of w: float * h: float

and Drawing(shapes: Shape list) =
    member _.Shapes = shapes
    member this.Area() = 0.0

let after x = x + 1
