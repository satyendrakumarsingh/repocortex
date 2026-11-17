#lang racket

(require racket/list
         racket/string)

(provide area
         (struct-out point))

(module+ test
  (require rackunit)
  (define test-helper 1)
  (check-equal? (area 1) 3))

(module util racket/base
  (provide twice)
  (define (twice x) (* 2 x)))

(define pi-approx 3.14159)

(define unit-name "unit")

(define (area r)
  (define squared (* r r))
  (* pi-approx squared))

(define (perimeter r) (* 2 pi-approx r))

(define ((curried a) b)
  (+ a b))

(define (variadic first . rest)
  (cons first rest))

;; keyword and optional arguments
(define (scaled-area r #:factor [factor 1] [extra 0])
  (* factor (area r) extra))

(define-inline (fast-add a b) (+ a b))

(define square (lambda (x) (* x x)))

(define cube
  (λ (x) (* x x x)))

(define-values (left right) (values 1 2))

(define-syntax-rule (swap! a b)
  (let ([tmp a])
    (set! a b)
    (set! b tmp)))

(define-syntax my-unless
  (syntax-rules ()
    [(_ c body ...) (if c (void) (begin body ...))]))

(define-syntax (my-when stx)
  (syntax-case stx ()
    [(_ c body ...) #'(if c (begin body ...) (void))]))

(struct point (x y))

(struct point3d point (z) #:transparent)

(struct shape ([name #:mutable] sides))

(define-struct segment (start end))

(define-struct (arc segment) (radius))

(define-logger shapes)

(define/contract (checked n)
  (-> integer? integer?)
  (add1 n))

(define-generics drawable
  (draw drawable))

(define my-class%
  (class object%
    (super-new)
    (define/public (greet) "hi")
    (define/private (secret) 1)
    (define field-value 3)))

(define (outer)
  (define (inner) 1)
  (define local-value 2)
  (inner))

(let ([hidden 1])
  (define in-let 2)
  hidden)

(when #t
  (define in-when 3))

(for ([i 3])
  (displayln (area i)))

(area 2)
