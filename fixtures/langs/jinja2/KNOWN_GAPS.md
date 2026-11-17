# Jinja2 known gaps

## A tag on its own line after another tag

```
{% endmacro %}
{% block sidebar %}{% endblock sidebar %}
```

The grammar has no node for the text between tags, and a tag that follows
another tag is given a node that starts at the end of that previous tag. When
a newline lies between them, the node's start is on the previous line, so the
line the index would record is wrong. The query therefore extracts a macro or
block only when its tag text begins at the tag itself: at the start of a
file, straight after another tag on the same line (`{% endmacro %}{% macro b() %}`),
or after plain text (HTML). A tag in that position on its own line is not
extracted. The fixture separates its tags with HTML lines so that each one is
extracted at the line it is on.

The grammar also wraps some `{% call %}` and other statement regions in error
nodes. A macro or block inside one of those regions is not read as a tag and is
missed.

## Not definitions (decided, not gaps)

`{% extends %}`, `{% include %}`, `{% import %}`, `{% set %}`, `{% for %}`,
`{% call %}`, `endmacro` and `endblock` tags, `{{ NAME() }}` calls, and the
text of a `{# comment #}`.
