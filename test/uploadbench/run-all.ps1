# run-all.ps1 —— 复现 docs/upload-load-test-report.md 里的全部实验（约 25 分钟）。
param([string]$Only = "*")
$ErrorActionPreference = "Stop"
$s = Join-Path $PSScriptRoot "run-scenario.ps1"

function Should($n) { return $n -like $Only }

# E1 单流文件大小扫描：定位每请求固定开销与读写放大
if (Should "E1*") {
  & $s -Label E1-size-1MB   -Limit 4 -BenchArgs "-scenario closed -c 1 -n 256 -size 1048576   -repo e1a -label E1-1MB"   | Out-Null
  & $s -Label E1-size-8MB   -Limit 4 -BenchArgs "-scenario closed -c 1 -n 64  -size 8388608   -repo e1b -label E1-8MB"   | Out-Null
  & $s -Label E1-size-64MB  -Limit 4 -BenchArgs "-scenario closed -c 1 -n 16  -size 67108864  -repo e1c -label E1-64MB"  | Out-Null
  & $s -Label E1-size-256MB -Limit 4 -BenchArgs "-scenario closed -c 1 -n 4   -size 268435456 -repo e1d -label E1-256MB" | Out-Null
}

# E2 并发梯度（limit 固定为默认值 4）：找吞吐拐点与 429 放大比
if (Should "E2*") {
  foreach ($c in 1,2,4,8,16,32,64) {
    $lbl = "E2-conc-{0:d2}" -f $c
    & $s -Label $lbl -Limit 4 -BenchArgs "-scenario closed -c $c -n 128 -size 8388608 -retry429 20ms -repo e2c$c -label $lbl" | Out-Null
  }
}

# E3 concurrentLimit 扫描（客户端并发固定 16）：判断该参数是不是正确的调节旋钮
if (Should "E3*") {
  foreach ($l in 1,2,4,8,16,32) {
    $lbl = "E3-limit-{0:d2}" -f $l
    & $s -Label $lbl -Limit $l -BenchArgs "-scenario closed -c 16 -n 128 -size 8388608 -retry429 20ms -repo e3l$l -label $lbl" | Out-Null
  }
}

# E4 数据集场景：大量小文件写进同一个 repo/revision，以及分仓对照组
if (Should "E4*") {
  & $s -Label E4c-dataset-n1000 -Limit 4 -BenchArgs "-scenario dataset -c 4 -n 1000 -size 65536 -repo ds1 -prefix data -label E4c-n1000" | Out-Null
  & $s -Label E4d-dataset-n2000 -Limit 4 -BenchArgs "-scenario dataset -c 4 -n 2000 -size 65536 -repo ds1 -prefix data -label E4d-n2000" | Out-Null
  & $s -Label E4a-dataset-1repo -Limit 4 -BenchArgs "-scenario dataset -c 4 -n 4000 -size 65536 -repo ds1 -prefix data -label E4a-1repo"  | Out-Null
  & $s -Label E4b-dataset-8repo -Limit 4 -BenchArgs "-scenario dataset -c 4 -n 4000 -size 65536 -shardRepos 8 -repo ds -prefix data -label E4b-8repo" | Out-Null
}

# E5 幂等快路径 + 同文件并发
if (Should "E5*") {
  & $s -Label E5a-idem-c1 -Limit 4 -BenchArgs "-scenario idem -c 1 -n 100 -size 8388608 -repo e5 -label E5a-idem-c1" | Out-Null
  & $s -Label E5b-idem-c4 -Limit 4 -BenchArgs "-scenario idem -c 4 -n 200 -size 8388608 -repo e5 -label E5b-idem-c4" | Out-Null
}

# E6 慢客户端占位：4 条 4KB/s 的连接能否打满全部并发槽
if (Should "E6*") {
  & $s -Label E6-slowloris -Limit 4 -BenchArgs "-scenario slowloris -holders 4 -holdRate 4096 -holdSize 8388608 -probeFor 25s -size 1048576 -repo e6 -label E6-slowloris" | Out-Null
}

# E7 blockSize 影响：块位图整表回写频率 vs 每槽内存
if (Should "E7*") {
  foreach ($bs in 1048576, 8388608, 67108864) {
    $lbl = "E7-bs-$($bs/1MB)MB"
    & $s -Label $lbl -Limit 4 -BlockSize $bs -BenchArgs "-scenario closed -c 4 -n 32 -size 67108864 -repo e7 -label $lbl" | Out-Null
  }
}

& (Join-Path $PSScriptRoot "summarize.ps1")
