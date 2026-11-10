import {
  test, expect, cold, settle, assertClean, assertNoPlaceholders, bodyText, horizontalOverflow,
} from '../support/fixtures.mjs';
import { REPOS, base } from '../support/ports.mjs';

/*
  THE CODE MAP'S TREE, TREEMAP AND CODE VIEWS.

  This file replaced the card browser's spec when the code map replaced the card
  browser (js/codemap.mjs). What it holds the three views to:

  1. THEY COUNT WHAT THE API COUNTS. The tree lists every directory the
     containers rung returns, the treemap draws one box per directory, and the
     Code view shows exactly the lines /api/source sent — compared against the
     payload, not against a number written into this file.
  2. ONE SELECTION, FOUR VIEWS. A symbol chosen in the tree is the symbol the
     map selects and the declaration the Code view highlights. That is the
     reason the views share an address, so it is asserted end to end.
  3. KEYBOARD FIRST. The tabs are a tablist and the tree a tree: arrows move,
     Enter acts, Esc goes back up.
  4. NOTHING FAILS SILENTLY. A path the index does not hold, and a card-browser
     link from before the redesign, each land on something that says what
     happened.
*/

const tabs = (page) => page.locator('#graph-view-switch [role=tab]');
const treeRows = (page) => page.locator('#graph-tree-host [role=treeitem]');
const panelTitle = (page, host) => page.locator(`${host} .cm-panel h3`);

async function apiJSON(page, repo, path) {
  const res = await page.request.get(`${base(repo)}${path}`, { headers: { Accept: 'application/json' } });
  expect(res.ok(), `${path} answered ${res.status()}`).toBeTruthy();
  return res.json();
}

/* The first symbol the API can show with its source: a file with a symbol. */
async function firstSymbolWithSource(page, repo) {
  const roots = await apiJSON(page, repo, '/api/graph?level=containers&nodes=512');
  for (const c of roots.containers || []) {
    const s = await apiJSON(page, repo, `/api/graph?level=symbols&container=${encodeURIComponent(c.id)}`);
    const sym = (s.nodes || []).find((n) => !n.external && String(n.container) === String(c.id) && n.line > 0);
    if (!sym) continue;
    const src = await apiJSON(page, repo, `/api/source?path=${encodeURIComponent(sym.path)}`);
    if ((src.lines || []).length) return { container: String(c.id), sym, src };
  }
  return null;
}

for (const key of Object.keys(REPOS)) {
  const repo = REPOS[key];

  test.describe(`${repo.name} — code map views`, () => {
    test('the tabs are a tablist: one tab stop, arrows move, the chosen view is in the URL', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      await expect(tabs(page)).toHaveCount(4);
      await expect(page.locator('#graph-view-switch [role=tab][aria-selected=true]')).toHaveText(/Graph/);
      expect(await page.locator('#graph-view-switch [role=tab][tabindex="0"]').count(), 'exactly one tab is a tab stop').toBe(1);

      await page.locator('#graph-view-switch [role=tab][aria-selected=true]').focus();
      await page.keyboard.press('ArrowRight');
      await expect(page.locator('#graph-view-switch [data-view=tree]')).toBeFocused();
      await page.keyboard.press('Enter');
      await expect(page.locator('#graph-tree-host')).toBeVisible();
      expect(page.url()).toContain('gv=tree');
      assertClean(page, 'tabs');
    });

    test('the tree lists every directory the API returns, and opens to files and symbols', async ({ page }) => {
      const roots = await apiJSON(page, repo, '/api/graph?level=containers&nodes=512');
      await cold(page, repo, '?s=graph&gv=tree');
      await expect(page.locator('#cm-tree-note')).toContainText(`${(roots.containers || []).length.toLocaleString()} director`);

      // The keyboard model: Home, Down, Right opens, Left closes.
      await treeRows(page).first().focus();
      await page.keyboard.press('ArrowDown');
      const second = page.locator('#graph-tree-host [role=treeitem]:focus');
      await expect(second).toHaveCount(1);

      const target = await firstSymbolWithSource(page, repo);
      test.skip(!target, 'no symbol with indexed source in this fixture');
      await cold(page, repo, `?s=graph&gv=tree&container=${encodeURIComponent(target.container)}&level=symbols&sel=L:${target.sym.id}`);
      const row = page.locator(`#graph-tree-host [data-key="s:${target.sym.id}"]`);
      await expect(row, 'the selection is revealed: its directory and file are open').toBeVisible({ timeout: 20_000 });
      await expect(row).toHaveAttribute('aria-selected', 'true');
      await expect(panelTitle(page, '#graph-tree-host')).toHaveText(target.sym.name);
      await expect(page.locator('#graph-tree-host .cm-panel')).toContainText(`${target.sym.path}:${target.sym.line}`);
      assertNoPlaceholders(await bodyText(page), 'tree');
      assertClean(page, 'tree');
    });

    test('the tree filter states its match count and highlights the matches', async ({ page }) => {
      const roots = await apiJSON(page, repo, '/api/graph?level=containers&nodes=512');
      const needle = String((roots.containers || [])[0].id).split('/').pop().slice(0, 3);
      await cold(page, repo, '?s=graph&gv=tree');
      await page.locator('#graph-tree-host input[type=search]').fill(needle);
      await expect(page.locator('#graph-tree-host .cm-tools [aria-live]')).toHaveText(/\d+ match/);
      await expect(page.locator('#graph-tree-host mark').first()).toBeVisible();
    });

    test('one selection carries from the tree to the graph and to the code', async ({ page }) => {
      const target = await firstSymbolWithSource(page, repo);
      test.skip(!target, 'no symbol with indexed source in this fixture');
      await cold(page, repo, `?s=graph&gv=tree&container=${encodeURIComponent(target.container)}&level=symbols&sel=L:${target.sym.id}`);
      await expect(panelTitle(page, '#graph-tree-host')).toHaveText(target.sym.name, { timeout: 20_000 });

      await page.locator('#graph-view-switch [data-view=map]').click();
      await settle(page, 45_000);
      await expect(page.locator('#graph-inspector'), 'the map selected the same symbol').toContainText(target.sym.name, { timeout: 30_000 });

      await page.locator('#graph-view-switch [data-view=code]').click();
      // From the map the Code tab has no file of its own yet; the inspector's
      // action carries one. Either way the same declaration must be current.
      if (await page.locator('#graph-code-host .cm-empty').count()) {
        await page.locator('#graph-view-switch [data-view=map]').click();
        await page.locator('#act-open-code').click();
      }
      await expect(page.locator('#graph-code-host .cm-outline [aria-current=true]')).toContainText(target.sym.name, { timeout: 20_000 });
      assertClean(page, 'tree → graph → code');
    });

    test('the treemap draws one box per directory, goes inside on Enter and back out on Esc', async ({ page }) => {
      const roots = await apiJSON(page, repo, '/api/graph?level=containers&nodes=512');
      await cold(page, repo, '?s=graph&gv=treemap');
      await expect(page.locator('#graph-treemap-host .cm-cell')).toHaveCount((roots.containers || []).length);
      const first = page.locator('#graph-treemap-host .cm-cell[tabindex="0"]');
      await first.focus();
      await page.keyboard.press('Enter');
      await expect.poll(() => new URL(page.url()).searchParams.get('container'), { message: 'Enter opened a directory' }).toBeTruthy();
      await expect(page.locator('#graph-treemap-host .cm-cell').first()).toBeVisible();
      await page.locator('#graph-treemap-host .cm-cell[tabindex="0"]').focus();
      await page.keyboard.press('Escape');
      await expect.poll(() => new URL(page.url()).searchParams.get('container'), { message: 'Esc went back up' }).toBeFalsy();
      assertClean(page, 'treemap');
    });

    test('the code view shows exactly the lines /api/source sent, with its declarations', async ({ page }) => {
      const target = await firstSymbolWithSource(page, repo);
      test.skip(!target, 'no symbol with indexed source in this fixture');
      await cold(page, repo, `?s=graph&gv=code&cfile=${encodeURIComponent(target.sym.path)}`);
      await expect(page.locator('#graph-code-host .cm-ln')).toHaveCount(target.src.lines.length);
      const firstLine = await page.locator('#graph-code-host .cm-ln .cm-no').first().innerText();
      expect(Number(firstLine)).toBe(target.src.from);
      const top = (target.src.declarations || []).filter((d) => !/^(variable|field|parameter|local|property)$/.test(d.kind));
      await expect(page.locator('#graph-code-host .cm-outline > button')).toHaveCount(top.length);
      // The source pane is excluded: a repository's own code may legitimately
      // say `undefined` or `NaN`. Everything the portal itself wrote is checked.
      const chrome = await page.locator('#graph-code-host').evaluate((host) => {
        const clone = host.cloneNode(true);
        clone.querySelectorAll('.cm-src').forEach((n) => n.remove());
        return clone.textContent;
      });
      assertNoPlaceholders(chrome, 'code');
      assertClean(page, 'code');
    });

    test('a path the index does not hold says so, rather than an empty file', async ({ page }) => {
      await cold(page, repo, '?s=graph&gv=code&cfile=' + encodeURIComponent('no/such/file.go'));
      await expect(page.locator('#graph-code-host .cm-empty h3')).toContainText('is not a file in this index');
      assertClean(page, 'code: missing path');
    });

    test('a card-browser link from before the redesign lands on the same directory', async ({ page }) => {
      const roots = await apiJSON(page, repo, '/api/graph?level=containers&nodes=512');
      const dc = String((roots.containers || [])[0].id);
      await cold(page, repo, `?s=graph&gv=browse&dc=${encodeURIComponent(dc)}`);
      await expect(page.locator('#graph-treemap-host')).toBeVisible();
      const u = new URL(page.url());
      expect(u.searchParams.get('gv')).toBe('treemap');
      expect(u.searchParams.get('container')).toBe(dc);
      expect(u.searchParams.get('dc'), 'the retired address is cleared').toBeNull();
    });

    test('no view scrolls sideways on a phone', async ({ page }) => {
      await page.setViewportSize({ width: 375, height: 812 });
      for (const gv of ['map', 'tree', 'treemap', 'code']) {
        await cold(page, repo, `?s=graph&gv=${gv}`);
        const o = await horizontalOverflow(page);
        expect(o.culprits, `${gv} overflows at 375px (${o.scrollWidth} > ${o.clientWidth})`).toEqual([]);
      }
    });
  });
}
