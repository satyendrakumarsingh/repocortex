// Ports owned by this suite, in a file of their own so that local-fixtures.mjs
// can import them without importing ports.mjs back — that cycle, across the
// top-level await ports.mjs uses to load the local file, deadlocks silently.
// 7900/7901 are deliberately avoided: developers
// keep portals open there. `local1`-`local3` are for the machine-local fixtures
// declared in local-fixtures.mjs; `doomed`, `cold` and `refresh` are borrowed
// by one spec each for a portal of its own and released again.
export const PORTS = { payment: 7925, local1: 7920, local2: 7921, local3: 7923, doomed: 7922, cold: 7924, refresh: 7926 };
