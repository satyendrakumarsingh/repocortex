// Everything here is shaped like an outbound call and is not one. A file with
// no client calls must produce nothing at all, or every impact report gains
// dependencies nobody wrote.

// fetch("/api/ghost") — a commented-out call is not a call.
/* axios.get("/api/also-ghost") */

import { useQuery } from "@tanstack/react-query";

export function refetch(id: string) {
  return id;
}

export function useTransactions() {
  // A name ending in fetch is not the fetch API.
  const client = { fetch: (u: string) => u };
  client.fetch("/api/transactions");

  // A cache, a store and a repository all spell themselves like this.
  cache.get("/api/transactions");
  store.post("/api/transactions");

  console.log('called fetch("/api/transactions")');
  return "GET /api/transactions is documented here, not called";
}
