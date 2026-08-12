# summarize.ps1 —— 把 runs/*/result.json 汇总成一张 CSV 表
param([string]$Filter = "*")
$Root = Join-Path $PSScriptRoot "runs"
if (-not (Test-Path $Root)) { Write-Error "还没有实验结果，先跑 run-all.ps1" }
Get-ChildItem -Path $Root -Directory -Filter $Filter | ForEach-Object {
  $f = Join-Path $_.FullName "result.json"
  if (-not (Test-Path $f)) { return }
  $r = Get-Content $f -Raw | ConvertFrom-Json
  $b = $r.bench
  $payloadMB = [math]::Round(($b.success * $b.fileSize)/1MB, 1)
  [pscustomobject]@{
    label = $r.label; limit = $r.limit; conc = $b.concurrency
    sizeMB = [math]::Round($b.fileSize/1MB, 3)
    ok = $b.success; r429 = $b.rejected429; c409 = $b.conflict409; err = $b.otherErr
    wallS = [math]::Round($b.wallSec, 2); MBps = [math]::Round($b.goodputMBps, 1)
    p50ms = [math]::Round($b.successLatencyMs.p50, 1)
    p95ms = [math]::Round($b.successLatencyMs.p95, 1)
    p99ms = [math]::Round($b.successLatencyMs.p99, 1)
    e2ep95ms = [math]::Round($b.e2eLatencyMs.p95, 1)
    payloadMB = $payloadMB; wrMB = $r.serverWriteMB; rdMB = $r.serverReadMB
    wrAmp = if ($payloadMB -gt 0) { [math]::Round($r.serverWriteMB/$payloadMB, 2) } else { 0 }
    rdAmp = if ($payloadMB -gt 0) { [math]::Round($r.serverReadMB/$payloadMB, 2) } else { 0 }
    cpuS = $r.serverCpuSec
    cpuMsPerMB = if ($payloadMB -gt 0) { [math]::Round($r.serverCpuSec/$payloadMB*1000, 2) } else { 0 }
    peakMB = $r.peakWorkingSetMB; diskMB = $r.reposOnDiskMB
  }
} | Sort-Object label | ConvertTo-Csv -NoTypeInformation | ForEach-Object { $_ -replace '"','' }
