package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestPowerShellResolution indexes a few scripts, a module, a class file and
// their Pester test, and checks every call against what dot-sourcing, importing
// and `using module` make it reach.
//
// What it is built to catch: `Write-Log` defined in lib/log.ps1, which the
// script dot-sources, and again in lib/legacy.ps1, which it does not; a
// function named like a cmdlet (`Get-Date`) in a file nobody sourced; a
// command written in another case; a module function reached by
// Import-Module and one that was not imported; a class reached by `using
// module`, its static method and its `$this` call; `Write-Host` and
// `Get-ChildItem`, which are the shell's; and the file under test, which a
// Pester test dot-sources through `$PSCommandPath`.
func TestPowerShellResolution(t *testing.T) {
	got := langEdges(t, powershellFixture())
	high := model.ConfidenceHigh
	wants := []want{
		{"scripts/deploy.ps1:Invoke-Deploy", "scripts/lib/log.ps1:Write-Log", high},
		{"scripts/deploy.ps1:Invoke-Deploy", "scripts/lib/net.ps1:Get-Remote", high},
		{"scripts/deploy.ps1:Invoke-Deploy", "scripts/modules/Billing.psm1:Format-Invoice", high},
		{"scripts/deploy.ps1:Invoke-Deploy", "scripts/deploy.ps1:Get-Plan", high},
		{"scripts/deploy.ps1:(top level)", "scripts/deploy.ps1:Invoke-Deploy", high},
		{"scripts/lib/net.ps1:Get-Remote", "scripts/lib/log.ps1:Write-Log", high},
		{"scripts/modules/Billing.psm1:Get-Total", "scripts/modules/Billing.psm1:Format-Invoice", high},
		// A command written in another case is the same command.
		{"scripts/deploy.ps1:Invoke-Lower", "scripts/lib/log.ps1:Write-Log", high},
		{"scripts/deploy.ps1:Invoke-Lower", "scripts/lib/net.ps1:Get-Remote", high},
		// Both spellings are defined and both files sourced: the call's own
		// spelling is the one it reaches.
		{"scripts/caser.ps1:Lower-Case", "scripts/lib/case_b.ps1:show-it", high},
		{"scripts/caser.ps1:Upper-Case", "scripts/lib/case_a.ps1:Show-It", high},
		// The class, by `using module`, and its members.
		{"scripts/deploy.ps1:Invoke-Deploy", "scripts/lib/Types.psm1:Account.Create", high},
		{"scripts/lib/Types.psm1:Account.Do", "scripts/lib/Types.psm1:Account.Save", high},
		// The status script dot-sources legacy.ps1 alone.
		{"scripts/status.ps1:Show-Status", "scripts/lib/legacy.ps1:Write-Log", high},
		// A Pester test dot-sources the file it tests.
		{"tests/deploy.Tests.ps1:(top level)", "tests/deploy.ps1:Invoke-Deploy", high},
	}
	forbid := []want{
		// legacy.ps1 is not sourced by deploy.ps1 or net.ps1.
		{Src: "scripts/deploy.ps1", Dst: "scripts/lib/legacy.ps1"},
		{Src: "scripts/lib/net.ps1", Dst: "scripts/lib/legacy.ps1"},
		// Get-Date is the cmdlet.
		{Src: "scripts/deploy.ps1", Dst: "legacy.ps1:Get-Date"},
		// The shell's commands, written in lower case, are still the shell's.
		{Src: "scripts/deploy.ps1:Invoke-Lower", Dst: "legacy.ps1"},
		// Each call is its own spelling's, not the other's.
		{Src: "scripts/caser.ps1:Lower-Case", Dst: "case_a.ps1"},
		{Src: "scripts/caser.ps1:Upper-Case", Dst: "case_b.ps1"},
		// Nothing imports Secret.
		{Src: "scripts/", Dst: "scripts/modules/Secret.psm1"},
	}
	// `[Other]::Create(1)` names a type the script never imported, and the one
	// type of that name in the repository is bound at low: a type may come into
	// scope by a route no import records (a dot-sourced file that defines it).
	known := want{Src: "scripts/deploy.ps1:Invoke-Deploy", Dst: "scripts/lib/Other.psm1:Other.Create", Min: model.ConfidenceLow}
	for _, e := range got {
		if e.Dst == known.Dst && e.Confidence != model.ConfidenceLow {
			t.Errorf("%s -> %s is %s, want low", e.Src, e.Dst, e.Confidence)
		}
	}
	scoreEdges(t, got, wants, forbid, nil, known)
}

func powershellFixture() map[string]string {
	return map[string]string{
		"scripts/deploy.ps1": `using module ./lib/Types.psm1

. "$PSScriptRoot\lib\log.ps1"
. ./lib/net.ps1
Import-Module "$PSScriptRoot\modules\Billing.psm1" -Force

function Get-Plan {
  param([string]$Name)
  return $Name
}

function Invoke-Deploy {
  param([string]$Target)
  Write-Log "start"
  $plan = Get-Plan -Name $Target
  Get-Remote -Url "https://example.com"
  Format-Invoice -Id 1
  Write-Host "deploying"
  Get-ChildItem -Path .
  Get-Date
  [Account]::Create(5)
  [Other]::Create(1)
  $thing.Save()
}

function Invoke-Lower {
  write-log "lower case"
  GET-REMOTE -Url "x"
  get-date
  get-childitem
}

Invoke-Deploy -Target prod
`,
		"scripts/lib/log.ps1": `function Write-Log {
  param([string]$Message)
  Write-Host $Message
}
`,
		"scripts/lib/net.ps1": `. "$PSScriptRoot\log.ps1"

function Get-Remote {
  param([string]$Url)
  Write-Log "fetching $Url"
  Invoke-WebRequest -Uri $Url
}
`,
		"scripts/lib/legacy.ps1": `function Write-Log {
  param([string]$Message)
  Write-Output $Message
}

function Get-Date {
  return 1
}

function Get-ChildItem {
  return @()
}
`,
		"scripts/caser.ps1": `. ./lib/case_a.ps1
. ./lib/case_b.ps1

function Lower-Case {
  show-it
}

function Upper-Case {
  Show-It
}
`,
		"scripts/lib/case_a.ps1": `function Show-It {
  return 1
}
`,
		"scripts/lib/case_b.ps1": `function show-it {
  return 2
}
`,
		"scripts/status.ps1": `. ./lib/legacy.ps1

function Show-Status {
  Write-Log "status"
}
`,
		"scripts/modules/Billing.psm1": `function Format-Invoice {
  param([int]$Id)
  return "inv-$Id"
}

function Get-Total {
  Format-Invoice -Id 2
}

Export-ModuleMember -Function Format-Invoice, Get-Total
`,
		"scripts/modules/Secret.psm1": `function Format-Invoice {
  param([int]$Id)
  return "secret"
}
`,
		"scripts/lib/Other.psm1": `class Other {
  [void] Save() {
    Write-Host "other"
  }

  static [Other] Create([int]$n) {
    return [Other]::new()
  }
}
`,
		"scripts/lib/Types.psm1": `class Account {
  [void] Save() {
    Write-Host "saved"
  }

  static [Account] Create([int]$n) {
    return [Account]::new()
  }

  [void] Do() {
    $this.Save()
  }
}
`,
		"tests/deploy.ps1": `function Invoke-Deploy {
  return 1
}
`,
		"tests/deploy.Tests.ps1": `BeforeAll {
  . $PSCommandPath.Replace('.Tests.ps1', '.ps1')
}

Describe 'Invoke-Deploy' {
  It 'runs' {
    Invoke-Deploy | Should -Be 1
  }
}
`,
	}
}
