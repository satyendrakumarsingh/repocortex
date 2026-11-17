namespace Toolkit.Builders

open System
open System.Threading.Tasks

/// Computation expression builder over a task of an option.
type TaskOptionBuilderBase() =
    member inline _.Return(value: 'T) : Task<'T option> = Task.FromResult(Some value)

    member inline _.ReturnFrom(taskOption: Task<'T option>) : Task<'T option> = taskOption

    member inline _.Zero() : Task<unit option> = Task.FromResult(Some())

    member inline this.Bind(task: Task<'T option>, binder: 'T -> Task<'U option>) : Task<'U option> =
        task.ContinueWith(fun (t: Task<'T option>) ->
            match t.Result with
            | Some v -> binder v
            | None -> Task.FromResult None)
        |> Task.Unwrap

    member inline _.Delay([<InlineIfLambda>] generator: unit -> Task<'T option>) : unit -> Task<'T option> = generator

    member _.Run(f: unit -> Task<'T option>) : Task<'T option> = f ()

    static member inline Combine(a: int, b: int) : int = a + b

    member val Name = "builder" with get, set

type Outcome<'T> =
    | Success of 'T
    | Failure of string

exception BuilderError of string

module TaskOptionBuilder =
    let taskOption = TaskOptionBuilderBase()

    let inline map ([<InlineIfLambda>] f: 'a -> 'b) (task: Task<'a option>) : Task<'b option> =
        task.ContinueWith(fun (t: Task<'a option>) -> Option.map f t.Result)

    let private unwrap (o: Outcome<'T>) : 'T option =
        match o with
        | Success v -> Some v
        | Failure _ -> None

module Validation =
    let ofOption err o =
        match o with
        | Some v -> Success v
        | None -> Failure err
