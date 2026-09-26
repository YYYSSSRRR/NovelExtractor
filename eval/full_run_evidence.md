# 全量爬取那一轮的现场记录

> `crawl.log` 本身**没有入库**（82KB，每次运行都不同，已在 `.gitignore` 里）。
> 这个文件是把它里面能被命令复算的部分摘出来。要完全复核这些数字得重跑一轮
> 全量爬取——站点内容会变，重跑的数字不会与这里相同。README 6.5 里那张表的
> 「能否从本仓库复算」一列全是「否」，原因就在这里。
>
> 本轮跑的是完整种子表：**1067 个域、12183 页、16 分 30 秒**，自己打印了收尾块。

## 现场

```
$ ./bin/crawler -data /tmp/fullrun -seeds seeds/seeds.txt \
      -workers 32 -delay 1500ms -budget 30 -per-host 15 \
      -max-pages 40000 -trace /tmp/fullrun/requests.trace

$ head -2 crawl.log
seed=1067 resume-queue=0 workers=32 delay=1.5s budget/host=30 target/host=15
已入队 1067 个种子

$ sed -n '/=== 抓取结束 ===/,$p' crawl.log
=== 抓取结束 ===
用时        16m30s（12.3 pages/s）
产出页数    12183（累计含续爬 12183）
覆盖域名    1028（已收尾 1005）
其中正文页  5572
下载字节    1041.8 MB
抽取为空    4459
抓取失败    182
robots 拒绝 38
Crawl-delay 过长放弃 5
连续失败放弃的域 38
已够 15 篇而收手的域 280（省下 2995 次请求）
续爬跳过    0
正文链接密度 0.0121（越低越干净）
总页面字符  26977395，正文占比 0.37
```

日志 1101 行，进度行 99 行，跨度 `[08:32:05]` → `[08:48:25]`。进程是 `nohup`
起的，退出状态没留下来，所以**退出码没有观测到**；能观测到的是收尾块打印了，
而 `printFinal` 只在 `run()` 的正常返回路径上执行。

> **`-data` 不是 README 里写的 `data`。** `store.Open` 用 `O_APPEND` 打开
> `pages.jsonl`，跑在仓库的 `data/` 上会把 12183 行追加到已提交的 300 行交付
> 语料后面，而且启动时的 `truncateTornLine` 会先对它做一次破坏性截断。
> 所以全量跑在临时目录里，交付语料不动。

## 产物自查

```
$ wc -l pages.jsonl
12183

$ python3 - <<'PY'
import json
raw=open("pages.jsonl","rb").read()
lines=raw.split(b"\n")
if lines and lines[-1]==b"": lines.pop()
bad=0; rows=[]
for i,l in enumerate(lines,1):
    try: rows.append(json.loads(l))
    except Exception as e: bad+=1
print("行数",len(lines),"可解析",len(rows),"坏行",bad,"域名",len({r['host'] for r in rows}))
PY
行数 12183 可解析 12183 坏行 0 域名 1005
```

12183 行 / 0 坏行 / 34.4MB / 1005 个域（与收尾块的「已收尾 1005」一致；
「覆盖域名 1028」比它多 23 个，那 23 个域一条都没落盘）。

落盘完整性也间接过了：`checkPersistence` 在「处理过的页面」与「落盘的页面」
差 2% 以上时会打一行警告，本轮的日志里没有：

```
$ grep -n "落盘失败\|对不上\|警告" crawl.log
（无输出）
```

## 正文页 5572 与 5476

收尾块写 5572，按同一判据在产物上重算是 5476：

```
$ python3 - <<'PY'
import json
rows=[json.loads(l) for l in open("pages.jsonl",encoding="utf-8")]
a=[r for r in rows if (r.get("content_chars") or 0)>=300 and (r.get("content_link_rate") or 0)<=0.3]
print(len(a))
PY
5476
```

差的 96 页来自**判据算了两次**：5572 是抓取当时判的（`cmd/crawler/main.go:433`），
5476 是产物里的值。域名收尾时跑跨页模板过滤，剥掉模板后重算 `content_chars`
（`main.go:536`），有 96 页因此掉到 300 字以下。

## 三条收手路径

```
$ grep -c "放弃该域" crawl.log
43                      # 38 条连续失败 + 5 条 Crawl-delay 过长

$ grep "Crawl-delay" crawl.log
Crawl-delay 10m0s 超过上限 1m0s，放弃该域: 5sing.kugou.com
Crawl-delay 1h0m0s 超过上限 1m0s，放弃该域: www.sephora.cn
Crawl-delay 2m0s 超过上限 1m0s，放弃该域: www.17u.cn
Crawl-delay 2m0s 超过上限 1m0s，放弃该域: www.people.com.cn
Crawl-delay 2m0s 超过上限 1m0s，放弃该域: www.ly.com

$ grep "放弃该域" crawl.log | head -3
连续失败 2 次，放弃该域剩余 18 条: zfxxgk.ndrc.gov.cn
连续失败 2 次，放弃该域剩余 27 条: www.icbc.com.cn
连续失败 2 次，放弃该域剩余 27 条: bj.5i5j.com

$ grep -c "已够" crawl.log
1                       # 汇总那一行：280 个域、省下 2995 次请求
```

`www.sephora.cn` 声明 `Crawl-delay: 1h0m0s`。修复前 `SetHostDelay` 无条件接受，
调度器会睡在那个域的 `nextAt` 上。卡死那次的末行是 `queue=1 inflight=0`——
只剩一条 URL 然后二十二分钟不动，与「那条 URL 属于一个 nextAt 在一小时后的域」
对得上。**这是推断，不是证据**：日志不记剩余 URL 属于哪个域。能确证的是这个站
确实声明了一小时间隔，而修复前的代码确实会照做。

## 失败原因分布

```
$ grep -o '抓取失败.*' crawl.log | sed 's/.*: //' | sort | uniq -c | sort -rn
  55 http 404
  30 context deadline exceeded
  15 http 403
  15 connection reset by peer
  13 too many redirects
   6 http 503
   4 http 521
   4 EOF
   3 no such host
   3 http 550
   3 http 502
   3 TLS handshake timeout
   2 no Host in request URL
   2 http 518
   2 http 500
  ...（共 31 种，合计 182）
```

`grep -c '抓取失败'` 是 183，比 182 多 1，多的是收尾块自己那行 `抓取失败 182`。

## 礼貌性：每域间隔

```
$ wc -l < requests.trace
13881

$ python3 - <<'PY'
import datetime
prev={}; viol=0; gaps=[]; n=0
for line in open("requests.trace",encoding="utf-8"):
    ts,host,url=line.rstrip("\n").split("\t")
    t=datetime.datetime.fromisoformat(ts); n+=1
    if host in prev:
        g=(t-prev[host]).total_seconds(); gaps.append(g)
        if g < 1.5-1e-6: viol+=1
    prev[host]=t
print(n, len(prev), len(gaps), "%.3f"%min(gaps), viol)
PY
13881 1067 12814 1.500 0
```

13881 次请求、1067 个域、12814 对相邻间隔、最小 1.500 秒、违反 0 次。
其中 1516 次是 `robots.txt`（`grep -c '/robots.txt' requests.trace`），
也都在限速之内。

## 分阶段速率

```
$ python3 - <<'PY'
import re
rows=[]
for l in open("crawl.log",encoding="utf-8"):
    m=re.match(r"\[(\d\d):(\d\d):(\d\d)\].*?pages=(\d+).*?([\d.]+) pages/s", l)
    if m: rows.append((int(m.group(1))*3600+int(m.group(2))*60+int(m.group(3)), int(m.group(4)), float(m.group(5))))
t0=rows[0][0]
for t,p,_,ts in rows:
    if p>=12000: print("pages>=12000 于", ts, "第", t-t0, "秒"); break
PY
pages>=12000 于 08:36:15 第 250 秒
```

98.5% 的页面（12000 / 12183）在前 250 秒内落盘，速率 40.0 页/秒；
最后 10 分钟只新增 36 页，0.06 页/秒。峰值 47.7 页/秒出现在爆发期。

## 这一轮**没有**证明的事

- 没有证明抽取质量。6.4 的人工抽检用的是另一批页面，不是这一轮的产物。
- 没有证明「1.5% 的失败率」比修复前更好：这一轮 280 个域提前收手，少发的
  2995 次请求里本来就会有一部分失败，分母小了分子也跟着小。要比较可靠性得
  看同一批 URL 上的表现，两次爬取不满足这个条件。
- 没有覆盖续爬。这一轮 `resume-queue=0`、`续爬跳过 0`，是从空队列跑到底的；
  续爬那条路径的证据在 `eval/verify_small/` 与 `cmd/crawler/shutdown_test.go`。
