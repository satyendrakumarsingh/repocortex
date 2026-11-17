package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestVueResolution indexes a small Vue 3 frontend and checks every call in a
// component's script against what its imports make it reach.
//
// What it is built to catch: `formatMoney` where two modules export one and only
// one is imported; a renamed import; a default import of a sibling component;
// `<script setup lang="ts">` and a plain options-API `<script>`; a call to a
// browser or Vue global (`ref`, `fetch`, `Math.max`) that shares a name with a
// repository function; a method of a module the file does not import; and a
// `.ts` test that imports a component's script.
func TestVueResolution(t *testing.T) {
	got := langEdges(t, vueFixture())
	high := model.ConfidenceHigh
	wants := []want{
		{"src/components/CartView.vue:checkout", "src/lib/money.ts:formatMoney", high},
		{"src/components/CartView.vue:checkout", "src/lib/tax.ts:applyTax", high},
		{"src/components/CartView.vue:checkout", "src/lib/money.ts:parseMoney", high},
		{"src/components/CartView.vue:checkout", "src/stores/cart.ts:useCart", high},
		{"src/components/CartView.vue:checkout", "src/components/CartView.vue:validate", high},
		{"src/components/LegacyList.vue:render", "src/lib/money.ts:formatMoney", high},
		// A typed receiver and a construction.
		{"src/components/CartView.vue:checkout", "src/lib/ledger.ts:Ledger", high},
		{"src/components/CartView.vue:checkout", "src/lib/ledger.ts:Ledger.post", high},
		// A template renders imported components: by tag, kebab-case, and under a
		// local name other than the component's own.
		{"src/components/CartView.vue:CartView", "src/components/Badge.vue:Badge", high},
		{"src/components/CartView.vue:CartView", "src/components/PriceTag.vue:PriceTag", high},
		{"src/components/CartView.vue:CartView", "src/components/Chip.vue:Chip", high},
		// A function a component's script exports, imported by a .ts module.
		{"src/composables/useTotals.ts:useTotals", "src/components/PriceTag.vue:fmt", high},
	}
	forbid := []want{
		{Src: "src/components/CartView.vue", Dst: "src/lib/legacy.ts"},
		{Src: "src/components/LegacyList.vue", Dst: "src/lib/legacy.ts"},
		{Src: "src/components/CartView.vue", Dst: "src/lib/unused.ts"},
		// A component of the same name that the file did not import, and one that
		// no import binds (a global or a router component).
		{Src: "src/components/CartView.vue", Dst: "src/other/Badge.vue"},
		{Src: "src/components/CartView.vue", Dst: "src/components/RouterLink.vue"},
		{Src: "src/components/CartView.vue", Dst: "src/lib/legacy.ts:Ledger"},
		{Src: "src/components/CartView.vue:checkout", Dst: "src/lib/other.ts:ref"},
		{Src: "src/components/CartView.vue:checkout", Dst: "src/lib/other.ts:fetch"},
		{Src: "src/components/CartView.vue:checkout", Dst: "src/lib/other.ts:max"},
	}
	scoreEdges(t, got, wants, forbid, nil)
}

func vueFixture() map[string]string {
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
		"src/lib/tax.ts": `export function applyTax(n: number): number {
  return n * 1.2
}
`,
		"src/lib/unused.ts": `export function applyTax(n: number): number {
  return n
}
`,
		"src/lib/other.ts": `export function ref(x: number) { return x }
export function fetch(u: string) { return u }
export function max(a: number, b: number) { return a }
`,
		"src/lib/ledger.ts": `export class Ledger {
  post(n: number): void {}
}
`,
		"src/other/Badge.vue": `<template><i>other</i></template>
`,
		"src/components/RouterLink.vue": `<template><a /></template>
`,
		"src/components/Chip.vue": `<template><b>chip</b></template>
`,
		"src/components/PriceTag.vue": `<script lang="ts">
export function fmt(n: number): string {
  return '$' + n
}
</script>

<template><span /></template>
`,
		"src/composables/useTotals.ts": `import { fmt } from '../components/PriceTag.vue'

export function useTotals(n: number) {
  return fmt(n)
}
`,
		"src/stores/cart.ts": `export function useCart() {
  return { items: [] as number[] }
}
`,
		"src/components/Badge.vue": `<template><span>badge</span></template>
`,
		"src/components/CartView.vue": `<script setup lang="ts">
import { ref } from 'vue'
import { formatMoney, parseMoney as parse } from '../lib/money'
import applyTax from '../lib/tax'
import * as tax from '../lib/tax'
import Badge from './Badge.vue'
import PriceTag from './PriceTag.vue'
import Pill from './Chip.vue'
import { Ledger } from '../lib/ledger'
import { useCart } from '@/stores/cart'

const total = ref(0)

function validate(n: number): boolean {
  return Math.max(n, 0) > 0
}

async function checkout() {
  const cart = useCart()
  const ledger: Ledger = new Ledger()
  ledger.post(1)
  validate(total.value)
  formatMoney(total.value)
  parse('3')
  tax.applyTax(1)
  await fetch('/api')
}
</script>

<template>
  <Badge />
  <Pill />
  <price-tag :n="total" />
  <RouterLink to="/" />
  <button>x</button>
  <button @click="checkout">pay</button>
</template>
`,
		"src/components/LegacyList.vue": `<script>
import { formatMoney } from '../lib/money'

export default {
  methods: {
    render(n) {
      return formatMoney(n)
    },
  },
}
</script>

<template><div /></template>
`,
	}
}
