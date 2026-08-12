# run-scenario.ps1 —— 拉起一个干净的 dingospeed 上传服务，跑一个压测场景，采集服务端资源计数，关停。
#
# 用法示例：
#   .\run-scenario.ps1 -Label E2-conc-16 -Limit 4 `
#       -BenchArgs "-scenario closed -c 16 -n 128 -size 8388608 -retry429 20ms -repo e2 -label E2-conc-16"
#
# 服务端固定跑在 18090/18091/16060，避开开发机上已有的 8090/8091/6060 实例。
param(
  [Parameter(Mandatory=$true)][string]$Label,
  [int]$Limit = 4,
  [long]$BlockSize = 8388608,
  [int]$CacheExpMin = 30,
  [int]$CacheCleanMin = 60,
  [string]$BenchArgs = "",
  [switch]$KeepRepos,
  [string]$ReposDir = "D:\dingospeed-bench\repos"
)

$ErrorActionPreference = "Stop"
$Here    = $PSScriptRoot
$Speed   = Resolve-Path (Join-Path $Here "..\..")
$Exe     = Join-Path $Speed "bin\dingospeed-test.exe"
$Bench   = Join-Path $Here "uploadbench.exe"
$RunDir  = Join-Path $Here "runs\$Label"

if (-not (Test-Path $Exe))   { Write-Error "缺少被测二进制，请先执行: go build -o bin/dingospeed-test.exe ./cmd" }
if (-not (Test-Path $Bench)) { Write-Error "缺少压测客户端，请先执行: go build -o test/uploadbench/uploadbench.exe ./test/uploadbench" }
New-Item -ItemType Directory -Force -Path $RunDir | Out-Null

# 1. 干净的 repos，保证每次实验的初始状态一致
if (-not $KeepRepos -and (Test-Path $ReposDir)) { Remove-Item -Recurse -Force $ReposDir }
New-Item -ItemType Directory -Force -Path $ReposDir | Out-Null

# 2. 生成本次实验的配置
$cfg = (Get-Content (Join-Path $Here "config-bench.tmpl.yaml") -Raw).
        Replace("__REPOS__", $ReposDir.Replace("\","/")).
        Replace("__LIMIT__", "$Limit").
        Replace("__BLOCKSIZE__", "$BlockSize").
        Replace("__CACHEEXP__", "$CacheExpMin").
        Replace("__CACHECLEAN__", "$CacheCleanMin")
$cfgPath = Join-Path $RunDir "config.yaml"
Set-Content -Path $cfgPath -Value $cfg -Encoding utf8

# 3. 启动服务
$srv = Start-Process -FilePath $Exe -ArgumentList "-config `"$cfgPath`"" -WorkingDirectory $Speed `
        -RedirectStandardOutput "$RunDir\server.out" -RedirectStandardError "$RunDir\server.err" `
        -PassThru -WindowStyle Hidden

$ready = $false
for ($i = 0; $i -lt 100; $i++) {
  Start-Sleep -Milliseconds 200
  try {
    if ((Test-NetConnection 127.0.0.1 -Port 18091 -WarningAction SilentlyContinue).TcpTestSucceeded) { $ready = $true; break }
  } catch {}
}
if (-not $ready) { Write-Error "上传服务未起来，见 $RunDir\server.err" }

# Win32_Process 的 *TransferCount 是进程累计的逻辑 IO 字节数，用来算读写放大。
function Get-ProcStats($procId) {
  $p  = Get-CimInstance Win32_Process -Filter "ProcessId=$procId"
  $ps = Get-Process -Id $procId
  [pscustomobject]@{
    ReadBytes = [long]$p.ReadTransferCount; WriteBytes = [long]$p.WriteTransferCount
    ReadOps   = [long]$p.ReadOperationCount; WriteOps  = [long]$p.WriteOperationCount
    CpuSec    = [double]$ps.CPU
  }
}

Start-Sleep -Milliseconds 500
$before = Get-ProcStats $srv.Id

# 4. 跑压测
$argList = @("-base", "http://127.0.0.1:18091", "-out", "$RunDir\samples.jsonl") +
           ($BenchArgs -split ' ' | Where-Object { $_ -ne "" })
$sw = [System.Diagnostics.Stopwatch]::StartNew()
$outJson = & $Bench @argList 2>"$RunDir\bench.err"
$sw.Stop()

$after  = Get-ProcStats $srv.Id
$peakWS = [math]::Round((Get-Process -Id $srv.Id).PeakWorkingSet64/1MB, 1)

# 5. 关停并统计落盘量
Stop-Process -Id $srv.Id -Force
Start-Sleep -Milliseconds 500
$onDisk = 0
if (Test-Path $ReposDir) { $onDisk = (Get-ChildItem -Recurse -File $ReposDir | Measure-Object Length -Sum).Sum }

$res = [pscustomobject]@{
  label = $Label; limit = $Limit; blockSize = $BlockSize; benchArgs = $BenchArgs
  wallSec          = [math]::Round($sw.Elapsed.TotalSeconds, 3)
  serverCpuSec     = [math]::Round($after.CpuSec - $before.CpuSec, 3)
  serverWriteMB    = [math]::Round(($after.WriteBytes - $before.WriteBytes)/1MB, 2)
  serverReadMB     = [math]::Round(($after.ReadBytes  - $before.ReadBytes)/1MB, 2)
  serverWriteOps   = $after.WriteOps - $before.WriteOps
  serverReadOps    = $after.ReadOps  - $before.ReadOps
  peakWorkingSetMB = $peakWS
  reposOnDiskMB    = [math]::Round($onDisk/1MB, 2)
  bench            = ($outJson -join "`n" | ConvertFrom-Json)
}
$res | ConvertTo-Json -Depth 8 | Set-Content (Join-Path $RunDir "result.json") -Encoding utf8
$res | ConvertTo-Json -Depth 8
