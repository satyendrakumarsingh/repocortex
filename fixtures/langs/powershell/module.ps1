# Prompt and tab-expansion helpers of a shell module.
$script:GitStatus = $null
$script:ShowStatusColors = $true

function script:Get-AliasPattern($cmd) {
    $aliases = @($cmd) + @(Get-Alias | Where-Object { $_.Definition -eq $cmd } | Select-Object -Exp Name)
    "($($aliases -join '|'))"
}

function script:Get-GitDirectory {
    param([string]$Path = $PWD)
    $current = Get-Item $Path
    while ($current) {
        if (Test-Path (Join-Path $current.FullName '.git')) { return $current.FullName }
        $current = $current.Parent
    }
}

function global:Write-Prompt {
    param(
        [Parameter(Mandatory)] [string] $Object,
        [ConsoleColor] $ForegroundColor
    )
    Write-Host $Object -ForegroundColor $ForegroundColor -NoNewline
}

function private:Format-Count($n) { "[$n]" }

function Invoke-Update {
    [CmdletBinding()]
    param([switch]$Force)
    $items = @{
        Name = 'x'
        Tags = @('a', 'b')
    }
    switch -Regex ($items.Name) {
        '^x' { 'ex' }
        default { 'other' }
    }
}

filter script:Skip-Blank { if ($_.Length -gt 0) { $_ } }

workflow Sync-Servers {
    param([string[]]$Servers)
    foreach -parallel ($s in $Servers) { Test-Connection $s }
}

class Repo {
    [string] $Name
    [int] $Depth

    Repo([string]$name) { $this.Name = $name }

    [string] Describe() { return "$($this.Name):$($this.Depth)" }

    static [Repo] Open([string]$path) { return [Repo]::new($path) }
}

enum Level {
    Low
    High
}

function Test-After {
    "after"
}
