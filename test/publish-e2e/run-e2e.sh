#!/usr/bin/env bash
# 环 5 端到端验证：真实进程 + 通用 HTTP 客户端（curl），服务端跑在 online 模式（针对坑 1）。
set -uo pipefail

DL=http://127.0.0.1:18090
UP=http://127.0.0.1:18091
TOKEN=debug-upload-token
HDR="X-Dingo-Upload-Token: $TOKEN"
SRC=./src
OUT=./out
FAIL=0

pass() { echo "  PASS  $1"; }
fail() { echo "  FAIL  $1"; FAIL=$((FAIL+1)); }
check() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1 (got '$2', want '$3')"; fi; }

rm -rf "$SRC" "$OUT" ./tmpdir; mkdir -p "$SRC/subdir/nested" "$OUT" ./tmpdir
printf '{"model_type":"demo","hidden":768}' > "$SRC/config.json"
printf '# demo model\n\nself-trained.\n'   > "$SRC/README.md"
printf '{"version":"1.0"}'                 > "$SRC/subdir/tokenizer.json"
printf 'a\nb\nc\n'                         > "$SRC/subdir/nested/vocab.txt"
# 20MB，跨多个 8MB 块，用来验证分段下载与多块内容
head -c 20971520 /dev/urandom             > "$SRC/weights.bin"

FILES="config.json README.md subdir/tokenizer.json subdir/nested/vocab.txt weights.bin"

sha_of() { sha256sum "$1" | cut -d' ' -f1; }
size_of() { stat -c %s "$1"; }

echo "=== 1. 批量路径：暂缓生效上传 5 个文件（含多级子目录） ==="
ITEMS=""
for f in $FILES; do
  sha=$(sha_of "$SRC/$f"); size=$(size_of "$SRC/$f")
  code=$(curl -s -o ./tmpdir/up.json -w '%{http_code}' -X POST \
    -H "$HDR" --data-binary "@$SRC/$f" \
    "$UP/api/local-upload/models/dingo-local/batch-demo/main/$f?size=$size&sha256=$sha&defer=true")
  status=$(python -c "import json,sys;print(json.load(open('./tmpdir/up.json')).get('status',''))" 2>/dev/null)
  commit=$(python -c "import json,sys;print(json.load(open('./tmpdir/up.json')).get('commit',''))" 2>/dev/null)
  if [ "$code" = "201" ] && [ "$status" = "staged" ] && [ -z "$commit" ]; then
    pass "staged $f"
  else
    fail "staged $f (http $code status '$status' commit '$commit')"
  fi
  [ -n "$ITEMS" ] && ITEMS="$ITEMS,"
  ITEMS="$ITEMS{\"path\":\"$f\",\"sha256\":\"$sha\",\"size\":$size}"
done

echo "=== 2. 发布之前：仓库对下载侧必须完全不可见（在线模式） ==="
meta_code=$(curl -s -o ./tmpdir/meta.json -w '%{http_code}' "$DL/api/models/dingo-local/batch-demo/revision/main")
if [ "$meta_code" != "200" ]; then pass "metadata not served before publish (http $meta_code)"; else fail "metadata served before publish: $(cat ./tmpdir/meta.json)"; fi
file_code=$(curl -s -o /dev/null -w '%{http_code}' "$DL/models/dingo-local/batch-demo/resolve/main/config.json")
if [ "$file_code" != "200" ]; then pass "file not served before publish (http $file_code)"; else fail "file served before publish"; fi
commit_dirs=$(ls ./repos/api/models/dingo-local/batch-demo/revision 2>/dev/null | wc -l)
check "no metadata on disk before publish" "$commit_dirs" "0"

echo "=== 3. 一条 curl 完成发布 ==="
pub_code=$(curl -s -o ./tmpdir/pub.json -w '%{http_code}' -X POST -H "$HDR" \
  -H 'Content-Type: application/json' \
  -d "{\"files\":[$ITEMS]}" \
  "$UP/api/local-publish/models/dingo-local/batch-demo/main")
check "publish http status" "$pub_code" "201"
BATCH_COMMIT=$(python -c "import json;print(json.load(open('./tmpdir/pub.json'))['commit'])")
pub_status=$(python -c "import json;print(json.load(open('./tmpdir/pub.json'))['status'])")
pub_count=$(python -c "import json;print(json.load(open('./tmpdir/pub.json'))['fileCount'])")
check "publish status" "$pub_status" "published"
check "publish fileCount" "$pub_count" "5"
echo "  batch commit = $BATCH_COMMIT"
commit_dirs=$(ls -d ./repos/api/models/dingo-local/batch-demo/revision/*/ 2>/dev/null | grep -vc '/main/$')
check "publishing 5 files produced exactly 1 commit" "$commit_dirs" "1"

echo "=== 4. 顺序路径：同一组文件逐个即时生效上传到另一个仓库 ==="
for f in $FILES; do
  sha=$(sha_of "$SRC/$f"); size=$(size_of "$SRC/$f")
  code=$(curl -s -o ./tmpdir/up2.json -w '%{http_code}' -X POST \
    -H "$HDR" --data-binary "@$SRC/$f" \
    "$UP/api/local-upload/models/dingo-local/seq-demo/main/$f?size=$size&sha256=$sha")
  [ "$code" = "201" ] || fail "sequential upload $f (http $code)"
  SEQ_COMMIT=$(python -c "import json;print(json.load(open('./tmpdir/up2.json'))['commit'])")
done
echo "  sequential commit = $SEQ_COMMIT"
check "§9.1 等价性：两条路径的快照标识逐字符相同" "$BATCH_COMMIT" "$SEQ_COMMIT"
seq_commit_dirs=$(ls -d ./repos/api/models/dingo-local/seq-demo/revision/*/ 2>/dev/null | grep -vc '/main/$')
check "对照组：逐个上传 5 个文件产生 5 个快照标识" "$seq_commit_dirs" "5"

echo "=== 5. 发布之后：整仓元数据与逐文件内容（在线模式，针对坑 1） ==="
meta_code=$(curl -s -o ./tmpdir/meta.json -w '%{http_code}' "$DL/api/models/dingo-local/batch-demo/revision/main")
check "metadata http status" "$meta_code" "200"
meta_sha=$(python -c "import json;print(json.load(open('./tmpdir/meta.json'))['sha'])")
check "metadata sha == published commit" "$meta_sha" "$BATCH_COMMIT"
sib=$(python -c "import json;d=json.load(open('./tmpdir/meta.json'));print(','.join(sorted(s['rfilename'] for s in d['siblings'])))")
check "整仓文件清单一个不少" "$sib" "README.md,config.json,subdir/nested/vocab.txt,subdir/tokenizer.json,weights.bin"

for f in $FILES; do
  mkdir -p "$OUT/$(dirname "$f")"
  code=$(curl -s -o "$OUT/$f" -w '%{http_code}' "$DL/models/dingo-local/batch-demo/resolve/main/$f")
  if [ "$code" = "200" ] && cmp -s "$SRC/$f" "$OUT/$f"; then
    pass "下载内容逐字节一致: $f"
  else
    fail "下载内容不一致: $f (http $code)"
  fi
done

echo "=== 6. 分段下载（开头/中间/末尾） ==="
total=$(size_of "$SRC/weights.bin")
for range in "0-1023" "10485760-10486783" "$((total-1024))-$((total-1))"; do
  curl -s -H "Range: bytes=$range" -o ./tmpdir/part.bin -D ./tmpdir/part.hdr "$DL/models/dingo-local/batch-demo/resolve/main/weights.bin"
  start=${range%-*}; end=${range#*-}
  dd if="$SRC/weights.bin" of=./tmpdir/expect.bin bs=1 skip="$start" count=$((end-start+1)) status=none
  code=$(head -1 ./tmpdir/part.hdr | tr -d '\r' | cut -d' ' -f2)
  if [ "$code" = "206" ] && cmp -s ./tmpdir/expect.bin ./tmpdir/part.bin; then
    pass "range bytes=$range"
  else
    fail "range bytes=$range (http $code)"
  fi
done

echo "=== 7. 追加一批文件：合并语义，老文件一个不能少 ==="
printf 'extra payload one' > "$SRC/extra1.bin"
printf 'extra payload two' > "$SRC/subdir/extra2.bin"
ITEMS2=""
for f in extra1.bin subdir/extra2.bin; do
  sha=$(sha_of "$SRC/$f"); size=$(size_of "$SRC/$f")
  curl -s -o /dev/null -X POST -H "$HDR" --data-binary "@$SRC/$f" \
    "$UP/api/local-upload/models/dingo-local/batch-demo/main/$f?size=$size&sha256=$sha&defer=true"
  [ -n "$ITEMS2" ] && ITEMS2="$ITEMS2,"
  ITEMS2="$ITEMS2{\"path\":\"$f\",\"sha256\":\"$sha\",\"size\":$size}"
done
curl -s -o ./tmpdir/pub2.json -X POST -H "$HDR" -d "{\"files\":[$ITEMS2]}" \
  "$UP/api/local-publish/models/dingo-local/batch-demo/main"
APPEND_COMMIT=$(python -c "import json;print(json.load(open('./tmpdir/pub2.json'))['commit'])")
append_count=$(python -c "import json;print(json.load(open('./tmpdir/pub2.json'))['fileCount'])")
check "追加后清单文件总数" "$append_count" "7"
if [ "$APPEND_COMMIT" != "$BATCH_COMMIT" ]; then pass "追加产生了新的快照标识"; else fail "追加没有改变快照标识"; fi
curl -s -o ./tmpdir/meta2.json "$DL/api/models/dingo-local/batch-demo/revision/main"
sib2=$(python -c "import json;d=json.load(open('./tmpdir/meta2.json'));print(','.join(sorted(s['rfilename'] for s in d['siblings'])))")
check "追加后老文件仍在" "$sib2" "README.md,config.json,extra1.bin,subdir/extra2.bin,subdir/nested/vocab.txt,subdir/tokenizer.json,weights.bin"
code=$(curl -s -o ./tmpdir/old.bin -w '%{http_code}' "$DL/models/dingo-local/batch-demo/resolve/main/config.json")
if [ "$code" = "200" ] && cmp -s "$SRC/config.json" ./tmpdir/old.bin; then pass "追加后老文件内容仍正确"; else fail "追加后老文件内容异常"; fi

echo "=== 8. 覆盖：未声明覆盖必须整次拒绝，声明后客户端能拿到新内容 ==="
printf '{"model_type":"demo","hidden":1024}' > "$SRC/config.json"
sha=$(sha_of "$SRC/config.json"); size=$(size_of "$SRC/config.json")
curl -s -o /dev/null -X POST -H "$HDR" --data-binary "@$SRC/config.json" \
  "$UP/api/local-upload/models/dingo-local/batch-demo/main/config.json?size=$size&sha256=$sha&defer=true"
code=$(curl -s -o ./tmpdir/conflict.json -w '%{http_code}' -X POST -H "$HDR" \
  -d "{\"files\":[{\"path\":\"config.json\",\"sha256\":\"$sha\",\"size\":$size}]}" \
  "$UP/api/local-publish/models/dingo-local/batch-demo/main")
check "未声明覆盖时的响应码" "$code" "409"
ccode=$(python -c "import json;print(json.load(open('./tmpdir/conflict.json'))['code'])")
check "未声明覆盖时的错误码" "$ccode" "PUBLISH_OVERWRITE_REQUIRED"
curl -s -o ./tmpdir/still.json "$DL/api/models/dingo-local/batch-demo/revision/main"
still=$(python -c "import json;print(json.load(open('./tmpdir/still.json'))['sha'])")
check "被拒绝的发布没有改变快照标识" "$still" "$APPEND_COMMIT"

curl -s -o ./tmpdir/pub3.json -X POST -H "$HDR" \
  -d "{\"files\":[{\"path\":\"config.json\",\"sha256\":\"$sha\",\"size\":$size}]}" \
  "$UP/api/local-publish/models/dingo-local/batch-demo/main?overwrite=true"
OVER_COMMIT=$(python -c "import json;print(json.load(open('./tmpdir/pub3.json'))['commit'])")
if [ "$OVER_COMMIT" != "$APPEND_COMMIT" ]; then pass "覆盖产生了新的快照标识"; else fail "覆盖没有改变快照标识"; fi
code=$(curl -s -o ./tmpdir/new.bin -w '%{http_code}' "$DL/models/dingo-local/batch-demo/resolve/main/config.json")
if [ "$code" = "200" ] && cmp -s "$SRC/config.json" ./tmpdir/new.bin; then pass "覆盖后客户端拿到新内容"; else fail "覆盖后拿到的不是新内容"; fi

echo "=== 9. 发布前置校验：清单里有没传完的文件 ==="
ghost_sha=$(printf 'never uploaded' | sha256sum | cut -d' ' -f1)
code=$(curl -s -o ./tmpdir/ghost.json -w '%{http_code}' -X POST -H "$HDR" \
  -d "{\"files\":[{\"path\":\"ghost.bin\",\"sha256\":\"$ghost_sha\",\"size\":14}]}" \
  "$UP/api/local-publish/models/dingo-local/batch-demo/main")
check "缺内容时的响应码" "$code" "409"
gcode=$(python -c "import json;print(json.load(open('./tmpdir/ghost.json'))['code'])")
check "缺内容时的错误码" "$gcode" "PUBLISH_CONTENT_NOT_READY"
gmsg=$(python -c "import json;print(json.load(open('./tmpdir/ghost.json'))['error'])")
case "$gmsg" in *ghost.bin*) pass "错误信息指出了缺失路径";; *) fail "错误信息未指出缺失路径: $gmsg";; esac

echo "=== 10. 数据集类型 ==="
sha=$(sha_of "$SRC/README.md"); size=$(size_of "$SRC/README.md")
curl -s -o /dev/null -X POST -H "$HDR" --data-binary "@$SRC/README.md" \
  "$UP/api/local-upload/datasets/dingo-local/ds-demo/main/data/train.md?size=$size&sha256=$sha&defer=true"
curl -s -o ./tmpdir/pubds.json -X POST -H "$HDR" \
  -d "{\"files\":[{\"path\":\"data/train.md\",\"sha256\":\"$sha\",\"size\":$size}]}" \
  "$UP/api/local-publish/datasets/dingo-local/ds-demo/main"
DS_COMMIT=$(python -c "import json;print(json.load(open('./tmpdir/pubds.json')).get('commit',''))")
if [ -n "$DS_COMMIT" ]; then pass "数据集发布成功"; else fail "数据集发布失败: $(cat ./tmpdir/pubds.json)"; fi
code=$(curl -s -o ./tmpdir/ds.bin -w '%{http_code}' "$DL/datasets/dingo-local/ds-demo/resolve/main/data/train.md")
if [ "$code" = "200" ] && cmp -s "$SRC/README.md" ./tmpdir/ds.bin; then pass "数据集下载内容一致"; else fail "数据集下载失败 (http $code)"; fi

echo "=== 11. 回归：公开模型元数据仍走上游（不受本增量影响） ==="
code=$(curl -s -o /dev/null -w '%{http_code}' -m 25 "$DL/api/models/gpt2/revision/main")
if [ "$code" = "200" ]; then pass "公开模型元数据仍可获取 (http $code)"; else echo "  SKIP  公开模型回归（外网不可达，http $code）"; fi

echo
if [ "$FAIL" = "0" ]; then echo "全部通过"; else echo "失败项：$FAIL"; fi
exit "$FAIL"
