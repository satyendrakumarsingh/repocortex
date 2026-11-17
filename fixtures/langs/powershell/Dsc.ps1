#Requires -Version 5.1
using module .\Helpers.psm1

[DscResource()]
class FileResource : BaseResource {
    [DscProperty(Key)] [string] $Path
    [DscProperty()] [Ensure] $Ensure = [Ensure]::Present
    hidden [int] $Retries = 3
    static [string] $Default = 'x'

    [FileResource] Get() { return $this }
    [void] Set() { Write-Verbose 'set' }
    [bool] Test() { return $true }
    static [void] Reset([string] $p) { }
    hidden [void] Internal() { }
}

enum Ensure {
    Absent = 0
    Present = 1
}

[Flags()] enum Perm : int {
    Read = 1
    Write = 2
}

Configuration WebSite {
    param([string[]]$Nodes)
    Node $Nodes {
        WindowsFeature IIS { Name = 'Web-Server' }
    }
}

function Outer-Func {
    function Inner-Func { 'inner' }
    Inner-Func
}

function Get-Thing
{
    [CmdletBinding(SupportsShouldProcess = $true,
                   ConfirmImpact = 'Medium')]
    [OutputType([string])]
    param(
        [Parameter(Mandatory = $true,
                   ValueFromPipeline = $true)]
        [ValidateNotNullOrEmpty()]
        [string]$Name,

        [ValidateSet('a', 'b')]
        [string]$Mode = 'a'
    )
    begin { $x = 1 }
    process {
        Write-Output "Thing: $Name $(if ($Mode) { 'yes' } else { "no" })"
    }
    end { }
}

function Invoke-Long {
    Get-ChildItem `
        -Path . `
        -Recurse | Where-Object { $_.Name -like '*.ps1' }
}

function Test-Dyn {
    [CmdletBinding()]
    param()
    dynamicparam {
        $d = New-Object System.Management.Automation.RuntimeDefinedParameterDictionary
        $d
    }
}

Set-Alias -Name gt -Value Get-Thing
New-Alias ot Outer-Func

function Test-Last { 1 }
