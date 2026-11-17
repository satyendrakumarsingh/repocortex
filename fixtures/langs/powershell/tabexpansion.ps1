using namespace System.Management.Automation

$script:expandedCommands = @{}
$script:gitCommandList = @{
    add = 'add'
    commit = 'commit'
}

$script:longGitParams = @{
    add = 'dry-run interactive patch edit force update all no-ignore-removal'
}

function script:gitCmdOperations($commands, $command, $filter) {
    $commands[$command].Trim() -split ' ' | Where-Object { $_ -like "$filter*" }
}

function script:gitAliases($prefix) {
    & git config --get-regexp ^alias\. 2>$null | ForEach-Object {
        if ($_ -match "^alias\.(?<alias>[^\s]+)\s+(?<expansion>.*)$") {
            $matches['alias']
        }
    }
}

function script:expandGitAlias($cmd, $rest) {
    $alias = & git config "alias.$cmd" 2>$null
    if ($alias) {
        $cmd = $alias -replace '^!', ''
        return "$cmd $rest"
    }
    return "$cmd $rest"
}

function script:gitRemotes($filter) {
    git remote | Where-Object { $_ -like "$filter*" }
}

function script:gitBranches($filter, $includeHEAD = $false, $prefix = '') {
    $branches = @(git branch --no-color | ForEach-Object { if ($_ -match "^\*?\s*(?<ref>.*)") { $Matches['ref'] } })
    $branches | Where-Object { $_ -ne '(no branch)' -and $_ -like "$filter*" } | ForEach-Object { $prefix + $_ }
}

function script:gitTags($filter, $prefix = '') {
    git tag | Where-Object { $_ -like "$filter*" } | ForEach-Object { $prefix + $_ }
}

$script:hereText = @'
function inside-here-string {
    "not a definition"
}
'@

function script:Get-TabExpansion {
    param($lastBlock)
    switch -regex ($lastBlock) {
        "^(?<cmd>$(Get-AliasPattern git)) (?<op>\S+)$" {
            gitCmdOperations $script:gitCommandList $matches['cmd'] $matches['op']
        }
        "^(?<cmd>$(Get-AliasPattern git)) .* (?<op>-\S*)$" {
            expandLongParams $script:longGitParams $matches['cmd'] $matches['op']
        }
    }
}

function TabExpansion($line, $lastWord) {
    $lastBlock = [regex]::Split($line, '[|;]')[-1].TrimStart()
    switch -regex ($lastBlock) {
        'git (.*)' { script:Get-TabExpansion $lastBlock }
        default { }
    }
}

Register-ArgumentCompleter -Native -CommandName git -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $text = $commandAst.ToString()
    TabExpansion $text $wordToComplete | ForEach-Object {
        [CompletionResult]::new($_, $_, 'ParameterValue', $_)
    }
}

function Enable-GitTabs {
    [CmdletBinding()]
    param([switch]$Quiet)
    if (-not $Quiet) { Write-Verbose 'enabled' }
}

if ($PSVersionTable.PSVersion.Major -ge 5) {
    function Get-Version5Feature { 'v5' }
}

function Export-Everything { Export-ModuleMember -Function * }
