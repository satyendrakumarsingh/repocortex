port module Main exposing (Model, Msg(..), main, update, view)

import Browser
import Html exposing (Html, div, text)


type alias Model =
    { count : Int
    , name : String
    }


type alias Pair a =
    ( a, a )


type Msg
    = Increment
    | Decrement
    | Set Int


type Shape a
    = Circle Float
    | Rect Float Float


limit : Int
limit =
    10


init : Model
init =
    { count = 0, name = "x" }


update : Msg -> Model -> Model
update msg model =
    let
        helper n =
            n + 1

        bump =
            helper model.count
    in
    case msg of
        Increment ->
            { model | count = bump }

        Decrement ->
            { model | count = model.count - 1 }

        Set n ->
            { model | count = n }


view : Model -> Html Msg
view model =
    div [] [ text (String.fromInt model.count) ]


noAnnotation x =
    x * 2


pipeTo : a -> (a -> b) -> b
pipeTo x f =
    f x


port sendMessage : String -> Cmd msg


port receive : (String -> msg) -> Sub msg


area : Shape Float -> Float
area shape =
    case shape of
        Circle r ->
            pi * r * r

        Rect w h ->
            w * h


main =
    Browser.sandbox { init = init, update = update, view = view }
