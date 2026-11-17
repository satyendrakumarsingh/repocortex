:- module(shapes, [area/2, perimeter/2, shape/1]).

:- use_module(library(lists)).
:- dynamic counter/1.
:- discontiguous colour/2.

% Facts.
shape(circle).
shape(square).
shape(rectangle).

colour(circle, red).
colour(square, blue).

counter(0).

pi_approx(3.14159).

% A rule of several clauses.
area(circle(R), A) :-
    pi_approx(Pi),
    A is Pi * R * R.
area(square(S), A) :-
    A is S * S.
area(rect(W, H), A) :-
    A is W * H.

perimeter(square(S), P) :- P is 4 * S.

% Same name, different arity.
describe(X) :- shape(X).
describe(X, Y) :- colour(X, Y).

% Definite clause grammar.
greeting --> [hello], name.
name --> [world].
name --> [prolog].

% Zero-arity predicates.
halt_check :- fail.
ready.

% Not definitions: directives and goals in bodies.
:- initialization(main).

main :-
    forall(shape(S), (area(S, A), format("~w ~w~n", [S, A]))),
    halt_check.

% A clause after a clause of another predicate, then the first again.
alpha(1).
beta(1).
alpha(2).
