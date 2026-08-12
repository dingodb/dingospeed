// uploadbench 是针对 dingospeed 本地单文件上传接口 POST /api/local-upload/... 的压测客户端。
// 只依赖标准库，内容按种子确定性生成，不落磁盘，避免客户端磁盘 IO 干扰服务端指标。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------- 确定性内容生成 ----------

type genReader struct {
	state     uint64
	remaining int64
	buf       []byte
	off       int
	// bytesPerSec > 0 时按该速率限流，用于模拟慢客户端
	bytesPerSec int64
	started     time.Time
	sent        int64
	// stallAfter > 0 时写满该字节数后阻塞 stallFor
	stallAfter int64
	stallFor   time.Duration
	stalled    bool
}

func newGenReader(seed uint64, size int64) *genReader {
	if seed == 0 {
		seed = 0x9E3779B97F4A7C15
	}
	b := make([]byte, 64*1024)
	return &genReader{state: seed, remaining: size, buf: b, off: len(b)}
}

func (g *genReader) fill() {
	for i := 0; i+8 <= len(g.buf); i += 8 {
		x := g.state
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		g.state = x
		g.buf[i+0] = byte(x)
		g.buf[i+1] = byte(x >> 8)
		g.buf[i+2] = byte(x >> 16)
		g.buf[i+3] = byte(x >> 24)
		g.buf[i+4] = byte(x >> 32)
		g.buf[i+5] = byte(x >> 40)
		g.buf[i+6] = byte(x >> 48)
		g.buf[i+7] = byte(x >> 56)
	}
	g.off = 0
}

func (g *genReader) Read(p []byte) (int, error) {
	if g.remaining <= 0 {
		return 0, io.EOF
	}
	if g.started.IsZero() {
		g.started = time.Now()
	}
	if g.stallAfter > 0 && !g.stalled && g.sent >= g.stallAfter {
		g.stalled = true
		time.Sleep(g.stallFor)
	}
	if g.bytesPerSec > 0 {
		want := time.Duration(float64(g.sent)/float64(g.bytesPerSec)*float64(time.Second)) - time.Since(g.started)
		if want > 0 {
			time.Sleep(want)
		}
		// 限速时把单次读取切小，保证限流粒度
		if int64(len(p)) > g.bytesPerSec/8+1 {
			p = p[:g.bytesPerSec/8+1]
		}
	}
	if g.off >= len(g.buf) {
		g.fill()
	}
	n := copy(p, g.buf[g.off:])
	if int64(n) > g.remaining {
		n = int(g.remaining)
	}
	g.off += n
	g.remaining -= int64(n)
	g.sent += int64(n)
	return n, nil
}

var shaCache sync.Map // key: seed<<32|size -> hex

func contentSha(seed uint64, size int64) string {
	key := fmt.Sprintf("%d:%d", seed, size)
	if v, ok := shaCache.Load(key); ok {
		return v.(string)
	}
	h := sha256.New()
	if _, err := io.Copy(h, newGenReader(seed, size)); err != nil {
		panic(err)
	}
	s := hex.EncodeToString(h.Sum(nil))
	shaCache.Store(key, s)
	return s
}

// ---------- 单次请求 ----------

type sample struct {
	Worker    int     `json:"worker"`
	Seq       int     `json:"seq"`
	File      string  `json:"file"`
	Size      int64   `json:"size"`
	StartMs   float64 `json:"startMs"`
	LatencyMs float64 `json:"latencyMs"`
	Status    int     `json:"status"`
	Code      string  `json:"code"`
	UpStatus  string  `json:"upStatus"`
	Err       string  `json:"err"`
	// E2eMs 只在最终成功的样本上有值：从该文件第一次尝试到成功返回的总耗时（含 429 重试等待）。
	E2eMs    float64 `json:"e2eMs"`
	Attempts int     `json:"attempts"`
}

type target struct {
	base     string
	token    string
	repoType string
	org      string
	repo     string
	revision string
}

func (t target) upload(cli *http.Client, filePath string, seed uint64, size int64, overwrite bool, body io.Reader) (int, string, string, error) {
	u := fmt.Sprintf("%s/api/local-upload/%s/%s/%s/%s/%s?size=%d&sha256=%s",
		t.base, t.repoType, t.org, t.repo, t.revision, filePath, size, contentSha(seed, size))
	if overwrite {
		u += "&overwrite=true"
	}
	req, err := http.NewRequest(http.MethodPost, u, body)
	if err != nil {
		return 0, "", "", err
	}
	req.ContentLength = size
	req.Header.Set(uploadTokenHeader, t.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := cli.Do(req)
	if err != nil {
		return 0, "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	var parsed map[string]interface{}
	code, upStatus := "", ""
	if json.Unmarshal(raw, &parsed) == nil {
		if v, ok := parsed["code"].(string); ok {
			code = v
		}
		if v, ok := parsed["status"].(string); ok {
			upStatus = v
		}
	}
	return resp.StatusCode, code, upStatus, nil
}

const uploadTokenHeader = "X-Dingo-Upload-Token"

// retry429 > 0 时，客户端遇到 429 会退避后重试同一个文件，模拟真实上传工具的行为。
var retry429 time.Duration

// shardRepos > 1 时 dataset 场景把文件分散到多个仓库，作为清单增长的对照组。
var shardRepos int

func newClient() *http.Client {
	tr := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 512,
		MaxConnsPerHost:     0,
		DisableCompression:  true,
		WriteBufferSize:     256 * 1024,
		ReadBufferSize:      64 * 1024,
	}
	return &http.Client{Transport: tr, Timeout: 0}
}

// ---------- 统计 ----------

type summary struct {
	Scenario     string             `json:"scenario"`
	Label        string             `json:"label"`
	Concurrency  int                `json:"concurrency"`
	FileSize     int64              `json:"fileSize"`
	Requests     int                `json:"requests"`
	Success      int                `json:"success"`
	Rejected429  int                `json:"rejected429"`
	Conflict409  int                `json:"conflict409"`
	OtherErr     int                `json:"otherErr"`
	CodeCounts   map[string]int     `json:"codeCounts"`
	StatusCounts map[string]int     `json:"upStatusCounts"`
	WallSec      float64            `json:"wallSec"`
	GoodputMBps  float64            `json:"goodputMBps"`
	ReqPerSec    float64            `json:"reqPerSec"`
	SuccessLat   map[string]float64 `json:"successLatencyMs"`
	AllLat       map[string]float64 `json:"allLatencyMs"`
	E2eLat       map[string]float64 `json:"e2eLatencyMs"`
}

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func latStats(v []float64) map[string]float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	sum := 0.0
	for _, x := range s {
		sum += x
	}
	m := map[string]float64{}
	if len(s) == 0 {
		return m
	}
	m["min"] = s[0]
	m["avg"] = sum / float64(len(s))
	m["p50"] = pct(s, 50)
	m["p90"] = pct(s, 90)
	m["p95"] = pct(s, 95)
	m["p99"] = pct(s, 99)
	m["max"] = s[len(s)-1]
	return m
}

func summarize(scenario, label string, conc int, size int64, samples []sample, wall time.Duration) summary {
	s := summary{
		Scenario: scenario, Label: label, Concurrency: conc, FileSize: size,
		Requests: len(samples), CodeCounts: map[string]int{}, StatusCounts: map[string]int{},
		WallSec: wall.Seconds(),
	}
	var okLat, allLat, e2eLat []float64
	var okBytes int64
	for _, sm := range samples {
		allLat = append(allLat, sm.LatencyMs)
		if sm.E2eMs > 0 {
			e2eLat = append(e2eLat, sm.E2eMs)
		}
		switch {
		case sm.Err != "":
			s.OtherErr++
			s.CodeCounts["transport:"+sm.Err]++
		case sm.Status == 201:
			s.Success++
			okLat = append(okLat, sm.LatencyMs)
			okBytes += sm.Size
			if sm.UpStatus != "" {
				s.StatusCounts[sm.UpStatus]++
			}
		case sm.Status == 429:
			s.Rejected429++
			s.CodeCounts[sm.Code]++
		case sm.Status == 409:
			s.Conflict409++
			s.CodeCounts[sm.Code]++
		default:
			s.OtherErr++
			s.CodeCounts[fmt.Sprintf("%d:%s", sm.Status, sm.Code)]++
		}
	}
	if wall > 0 {
		s.GoodputMBps = float64(okBytes) / 1024 / 1024 / wall.Seconds()
		s.ReqPerSec = float64(len(samples)) / wall.Seconds()
	}
	s.SuccessLat = latStats(okLat)
	s.AllLat = latStats(allLat)
	s.E2eLat = latStats(e2eLat)
	return s
}

// ---------- 主流程 ----------

func main() {
	var (
		base     = flag.String("base", "http://127.0.0.1:8091", "上传服务地址")
		token    = flag.String("token", "bench-token", "上传 token")
		repoType = flag.String("repoType", "datasets", "models|datasets")
		org      = flag.String("org", "dingo-local", "命名空间")
		repo     = flag.String("repo", "bench", "仓库名")
		revision = flag.String("revision", "main", "版本")
		scenario = flag.String("scenario", "closed", "closed|dataset|slowloris|idem")
		label    = flag.String("label", "", "结果标签")
		conc     = flag.Int("c", 4, "并发数")
		total    = flag.Int("n", 32, "总请求数")
		size     = flag.Int64("size", 4<<20, "单文件字节数")
		prefix   = flag.String("prefix", "f", "文件名前缀")
		out      = flag.String("out", "", "逐请求样本 JSONL 输出路径")
		holders  = flag.Int("holders", 4, "slowloris 场景占位连接数")
		holdRate = flag.Int64("holdRate", 4096, "slowloris 每个占位连接的字节/秒")
		holdSize = flag.Int64("holdSize", 8<<20, "slowloris 占位文件字节数")
		probeFor = flag.Duration("probeFor", 20*time.Second, "slowloris 探测持续时间")
		warmup   = flag.Bool("warmup", true, "先发一次请求预热连接与目录")
		shards   = flag.Int("shardRepos", 1, "dataset 场景把文件散列到 N 个仓库，1 表示全部写同一个仓库")
		retry    = flag.Duration("retry429", 0, "closed 场景遇到 429 的退避重试间隔，0 表示不重试")
	)
	flag.Parse()
	retry429 = *retry
	shardRepos = *shards

	t := target{base: *base, token: *token, repoType: *repoType, org: *org, repo: *repo, revision: *revision}
	lbl := *label
	if lbl == "" {
		lbl = fmt.Sprintf("%s-c%d-n%d-%dB", *scenario, *conc, *total, *size)
	}

	var samples []sample
	var wall time.Duration

	switch *scenario {
	case "closed":
		samples, wall = runClosed(t, *conc, *total, *size, *prefix, *warmup, false)
	case "idem":
		samples, wall = runClosed(t, *conc, *total, *size, *prefix, *warmup, true)
	case "dataset":
		samples, wall = runDataset(t, *conc, *total, *size, *prefix)
	case "slowloris":
		samples, wall = runSlowloris(t, *holders, *holdRate, *holdSize, *probeFor, *size, *prefix)
	default:
		fmt.Fprintln(os.Stderr, "unknown scenario")
		os.Exit(2)
	}

	if *out != "" {
		f, err := os.Create(*out)
		if err == nil {
			enc := json.NewEncoder(f)
			for _, s := range samples {
				_ = enc.Encode(s)
			}
			f.Close()
		}
	}
	sum := summarize(*scenario, lbl, *conc, *size, samples, wall)
	b, _ := json.MarshalIndent(sum, "", "  ")
	fmt.Println(string(b))
}

// runClosed: 闭环压测，conc 个 worker 各自循环上传互不相同的文件，直到发出 total 个请求。
// idempotent=true 时所有请求上传同一个内容/路径，命中 already_exists 快路径。
func runClosed(t target, conc, total int, size int64, prefix string, warmup, idempotent bool) ([]sample, time.Duration) {
	cli := newClient()
	if warmup {
		g := newGenReader(1, 1024)
		_, _, _, _ = t.upload(cli, fmt.Sprintf("%s-warmup.bin", prefix), 1, 1024, true, g)
	}
	if idempotent {
		// 先把目标文件真正写一遍，后续请求才会命中幂等快路径
		g := newGenReader(7, size)
		_, _, _, _ = t.upload(cli, fmt.Sprintf("%s-idem.bin", prefix), 7, size, true, g)
	}

	var counter int64
	res := make([][]sample, conc)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			c := newClient()
			for {
				seq := int(atomic.AddInt64(&counter, 1)) - 1
				if seq >= total {
					return
				}
				name := fmt.Sprintf("%s-%06d.bin", prefix, seq)
				seed := uint64(seq)*2654435761 + 11
				if idempotent {
					name = fmt.Sprintf("%s-idem.bin", prefix)
					seed = 7
				}
				first := time.Now()
				for attempt := 1; ; attempt++ {
					g := newGenReader(seed, size)
					t0 := time.Now()
					st, code, upst, err := t.upload(c, name, seed, size, true, g)
					sm := sample{Worker: w, Seq: seq, File: name, Size: size,
						StartMs: t0.Sub(start).Seconds() * 1000, LatencyMs: time.Since(t0).Seconds() * 1000,
						Status: st, Code: code, UpStatus: upst, Attempts: attempt}
					if err != nil {
						sm.Err = classifyErr(err)
					}
					if st == 429 && retry429 > 0 && attempt < 5000 {
						res[w] = append(res[w], sm)
						time.Sleep(retry429)
						continue
					}
					sm.E2eMs = time.Since(first).Seconds() * 1000
					res[w] = append(res[w], sm)
					break
				}
			}
		}(w)
	}
	wg.Wait()
	wall := time.Since(start)
	var all []sample
	for _, r := range res {
		all = append(all, r...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Seq < all[j].Seq })
	return all, wall
}

// runDataset: 模拟"上传一个数据集"——把 total 个文件顺序写进同一个 repo/revision。
// 关键观察量是第 k 个文件的耗时随 k 的变化（清单/元数据重写代价）。
// 为了让 429 不污染观测，失败请求会自动重试。
func runDataset(t target, conc, total int, size int64, prefix string) ([]sample, time.Duration) {
	var counter int64
	res := make([][]sample, conc)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			c := newClient()
			for {
				seq := int(atomic.AddInt64(&counter, 1)) - 1
				if seq >= total {
					return
				}
				// 目录结构贴近真实数据集：每 100 个文件一个分片目录
				name := fmt.Sprintf("%s/shard-%03d/part-%06d.bin", prefix, seq/100, seq)
				seed := uint64(seq)*1099511628211 + 3
				// shardRepos > 1 时把同一批文件散列到多个仓库，用作"清单增长"这一变量的对照组
				ft := t
				if shardRepos > 1 {
					ft.repo = fmt.Sprintf("%s-r%02d", t.repo, seq%shardRepos)
				}
				var sm sample
				for attempt := 0; attempt < 50; attempt++ {
					g := newGenReader(seed, size)
					t0 := time.Now()
					st, code, upst, err := ft.upload(c, name, seed, size, true, g)
					sm = sample{Worker: w, Seq: seq, File: name, Size: size,
						StartMs: t0.Sub(start).Seconds() * 1000, LatencyMs: time.Since(t0).Seconds() * 1000,
						Status: st, Code: code, UpStatus: upst}
					if err != nil {
						sm.Err = classifyErr(err)
					}
					if st == 429 {
						time.Sleep(20 * time.Millisecond)
						continue
					}
					break
				}
				res[w] = append(res[w], sm)
			}
		}(w)
	}
	wg.Wait()
	wall := time.Since(start)
	var all []sample
	for _, r := range res {
		all = append(all, r...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Seq < all[j].Seq })
	return all, wall
}

// runSlowloris: holders 个极慢的上传连接占住并发槽位，同时用正常速度的探测请求
// 观察正常用户在这段时间内能否成功。
func runSlowloris(t target, holders int, rate, holdSize int64, probeFor time.Duration, probeSize int64, prefix string) ([]sample, time.Duration) {
	start := time.Now()
	stop := make(chan struct{})
	var hwg sync.WaitGroup
	for i := 0; i < holders; i++ {
		hwg.Add(1)
		go func(i int) {
			defer hwg.Done()
			c := newClient()
			g := newGenReader(uint64(9000+i), holdSize)
			g.bytesPerSec = rate
			_, _, _, _ = t.upload(c, fmt.Sprintf("%s-holder-%02d.bin", prefix, i), uint64(9000+i), holdSize, true, g)
		}(i)
	}
	// 给占位连接一点时间抢占槽位
	time.Sleep(2 * time.Second)

	var mu sync.Mutex
	var samples []sample
	var pwg sync.WaitGroup
	deadline := time.Now().Add(probeFor)
	for p := 0; p < 2; p++ {
		pwg.Add(1)
		go func(p int) {
			defer pwg.Done()
			c := newClient()
			seq := 0
			for time.Now().Before(deadline) {
				seed := uint64(p*100000+seq)*2246822519 + 5
				name := fmt.Sprintf("%s-probe-%d-%05d.bin", prefix, p, seq)
				g := newGenReader(seed, probeSize)
				t0 := time.Now()
				st, code, upst, err := t.upload(c, name, seed, probeSize, true, g)
				sm := sample{Worker: p, Seq: seq, File: name, Size: probeSize,
					StartMs: t0.Sub(start).Seconds() * 1000, LatencyMs: time.Since(t0).Seconds() * 1000,
					Status: st, Code: code, UpStatus: upst}
				if err != nil {
					sm.Err = classifyErr(err)
				}
				mu.Lock()
				samples = append(samples, sm)
				mu.Unlock()
				seq++
				time.Sleep(200 * time.Millisecond)
			}
		}(p)
	}
	pwg.Wait()
	close(stop)
	wall := time.Since(start)
	// 占位连接可能还要很久才结束，不等它们
	_ = stop
	return samples, wall
}

func classifyErr(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "connection refused"):
		return "conn_refused"
	case strings.Contains(s, "reset by peer"), strings.Contains(s, "forcibly closed"):
		return "conn_reset"
	case strings.Contains(s, "timeout"), strings.Contains(s, "deadline"):
		return "timeout"
	case strings.Contains(s, "EOF"):
		return "eof"
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}
