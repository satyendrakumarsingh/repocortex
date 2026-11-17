package parse

// nginx. What the index holds, see fixtures/langs/nginx/expected.tsv:
//
//   - each `server` block that has a server_name is a class named by its
//     first server_name value (the first name after the keyword). A server
//     block with no server_name is not extracted;
//   - each `location` block is a function named by its path as written. The
//     modifier (=, ~, ~*, ^~) is not part of the name;
//   - each `upstream NAME` block is a class named NAME. The grammar does not
//     read the body of an upstream reliably (a `server host:port;` line is an
//     error), so only the name is taken, and the grammar's recovery leaves
//     the rest of the file readable. See KNOWN_GAPS.md.
//
// Directives, if, map, http and events blocks, and comments are not
// definitions.
func init() {
	registerDefinitions("nginx", nginxDefinitions, true)
}

const nginxDefinitions = `
(attribute (keyword) @_upstream . (value) @name
  (#eq? @_upstream "upstream")) @definition.class

(attribute (keyword) @_server
  (block
    (attribute (keyword) @_server_name . (value) @name
      (#eq? @_server_name "server_name")))
  (#eq? @_server "server")) @definition.class

(location (location_route) @name (block)) @definition.function
`
