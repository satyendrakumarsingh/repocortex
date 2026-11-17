# nginx known gaps

## The body of an upstream block

```
upstream backend_pool {
    server 10.0.0.1:8080;
}
```

The name is extracted. The grammar cannot read the body: a `server host:port;`
line becomes a `numeric_literal` and `cpumask` error, so the servers of the
pool are not read and no definition is taken from inside the block. The
rest of the file is still read, as the fixture shows.

## A server block with more than one server_name

```
server {
    server_name first.example.com;
    listen 80;
    server_name second.example.com;
}
```

The rule names a server by its first server_name. The query matches each
`server_name` directive, and the grammar gives no way to tell the first from a
later one inside a block, so this block yields two classes on its own line,
`first.example.com` and `second.example.com`. The second is not the rule's
name. The fixture has no such block for that reason; a block with one
server_name is extracted once, by that name.

## Not definitions (decided, not gaps)

`if`, `map`, `http` and `events` blocks, directives such as `listen` and
`proxy_pass`, and a `server` block with no server_name. A location inside an
unnamed server block is still a function.
