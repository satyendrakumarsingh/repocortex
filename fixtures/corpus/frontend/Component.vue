<script setup lang="ts">
// A single-file component. Its own grammar leaves this script opaque, so 514
// files like it were skipped entirely until the SFC path existed.
import { ref, computed } from 'vue'

export interface BranchDetail {
  id: string
  label: string
}

const rows = ref<BranchDetail[]>([])
const total = computed(() => rows.value.length)

function loadBranches(source: string): BranchDetail[] {
  return rows.value.filter(r => r.id === source)
}
</script>

<template>
  <!-- `truncate` is a Tailwind utility and TRUNCATE is a SQL verb. This line
       once put a table named `flex-1` in the graph. -->
  <span class="truncate flex-1">{{ total }}</span>
  <span class="truncate font-medium">{{ rows.length }}</span>
</template>
