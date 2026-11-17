# Liquid known gaps

## A capture tag written over several lines

```
{% capture
    footer_text %}Thanks{% endcapture %}
```

The variable is recorded on the line its name is on (the second line here),
not on the line `{% capture` starts. The grammar gives the name as a node of
its own, and the capture statement's own start is not reliable: after another
tag on an earlier line it starts on that line. The name node is the only
position that is correct for every capture, so the name line is used. A
capture on one line, which is the common form, is recorded on that line. The
fixture has only single-line captures for that reason.

## Not definitions (decided, not gaps)

The loop variable of `{% for %}`, the names in `{% include %}` and
`{% render %}`, filters, output expressions, and everything inside a
`{% comment %}` block. An assign inside a loop, an if or a capture is not a
top-level declaration and is not extracted.
