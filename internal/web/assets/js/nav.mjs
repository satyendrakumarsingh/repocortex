/*
  THE RAIL, AS DATA.

  One list, organized into 4 clear architectural pillars:
  1. Architecture & Surface (Overview, Visual Map, APIs, Tables, Events, Config, Trace, Estate)
  2. Change Safety & Impact (Plan Refactor, Blast Radius, Task Brief, Verify, Fact Diff)
  3. Code Intelligence & Quality (Find Symbol, Blindspots, Tests, Metrics, Governance, Deps, Accuracy, Ownership)
  4. AI & Agent Integration (Connect Agent/MCP, Retrieval Quality)
*/
export const GROUPS = [
  { title: '', screens: [
    { id: 'overview', label: 'Index health' },
  ] },
  { title: 'Understand the code', screens: [
    { id: 'graph', label: 'Map of the code' },
    { id: 'search', label: 'Find a symbol' },
    { id: 'metrics', label: 'Shape of the code' },
  ] },
  { title: 'Before you change something', screens: [
    { id: 'planchange', label: 'Plan a refactor' },
    { id: 'blast', label: 'What could break' },
    { id: 'contract', label: 'Task brief' },
    { id: 'verify', label: 'Check a change' },
    { id: 'ownership', label: 'Who knows this code' },
    { id: 'diff', label: 'What changed' },
  ] },
  { title: 'How it runs', screens: [
    { id: 'crossstack', label: 'APIs and what they touch' },
    { id: 'tables', label: 'Database tables' },
    { id: 'events', label: 'Events and queues' },
    { id: 'config', label: 'Settings and secrets' },
    { id: 'trace', label: 'What actually ran' },
    { id: 'estate', label: 'Across repositories' },
  ] },
  { title: 'Quality', screens: [
    { id: 'tests', label: 'Test coverage' },
    { id: 'missed', label: 'Code blindspots' },
    { id: 'governance', label: 'Architecture rules' },
    { id: 'deps', label: 'Dependencies' },
    { id: 'accuracy', label: 'How accurate is this index' },
  ] },
  { title: 'For your AI agent', screens: [
    { id: 'agent', label: 'Connect an agent' },
    { id: 'retrieval', label: 'Retrieval quality' },
  ] },
];

/* The label a screen id renders as, for document.title and answer lines. */
export const LABEL = Object.fromEntries(GROUPS.flatMap((g) => g.screens.map((s) => [s.id, s.label])));

/*
  Builds the rail's buttons INTO an existing <nav>, between .rail-brand and
  .rail-foot. Every entry is a real <button> so keyboard Tab reaches all in order.
*/
export function renderRail(nav, active) {
  const foot = nav.querySelector('.rail-foot');
  for (const old of nav.querySelectorAll('.rail-label, .nav')) old.remove();
  for (const group of GROUPS) {
    if (group.title) {
      const label = document.createElement('div');
      label.className = 'rail-label';
      label.textContent = group.title;
      nav.insertBefore(label, foot);
    }
    for (const s of group.screens) {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'nav';
      b.dataset.screen = s.id;
      b.textContent = s.label;
      if (s.id === active) b.setAttribute('aria-current', 'page');
      nav.insertBefore(b, foot);
    }
  }
}
