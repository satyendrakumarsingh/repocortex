namespace Toolkit.Json

open System
open System.IO
open System.Text

type JsonPickleFormatProvider internal (indent, omitHeader) as self =
    let mutable depth = 0

    member __.CreateWriter (stream: Stream, encoding: Encoding) =
        new StreamWriter(stream, encoding)

    member this.Indent = indent

    member inline this.Depth(delta: int) = depth <- depth + delta

type Reader(path: string) =
    member _.Path = path
    member inline _.Read(count: int) : byte[] = Array.zeroCreate count

module Json =
    let serialize (x: obj) = string x

module Bson =
    let deserialize (b: byte[]) = b.Length
