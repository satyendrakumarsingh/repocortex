# awk definitions. Rule:
#  - `function NAME(params)` and the `func NAME(params)` spelling are functions, wherever they are written;
#  - a multi-line parameter list does not change the line, which is the line the name is on;
#  - BEGIN and END blocks, pattern-action rules, calls, variables (global or in a parameter list) and comments are not definitions.

BEGIN {
    FS = ","
    total = 0
}

function add_row(a, b,    local_sum) {
    local_sum = a + b
    return local_sum
}

func format_money(amount) {
    return sprintf("%.2f", amount)
}

function parse_line(line,
                    fields, n) {
    n = split(line, fields, FS)
    return n
}

# function commented_out(x) { return x }

/^total/ { total = add_row(total, 1) }

END {
    print format_money(total)
}
