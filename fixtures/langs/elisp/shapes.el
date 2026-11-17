;;; shapes.el --- Shape helpers  -*- lexical-binding: t; -*-

;;; Commentary:
;; A small library.

(require 'cl-lib)
(require 'subr-x)

(provide 'shapes)

(defvar shapes-default-sides 4
  "Sides of a default shape.")

(defvar shapes-unbound)

(defconst shapes-pi 3.14159
  "Pi, roughly.")

(defcustom shapes-scale 1.0
  "Scale factor."
  :type 'number
  :group 'shapes)

(defgroup shapes nil
  "Shapes customisation."
  :group 'tools)

(defface shapes-highlight
  '((t :weight bold))
  "Face for highlights.")

(defvar-local shapes-buffer-state nil)

(defun shapes-area (r)
  "Area of a circle of radius R."
  (let ((squared (* r r)))
    (* shapes-pi squared)))

(defun shapes--private-helper (x &optional y &rest more)
  (+ x (or y 0) (length more)))

(defun shapes-interactive (n)
  (interactive "p")
  (message "%d" n))

(defmacro shapes-with-scale (scale &rest body)
  (declare (indent 1))
  `(let ((shapes-scale ,scale))
     ,@body))

(defsubst shapes-fast (x)
  (* x 2))

(define-inline shapes-inline (x)
  (inline-quote (+ ,x 1)))

(defalias 'shapes-alias #'shapes-area)

(cl-defun shapes-keyword (a &key (b 1) c)
  (list a b c))

(cl-defmacro shapes-cl-macro (x)
  `(list ,x))

(cl-defgeneric shapes-describe (shape)
  "Describe SHAPE.")

(cl-defmethod shapes-describe ((s shapes-circle))
  (format "circle %s" (shapes-circle-radius s)))

(cl-defstruct shapes-circle radius)

(cl-defstruct (shapes-rect (:constructor shapes-make-rect))
  width height)

(cl-defstruct shapes-box
  "A box."
  (depth 0))

(defclass shapes-base ()
  ((name :initarg :name))
  "Base class.")

(define-minor-mode shapes-mode
  "Minor mode for shapes."
  :lighter " Sh")

(define-derived-mode shapes-view-mode special-mode "Shapes"
  "Major mode for viewing shapes.")

(define-globalized-minor-mode global-shapes-mode shapes-mode shapes--turn-on)

(define-error 'shapes-error "Shapes failed")

;; character literals and quoting that have confused readers
(defun shapes-chars ()
  "Return (a list) of \"quoted\" characters."
  (list ?\( ?\) ?a ?\s '(1 . 2) [1 2] #'car))

(cl-defun shapes--outer ()
  (defun shapes--nested () 1)
  (defvar shapes-local 2)
  (let ((f (lambda (x) x)))
    (funcall f 1)))

(when (boundp 'shapes-flag)
  (defun shapes-conditional () 1))

(eval-and-compile
  (defun shapes-compiled () 1))

(shapes-area 2)
(setq shapes-set 5)
(add-hook 'shapes-hook #'shapes-area)

;;; shapes.el ends here
