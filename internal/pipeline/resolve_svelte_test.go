package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestSvelteResolution indexes a small SvelteKit app and checks every call in a
// component's script against what its imports make it reach.
//
// What it is built to catch: `formatMoney` where two modules export one and only
// one is imported; the `$lib` alias; `<script lang="ts">` and
// `<script context="module">`; a template that renders imported components and
// one that renders a component nothing imports; a store module's function; and
// the names Svelte and the browser own (`fetch`, `onMount`, `Math.max`) where a
// module of the repository has the same.
func TestSvelteResolution(t *testing.T) {
	got := langEdges(t, svelteFixture())
	high := model.ConfidenceHigh
	wants := []want{
		{"src/routes/Cart.svelte:checkout", "src/lib/money.ts:formatMoney", high},
		{"src/routes/Cart.svelte:checkout", "src/lib/money.ts:parseMoney", high},
		{"src/routes/Cart.svelte:checkout", "src/lib/tax.js:applyTax", high},
		{"src/routes/Cart.svelte:checkout", "src/lib/stores.ts:loadCart", high},
		{"src/routes/Cart.svelte:checkout", "src/routes/Cart.svelte:validate", high},
		{"src/routes/Cart.svelte:checkout", "src/lib/ledger.ts:Ledger", high},
		{"src/routes/Cart.svelte:checkout", "src/lib/ledger.ts:Ledger.post", high},
		{"src/routes/Cart.svelte:Cart", "src/lib/Badge.svelte:Badge", high},
		{"src/routes/Cart.svelte:Cart", "src/lib/Icon.svelte:Icon", model.ConfidenceMedium},
		{"src/routes/Static.svelte:show", "src/lib/money.ts:formatMoney", high},
		{"src/lib/format.ts:label", "src/lib/Badge.svelte:fmt", high},
	}
	forbid := []want{
		{Src: "src/routes/Cart.svelte", Dst: "src/lib/legacy.ts"},
		{Src: "src/routes/Static.svelte", Dst: "src/lib/legacy.ts"},
		{Src: "src/routes/Cart.svelte", Dst: "src/other/Badge.svelte"},
		{Src: "src/routes/Cart.svelte", Dst: "src/lib/Modal.svelte"},
		{Src: "src/routes/Cart.svelte:checkout", Dst: "src/lib/other.ts"},
	}
	// Rendering from onMount, outside any function, is the component's own call.
	extraOK := []want{{"src/routes/Cart.svelte:Cart", "src/routes/Cart.svelte:checkout", high}}
	scoreEdges(t, got, wants, forbid, extraOK)
}

func svelteFixture() map[string]string {
	return map[string]string{
		"src/lib/money.ts": `export function formatMoney(n: number): string {
  return n.toFixed(2)
}

export function parseMoney(s: string): number {
  return Number(s)
}
`,
		"src/lib/legacy.ts": `export function formatMoney(n: number): string {
  return String(n)
}
`,
		"src/lib/tax.js": `export function applyTax(n) {
  return n * 1.2
}
`,
		"src/lib/stores.ts": `export function loadCart() {
  return []
}
`,
		"src/lib/ledger.ts": `export class Ledger {
  post(n: number): void {}
}
`,
		// Names that Svelte and the platform own.
		"src/lib/other.ts": `export function fetch(u: string) { return u }
export function onMount(f: () => void) { return f }
export function max(a: number, b: number) { return a }
`,
		"src/lib/Badge.svelte": `<script context="module" lang="ts">
  export function fmt(n: number): string {
    return '#' + n
  }
</script>

<span>badge</span>
`,
		"src/lib/Icon.svelte": `<i>icon</i>
`,
		"src/lib/Modal.svelte": `<div>modal</div>
`,
		"src/other/Badge.svelte": `<b>other</b>
`,
		"src/lib/format.ts": `import { fmt } from './Badge.svelte'

export function label(n: number) {
  return fmt(n)
}
`,
		"src/routes/Cart.svelte": `<script lang="ts">
  import { onMount } from 'svelte'
  import { formatMoney, parseMoney } from '$lib/money'
  import { applyTax } from '$lib/tax.js'
  import { loadCart } from '$lib/stores'
  import { Ledger } from '$lib/ledger'
  import Badge from '$lib/Badge.svelte'
  import Glyph from '$lib/Icon.svelte'

  let total = 0

  function validate(n: number): boolean {
    return Math.max(n, 0) > 0
  }

  async function checkout() {
    const items = loadCart()
    const ledger: Ledger = new Ledger()
    ledger.post(1)
    validate(total)
    formatMoney(total)
    parseMoney('3')
    applyTax(2)
    await fetch('/api')
  }

  onMount(() => checkout())
</script>

<Badge />
<Glyph />
<Modal />
<button on:click={checkout}>pay</button>
`,
		"src/routes/Static.svelte": `<script>
  import { formatMoney } from '../lib/money'

  export let n = 0

  function show() {
    return formatMoney(n)
  }
</script>

<p>{show()}</p>
`,
	}
}
