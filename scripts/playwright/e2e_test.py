import os
import sys
import time
import subprocess
import signal
from playwright.sync_api import sync_playwright

SCREENSHOTS_DIR = "/tmp/repocortex_e2e_screenshots"
os.makedirs(SCREENSHOTS_DIR, exist_ok=True)

PROJECTS_TO_TEST = [
    {
        "name": "autosettlement",
        "path": "/Users/satyendra/Documents/DEVWORKSPACE/autosettlement",
        "test_symbol": "KafkaConsumers",
        "has_events": True,
        "has_tables": True,
    },
    {
        "name": "modern-callback-framework",
        "path": "/Users/satyendra/Documents/DEVWORKSPACE/modern-callback-framework",
        "test_symbol": "CallbackService",
        "has_events": False,
        "has_tables": True,
    },
    {
        "name": "pay-core",
        "path": "/Users/satyendra/Documents/DEVWORKSPACE/pay-core",
        "test_symbol": "IsoFieldType",
        "has_events": False,
        "has_tables": False,
    },
    {
        "name": "rki-console",
        "path": "/Users/satyendra/Documents/DEVWORKSPACE/rki-console",
        "test_symbol": "App",
        "has_events": False,
        "has_tables": False,
    }
]

def run_project_e2e(p, binary_path, project_info, port=7788):
    proj_name = project_info["name"]
    proj_path = project_info["path"]
    print(f"\n=======================================================")
    print(f"Testing RepoCortex UX on project: {proj_name} ({proj_path})")
    print(f"=======================================================")

    # Start repocortex ui
    cmd = [binary_path, "ui", "--port", str(port), "--project", proj_path]
    proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    
    base_url = f"http://127.0.0.1:{port}"
    
    # Wait for server to bind and answer health check
    import urllib.request
    ready = False
    for _ in range(50):
        if proc.poll() is not None:
            break
        try:
            with urllib.request.urlopen(f"{base_url}/api/health", timeout=1.0) as resp:
                if resp.status == 200:
                    ready = True
                    break
        except Exception:
            time.sleep(0.1)

    if not ready or proc.poll() is not None:
        out, err = proc.communicate(timeout=2.0) if proc.poll() is not None else ("", "")
        print(f"Failed to start repocortex ui for {proj_name}: {out}\n{err}")
        return False, ["Failed to start repocortex ui server"]

    errors = []
    warnings = []

    try:
        browser = p.chromium.launch(headless=True)
        context = browser.new_context(viewport={"width": 1440, "height": 900})
        page = context.new_page()

        # Capture console and page errors
        def on_console(msg):
            if msg.type in ("error",):
                errors.append(f"Console error: {msg.text}")
            elif msg.type == "warning":
                warnings.append(f"Console warning: {msg.text}")

        def on_page_error(err):
            errors.append(f"Page unhandled exception: {err}")

        def on_request_failed(req):
            errors.append(f"Network request failed: {req.url} - {req.failure}")

        def on_response(res):
            if res.status >= 400 and not "/api/planchange" in res.url:
                errors.append(f"HTTP error {res.status}: {res.url}")

        page.on("console", on_console)
        page.on("pageerror", on_page_error)
        page.on("requestfailed", on_request_failed)
        page.on("response", on_response)

        # 1. Load Overview Screen
        print("[Step 1] Loading Overview screen...")
        page.goto(f"{base_url}/?s=overview", wait_until="networkidle")
        page.wait_for_selector("#screen-overview", state="visible", timeout=10000)
        
        # Verify title & indexbar
        title = page.title()
        print(f"  Page title: {title}")
        project_badge = page.locator("#project").inner_text()
        print(f"  Project badge: {project_badge}")
        assert proj_name.lower() in project_badge.lower() or "repocortex" in project_badge.lower()

        # Check KPIs
        kpi_count = page.locator(".kpi").count()
        print(f"  KPIs rendered: {kpi_count}")
        assert kpi_count > 0, "Expected at least one KPI on overview screen"
        
        page.screenshot(path=f"{SCREENSHOTS_DIR}/{proj_name}_01_overview.png")

        # 2. Test Confidence Tier Filter Bar
        print("[Step 2] Testing Confidence Tier filter buttons...")
        tier_buttons = page.locator(".tierbar button")
        btn_count = tier_buttons.count()
        print(f"  Tier buttons found: {btn_count}")
        if btn_count > 0:
            # Click the second tier button
            tier_buttons.nth(1).click()
            time.sleep(0.3)
            # Click the first tier button
            tier_buttons.nth(0).click()
            time.sleep(0.3)

        # 3. Test Events Screen
        print("[Step 3] Testing Events Topology screen...")
        page.locator("button.nav[data-screen='events']").click()
        page.wait_for_selector("#screen-events", state="visible", timeout=10000)
        time.sleep(0.5)
        page.screenshot(path=f"{SCREENSHOTS_DIR}/{proj_name}_02_events.png")

        if project_info["has_events"]:
            topic_rows = page.locator("#screen-events table tbody tr")
            row_count = topic_rows.count()
            print(f"  Event topic rows found: {row_count}")
            assert row_count > 0, "Expected discovered event topics in autosettlement"
            
            # Click first topic row to open inspector drawer
            print("  Clicking first topic to test 360° Inspector Drawer...")
            topic_rows.first.click()
            page.wait_for_selector("#inspector-drawer", state="visible", timeout=5000)
            insp_title = page.locator("#inspector-title").inner_text()
            print(f"  Inspector drawer opened for: {insp_title}")
            assert len(insp_title) > 0
            page.screenshot(path=f"{SCREENSHOTS_DIR}/{proj_name}_03_inspector.png")
            
            # Close inspector drawer
            page.locator("#inspector-close").click()
            time.sleep(0.3)
            assert page.locator("#inspector-drawer").is_hidden(), "Inspector drawer should be closed"

        # 4. Test Missed (Blindspots) Screen
        print("[Step 4] Testing Code Blindspots screen...")
        page.locator("button.nav[data-screen='missed']").click()
        page.wait_for_selector("#screen-missed", state="visible", timeout=10000)
        time.sleep(0.5)
        blindspot_kpis = page.locator("#screen-missed .kpis .kpi")
        print(f"  Blindspot KPI categories: {blindspot_kpis.count()}")
        page.screenshot(path=f"{SCREENSHOTS_DIR}/{proj_name}_04_missed.png")

        # 5. Test Plan Refactor Screen
        print("[Step 5] Testing Plan Refactor Studio screen...")
        page.locator("button.nav[data-screen='planchange']").click()
        page.wait_for_selector("#screen-planchange", state="visible", timeout=10000)
        time.sleep(0.3)

        # Type test symbol and generate blueprint
        sym = project_info["test_symbol"]
        print(f"  Synthesizing refactor blueprint for symbol: '{sym}'...")
        page.locator("#planchange-input").fill(sym)
        page.locator("#screen-planchange form button[type='submit']").click()
        time.sleep(1.0)
        page.screenshot(path=f"{SCREENSHOTS_DIR}/{proj_name}_05_planchange.png")

        # 6. Test Command Palette (Cmd+K)
        print("[Step 6] Testing Raycast/Linear-grade Command Palette...")
        # Trigger via keyboard shortcut Cmd+K or Ctrl+K
        page.keyboard.press("Meta+k")
        time.sleep(0.3)
        if not page.locator("#command-palette-backdrop").is_visible():
            # Fallback to click button
            page.locator("#palette-open-btn").click()
            time.sleep(0.3)
        assert page.locator("#command-palette-backdrop").is_visible(), "Command palette should be visible"
        
        # Type 'event' in palette
        print("  Filtering palette with 'event'...")
        page.locator("#palette-search-input").fill("event")
        time.sleep(0.3)
        items = page.locator(".palette-item")
        print(f"  Matching palette items: {items.count()}")
        assert items.count() > 0, "Should find at least 1 palette item for 'event'"
        page.screenshot(path=f"{SCREENSHOTS_DIR}/{proj_name}_06_palette.png")

        # Click the item to navigate
        items.first.click()
        time.sleep(0.3)
        assert page.locator("#command-palette-backdrop").is_hidden(), "Palette should close after selection"
        assert page.locator("#screen-events").is_visible(), "Should have navigated to events screen"

        # 7. Test Theme Toggle
        print("[Step 7] Testing Theme Toggle...")
        theme_btn = page.locator("#theme-toggle")
        initial_theme = page.evaluate("() => document.documentElement.dataset.theme")
        print(f"  Initial theme: {initial_theme}")
        theme_btn.click()
        time.sleep(0.2)
        new_theme = page.evaluate("() => document.documentElement.dataset.theme")
        print(f"  New theme after toggle: {new_theme}")
        assert initial_theme != new_theme, "Theme attribute should change on toggle"

        # 8. Test Graph Canvas Screen
        print("[Step 8] Testing Map of the Code (Graph Canvas)...")
        page.locator("button.nav[data-screen='graph']").click()
        page.wait_for_selector("#screen-graph", state="visible", timeout=10000)
        time.sleep(1.0)
        canvas = page.locator("#graph-canvas")
        assert canvas.is_visible(), "Graph canvas should be visible"
        page.screenshot(path=f"{SCREENSHOTS_DIR}/{proj_name}_07_graph.png")

        # 9. Test Database Tables Screen
        print("[Step 9] Testing Database Tables screen...")
        page.locator("button.nav[data-screen='tables']").click()
        page.wait_for_selector("#screen-tables", state="visible", timeout=10000)
        time.sleep(0.5)
        page.screenshot(path=f"{SCREENSHOTS_DIR}/{proj_name}_08_tables.png")

        # 10. Test Quick Sweep across remaining screens to ensure zero uncaught JS crashes
        screens_to_sweep = [
            'search', 'metrics', 'crossstack', 'blast', 'contract', 'verify',
            'ownership', 'diff', 'config', 'tests', 'governance', 'deps',
            'accuracy', 'agent', 'retrieval'
        ]
        print(f"[Step 10] Rapid sweep across {len(screens_to_sweep)} screens...")
        for s in screens_to_sweep:
            nav_btn = page.locator(f"button.nav[data-screen='{s}']")
            if nav_btn.count() > 0:
                nav_btn.click()
                time.sleep(0.2)
                assert page.locator(f"#screen-{s}").is_visible()

        browser.close()

    finally:
        # Terminate server
        proc.send_signal(signal.SIGINT)
        try:
            proc.wait(timeout=2.0)
        except subprocess.TimeoutExpired:
            proc.kill()

    print(f"\nResults for {proj_name}:")
    print(f"  Errors recorded: {len(errors)}")
    if errors:
        for e in errors:
            print(f"    - {e}")
    print(f"  Warnings recorded: {len(warnings)}")
    return len(errors) == 0, errors

def main():
    binary_path = "/Users/satyendra/Documents/AI-TOOLS/repocortex/bin/repocortex"
    if not os.path.exists(binary_path):
        print(f"Error: binary {binary_path} does not exist. Run go build first.")
        sys.exit(1)

    all_passed = True
    total_findings = {}

    with sync_playwright() as p:
        for idx, proj in enumerate(PROJECTS_TO_TEST):
            port = 7780 + idx
            passed, errs = run_project_e2e(p, binary_path, proj, port=port)
            if not passed:
                all_passed = False
                total_findings[proj["name"]] = errs

    print("\n=======================================================")
    print("PLAYWRIGHT E2E SUMMARY ACROSS ALL DEVWORKSPACE PROJECTS:")
    print("=======================================================")
    if all_passed:
        print("ALL DEVWORKSPACE PROJECTS PASSED PLAYWRIGHT E2E TESTS WITH 0 ERRORS! ✅")
        sys.exit(0)
    else:
        print("FINDINGS DETECTED THAT REQUIRE FIXING:")
        for k, v in total_findings.items():
            print(f"  Project {k}:")
            for item in v:
                print(f"    • {item}")
        sys.exit(1)

if __name__ == "__main__":
    main()
