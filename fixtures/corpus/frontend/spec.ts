// A function and, inside its own parameter type, a method signature of the
// same name — both on one line. Identical in every column the symbols UNIQUE
// constraint covers, which aborted an entire 8,094-file index.
async function json<T = any>(res: { ok(): boolean; json(): Promise<any> }, what: string): Promise<T> {
  return (await res.json()) as T
}

export async function fetchBranches(url: string): Promise<unknown> {
  const res = await fetch(url)
  return json(res, 'branches')
}
